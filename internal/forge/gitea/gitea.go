// Package gitea implements forge.Forge against Gitea 1.27: REST for pull
// requests, statuses, branches, comments and files, and git plumbing in a bare
// cache clone for the two things REST cannot do — build the train commit and
// read a real tree id.
//
// The token is taken once, by the constructor, and reaches only the
// Authorization header and a git child's environment. It is never logged,
// never put in argv or a URL, and never interpolated into an error; a non-2xx
// response becomes a forge.StatusError, which carries no body.
//
// Governing: ADR-0032 (merge train), SPEC-0025 REQ-12, design.md "The Gitea
// implementation".
package gitea

// Gitea REST Client
//
// Paths are built one escaped segment at a time, because Gitea routes branch
// names and file paths with a wildcard: "train/12" must reach the server as
// two segments, not as "train%2F12".

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/stump-wtf/harness/internal/forge"
)

// requestTimeout bounds every REST call (#600).
const requestTimeout = 30 * time.Second

// pageSize is Gitea's default maximum page size.
const pageSize = 50

// maxBody caps how much of a response is read.
const maxBody = 32 << 20

// Options configures a Client.
type Options struct {
	// BaseURL is the forge's root, e.g. "https://gitea.stump.rocks".
	BaseURL string
	// Token is a Gitea access token for the identity the train acts as.
	Token string
	// CacheDir holds one bare clone per repo, for CreateTrainBranch and
	// TreeOf. Required for those two methods only.
	CacheDir string
	// GitBinary is the git executable. Default "git".
	GitBinary string
	// GitURL returns the clone URL for repo. Default BaseURL/owner/name.git.
	// Tests point it at a local bare repository.
	GitURL func(repo string) string
	// HTTPClient overrides the default client (which has a 30 s timeout).
	HTTPClient *http.Client
}

// Client is a forge.Forge for one Gitea instance.
type Client struct {
	api    string // BaseURL + "/api/v1"
	token  string
	http   *http.Client
	gitBin string
	gitURL func(repo string) string
	cache  string

	gitMu sync.Map // repo → *sync.Mutex, serialising git per clone

	idMu sync.Mutex
	id   identity // cached once read successfully
}

var _ forge.Forge = (*Client)(nil)

// New returns a Client. It makes no network call.
func New(opts Options) (*Client, error) {
	base := strings.TrimRight(opts.BaseURL, "/")
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("gitea: BaseURL must be an http(s) URL with a host")
	}
	if u.User != nil {
		return nil, errors.New("gitea: BaseURL must not carry credentials")
	}
	if opts.Token == "" {
		return nil, errors.New("gitea: token is empty")
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: requestTimeout}
	}
	gitBin := opts.GitBinary
	if gitBin == "" {
		gitBin = "git"
	}
	gitURL := opts.GitURL
	if gitURL == nil {
		gitURL = func(repo string) string { return base + "/" + repo + ".git" }
	}
	return &Client{
		api:    base + "/api/v1",
		token:  opts.Token,
		http:   hc,
		gitBin: gitBin,
		gitURL: gitURL,
		cache:  opts.CacheDir,
	}, nil
}

// repoPath returns "/repos/<owner>/<name>" with both segments escaped.
func repoPath(repo string) (string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("gitea: repo %q is not owner/name", repo)
	}
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name), nil
}

// escapeSegments escapes each "/"-separated segment of p on its own.
func escapeSegments(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return strings.Join(parts, "/")
}

// do performs one REST call. method names the Forge method for errors. A
// non-2xx status is a *forge.StatusError; the body is never read into it.
func (c *Client) do(ctx context.Context, method, repo, verb, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("gitea: %s on %s: encode request: %w", method, repo, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, verb, c.api+path, body)
	if err != nil {
		return fmt.Errorf("gitea: %s on %s: build request: %w", method, repo, err)
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		// A *url.Error names the URL, which never carries the token (it
		// travels in a header); still, strip it to the operation.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("gitea: %s on %s: %w", method, repo, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &forge.StatusError{Method: method, Status: resp.StatusCode, Repo: repo}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
		return fmt.Errorf("gitea: %s on %s: decode response: %w", method, repo, err)
	}
	return nil
}

// notFound maps a 404 StatusError to forge.ErrNotFound, keeping the rest.
func notFound(err error) error {
	var se *forge.StatusError
	if errors.As(err, &se) && se.Status == http.StatusNotFound {
		return fmt.Errorf("%w (%s)", forge.ErrNotFound, se.Error())
	}
	return err
}

type apiUser struct {
	Login string `json:"login"`
	Email string `json:"email"`
}

type apiPull struct {
	Number         int     `json:"number"`
	User           apiUser `json:"user"`
	Draft          bool    `json:"draft"`
	Mergeable      bool    `json:"mergeable"`
	MergeCommitSHA string  `json:"merge_commit_sha"`
	Head           struct {
		SHA string `json:"sha"`
	} `json:"head"`
}

type apiReview struct {
	User        apiUser   `json:"user"`
	State       string    `json:"state"`
	CommitID    string    `json:"commit_id"`
	SubmittedAt time.Time `json:"submitted_at"`
	Dismissed   bool      `json:"dismissed"`
}

type apiStatus struct {
	State      string `json:"state"`
	TotalCount int    `json:"total_count"`
}

// stateDismissed replaces the state of a dismissed review, so a dismissed
// approval can never qualify.
const stateDismissed = "DISMISSED"

// ListOpenPRs lists open pull requests with their reviews and head CI state.
func (c *Client) ListOpenPRs(ctx context.Context, repo string) ([]forge.PullRequest, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	var pulls []apiPull
	for page := 1; ; page++ {
		var batch []apiPull
		q := "?state=open&limit=" + strconv.Itoa(pageSize) + "&page=" + strconv.Itoa(page)
		if err := c.do(ctx, "ListOpenPRs", repo, http.MethodGet, rp+"/pulls"+q, nil, &batch); err != nil {
			return nil, err
		}
		pulls = append(pulls, batch...)
		if len(batch) < pageSize {
			break
		}
	}
	out := make([]forge.PullRequest, 0, len(pulls))
	for _, p := range pulls {
		pr := forge.PullRequest{
			Number:    p.Number,
			Author:    p.User.Login,
			HeadSHA:   p.Head.SHA,
			Draft:     p.Draft,
			Mergeable: p.Mergeable,
		}
		if pr.Reviews, err = c.reviews(ctx, repo, rp, p.Number); err != nil {
			return nil, err
		}
		if pr.CIState, err = c.CombinedStatus(ctx, repo, p.Head.SHA); err != nil {
			return nil, err
		}
		out = append(out, pr)
	}
	return out, nil
}

func (c *Client) reviews(ctx context.Context, repo, rp string, n int) ([]forge.Review, error) {
	var out []forge.Review
	for page := 1; ; page++ {
		var batch []apiReview
		q := "?limit=" + strconv.Itoa(pageSize) + "&page=" + strconv.Itoa(page)
		if err := c.do(ctx, "ListOpenPRs", repo, http.MethodGet, rp+"/pulls/"+strconv.Itoa(n)+"/reviews"+q, nil, &batch); err != nil {
			return nil, err
		}
		for _, r := range batch {
			state := r.State
			if r.Dismissed {
				state = stateDismissed
			}
			out = append(out, forge.Review{Author: r.User.Login, State: state, CommitID: r.CommitID, SubmittedAt: r.SubmittedAt})
		}
		if len(batch) < pageSize {
			return out, nil
		}
	}
}

// BranchHead returns the SHA branch points at.
func (c *Client) BranchHead(ctx context.Context, repo, branch string) (string, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return "", err
	}
	var b struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if err := c.do(ctx, "BranchHead", repo, http.MethodGet, rp+"/branches/"+escapeSegments(branch), nil, &b); err != nil {
		return "", notFound(err)
	}
	if b.Commit.ID == "" {
		return "", fmt.Errorf("gitea: BranchHead on %s: response has no commit id", repo)
	}
	return b.Commit.ID, nil
}

// DeleteBranch deletes name; a branch that is already gone is not an error.
func (c *Client) DeleteBranch(ctx context.Context, repo, name string) error {
	rp, err := repoPath(repo)
	if err != nil {
		return err
	}
	err = c.do(ctx, "DeleteBranch", repo, http.MethodDelete, rp+"/branches/"+escapeSegments(name), nil, nil)
	var se *forge.StatusError
	if errors.As(err, &se) && se.Status == http.StatusNotFound {
		return nil
	}
	return err
}

// CombinedStatus returns the combined state of sha; no statuses is "pending".
func (c *Client) CombinedStatus(ctx context.Context, repo, sha string) (string, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return "", err
	}
	var st apiStatus
	if err := c.do(ctx, "CombinedStatus", repo, http.MethodGet, rp+"/commits/"+url.PathEscape(sha)+"/status", nil, &st); err != nil {
		return "", err
	}
	if st.TotalCount == 0 || st.State == "" {
		return "pending", nil
	}
	return st.State, nil
}

// SquashMerge squash-merges pr with its head pinned, then reads back the
// commit the merge created.
func (c *Client) SquashMerge(ctx context.Context, repo string, pr int, headSHA, title, message string) (string, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return "", err
	}
	if headSHA == "" {
		return "", errors.New("gitea: SquashMerge needs the expected head SHA")
	}
	body := map[string]any{
		"Do":                "squash",
		"MergeTitleField":   title,
		"MergeMessageField": message,
		"head_commit_id":    headSHA,
	}
	n := strconv.Itoa(pr)
	if err := c.do(ctx, "SquashMerge", repo, http.MethodPost, rp+"/pulls/"+n+"/merge", body, nil); err != nil {
		return "", err
	}
	var p apiPull
	if err := c.do(ctx, "SquashMerge", repo, http.MethodGet, rp+"/pulls/"+n, nil, &p); err != nil {
		return "", err
	}
	if p.MergeCommitSHA == "" {
		return "", fmt.Errorf("gitea: SquashMerge on %s: pull %d reports no merge commit", repo, pr)
	}
	return p.MergeCommitSHA, nil
}

// Comment posts body on pr.
func (c *Client) Comment(ctx context.Context, repo string, pr int, body string) error {
	rp, err := repoPath(repo)
	if err != nil {
		return err
	}
	return c.do(ctx, "Comment", repo, http.MethodPost, rp+"/issues/"+strconv.Itoa(pr)+"/comments", map[string]string{"body": body}, nil)
}

// ListComments returns every comment body on pr, oldest first.
func (c *Client) ListComments(ctx context.Context, repo string, pr int) ([]string, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	var out []string
	for page := 1; ; page++ {
		var batch []struct {
			Body string `json:"body"`
		}
		q := "?limit=" + strconv.Itoa(pageSize) + "&page=" + strconv.Itoa(page)
		if err := c.do(ctx, "ListComments", repo, http.MethodGet, rp+"/issues/"+strconv.Itoa(pr)+"/comments"+q, nil, &batch); err != nil {
			return nil, err
		}
		for _, cm := range batch {
			out = append(out, cm.Body)
		}
		if len(batch) < pageSize {
			return out, nil
		}
	}
}

// FileContentAtRef reads path at ref. The ref is a query parameter because a
// branch name can contain "/".
func (c *Client) FileContentAtRef(ctx context.Context, repo, path, ref string) ([]byte, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.api+rp+"/raw/"+escapeSegments(strings.TrimPrefix(path, "/"))+"?ref="+url.QueryEscape(ref), nil)
	if err != nil {
		return nil, fmt.Errorf("gitea: FileContentAtRef on %s: build request: %w", repo, err)
	}
	req.Header.Set("Authorization", "token "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("gitea: FileContentAtRef on %s: %w", repo, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		return nil, notFound(&forge.StatusError{Method: "FileContentAtRef", Status: resp.StatusCode, Repo: repo})
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if err != nil {
		return nil, fmt.Errorf("gitea: FileContentAtRef on %s: read: %w", repo, err)
	}
	return b, nil
}

// whoami reads the token's user, for the train commit's identity. A success
// is cached; a failure is not, so a transient error does not stick.
func (c *Client) whoami(ctx context.Context) (identity, error) {
	c.idMu.Lock()
	defer c.idMu.Unlock()
	if c.id.name != "" {
		return c.id, nil
	}
	var u apiUser
	if err := c.do(ctx, "whoami", "", http.MethodGet, "/user", nil, &u); err != nil {
		return identity{}, err
	}
	if u.Login == "" {
		return identity{}, errors.New("gitea: /user returned no login")
	}
	email := u.Email
	if email == "" {
		email = u.Login + "@users.noreply.invalid"
	}
	c.id = identity{name: u.Login, email: email}
	return c.id, nil
}

// Login returns the login the token authenticates as, so the daemon can log
// who the train will act as.
func (c *Client) Login(ctx context.Context) (string, error) {
	id, err := c.whoami(ctx)
	return id.name, err
}

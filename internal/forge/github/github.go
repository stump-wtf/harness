// Package github implements forge.Forge against GitHub's REST API so the merge
// train can land pull requests in a repository whose gate is GitHub Actions.
//
// The train commit itself is built by the same content-addressed git plumbing
// the Gitea backend uses (internal/forge/gitcache); what differs is where its
// CI comes from and how the train branch reaches that gate. Actions runs on a
// pull_request or a push, never on the arrival of a commit, so a repository
// whose workflows the train must not edit is gated by a draft pull request the
// train opens for its own branch and closes again (ADR-0061).
//
// The token is taken once, by the constructor, and reaches only the
// Authorization header and a git child's environment. It is never logged,
// never put in argv or a URL, and never interpolated into an error; a non-2xx
// response becomes a forge.StatusError carrying only a bounded,
// credential-redacted body summary.
//
// Governing: ADR-0032 (merge train), ADR-0061 (GitHub forge backend),
// SPEC-0025 REQ-12.
package github

// GitHub REST Client
//
// Paths are built one escaped segment at a time, the way the Gitea client
// does, because branch names and file paths reach the router as several
// segments.

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
	"github.com/stump-wtf/harness/internal/forge/gitcache"
)

// requestTimeout bounds every REST call.
const requestTimeout = 30 * time.Second

// DefaultBaseURL is GitHub's API root.
const DefaultBaseURL = "https://api.github.com"

// apiVersion pins the REST version, so a GitHub-side change to a field the
// train reads cannot arrive silently mid-run.
const apiVersion = "2022-11-28"

// perPage is GitHub's maximum page size.
const perPage = 100

// maxBody caps how much of a response is read.
const maxBody = 32 << 20

// CI gate topologies (ADR-0061).
const (
	// GateDraftPR gates the train commit with a draft pull request from the
	// train branch to the base branch, which runs the repository's existing
	// pull_request workflows. It is the only topology that works where the
	// train may not edit .github/workflows/ — a fork, or any repo whose
	// workflows sync from upstream.
	GateDraftPR = "draft-pr"
	// GateTrainPush gates the train commit by pushing train/**, which runs
	// whatever push triggers the repository's workflows declare. It costs no
	// extra pull request, and requires the workflows to name train/**.
	GateTrainPush = "train-push"
)

// Options configures a Client.
type Options struct {
	// BaseURL is the API root. Default DefaultBaseURL.
	BaseURL string
	// Token is a GitHub token for the identity the train acts as.
	Token string
	// CacheDir holds one bare clone per repo, for CreateTrainBranch and
	// TreeOf. Required for those two methods only.
	CacheDir string
	// GitBinary is the git executable. Default "git".
	GitBinary string
	// GitURL returns the clone URL for repo. Default https://github.com/<repo>.git.
	GitURL func(repo string) string
	// CIGate is GateDraftPR (default) or GateTrainPush.
	CIGate string
	// HTTPClient overrides the default client (which has a 30 s timeout).
	HTTPClient *http.Client
}

// Client is a forge.Forge for one GitHub account or organization.
type Client struct {
	api   string
	token string
	http  *http.Client
	git   *gitcache.Cache
	gate  string
	trees treeCache

	idMu sync.Mutex
	id   identity
}

var _ forge.Forge = (*Client)(nil)

// New returns a Client. It makes no network call.
func New(opts Options) (*Client, error) {
	base := strings.TrimRight(opts.BaseURL, "/")
	if base == "" {
		base = DefaultBaseURL
	}
	u, err := url.Parse(base)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("github: BaseURL must be an http(s) URL with a host")
	}
	if u.User != nil {
		return nil, errors.New("github: BaseURL must not carry credentials")
	}
	if opts.Token == "" {
		return nil, errors.New("github: token is empty")
	}
	gate := opts.CIGate
	if gate == "" {
		gate = GateDraftPR
	}
	if gate != GateDraftPR && gate != GateTrainPush {
		return nil, fmt.Errorf("github: CIGate %q is not %q or %q", opts.CIGate, GateDraftPR, GateTrainPush)
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: requestTimeout}
	}
	gitURL := opts.GitURL
	if gitURL == nil {
		gitURL = func(repo string) string { return "https://github.com/" + repo + ".git" }
	}
	return &Client{
		api:   strings.TrimRight(base, "/"),
		token: opts.Token,
		http:  hc,
		gate:  gate,
		git: gitcache.New(gitcache.Options{
			Prefix:     "github",
			AuthHeader: basicAuth(opts.Token),
			CacheDir:   opts.CacheDir,
			GitBinary:  opts.GitBinary,
			GitURL:     gitURL,
		}),
	}, nil
}

// repoPath returns "/repos/<owner>/<name>" with both segments escaped.
func repoPath(repo string) (string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", fmt.Errorf("github: repo %q is not owner/name", repo)
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
// non-2xx status is a *forge.StatusError carrying a bounded, credential-redacted
// body summary (ADR-0049). GitHub answers a rate limit with 403 rather than
// 429, so that case is reported as 429: forge.Transient is what decides whether
// a tick retries, and a limit exhausted by another job on the same token is
// worth another tick.
func (c *Client) do(ctx context.Context, method, repo, verb, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return fmt.Errorf("github: %s on %s: encode request: %w", method, repo, err)
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, verb, c.api+path, body)
	if err != nil {
		return fmt.Errorf("github: %s on %s: build request: %w", method, repo, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("github: %s on %s: %w", method, repo, err)
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxBody))
		_ = resp.Body.Close()
	}()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, forge.BodyLimit))
		status := resp.StatusCode
		if status == http.StatusForbidden && isRateLimit(body) {
			status = http.StatusTooManyRequests
		}
		return &forge.StatusError{Method: method, Status: status, Repo: repo, Body: forge.SanitizeBody(c.token, body)}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxBody)).Decode(out); err != nil {
		return fmt.Errorf("github: %s on %s: decode response: %w", method, repo, err)
	}
	return nil
}

// isRateLimit reports whether a 403 body is GitHub's primary or secondary rate
// limit rather than a refusal with a reason in it.
func isRateLimit(body []byte) bool {
	s := strings.ToLower(string(body))
	return strings.Contains(s, "rate limit") || strings.Contains(s, "secondary limit")
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
	ID    int64  `json:"id"`
}

type apiPull struct {
	Number    int     `json:"number"`
	User      apiUser `json:"user"`
	Draft     bool    `json:"draft"`
	State     string  `json:"state"`
	Mergeable *bool   `json:"mergeable"`
	Title     string  `json:"title"`
	Head      struct {
		SHA  string `json:"sha"`
		Ref  string `json:"ref"`
		User struct {
			Login string `json:"login"`
		} `json:"user"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

type apiReview struct {
	User        apiUser   `json:"user"`
	State       string    `json:"state"`
	CommitID    string    `json:"commit_id"`
	SubmittedAt time.Time `json:"submitted_at"`
}

// reviewStates maps GitHub's review states onto the two the train reasons
// about. GitHub spells a change request CHANGES_REQUESTED where Gitea spells it
// REQUEST_CHANGES; the train's vocabulary is Gitea's, so the adapter maps.
var reviewStates = map[string]string{
	"CHANGES_REQUESTED": "REQUEST_CHANGES",
}

func reviewState(s string) string {
	if m, ok := reviewStates[s]; ok {
		return m
	}
	return s
}

// ListOpenPRs lists open pull requests with their reviews and head CI state.
func (c *Client) ListOpenPRs(ctx context.Context, repo string) ([]forge.PullRequest, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	var pulls []apiPull
	for page := 1; ; page++ {
		var batch []apiPull
		q := "?state=open&per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page)
		if err := c.do(ctx, "ListOpenPRs", repo, http.MethodGet, rp+"/pulls"+q, nil, &batch); err != nil {
			return nil, err
		}
		pulls = append(pulls, batch...)
		if len(batch) < perPage {
			break
		}
	}
	out := make([]forge.PullRequest, 0, len(pulls))
	for _, p := range pulls {
		pr := forge.PullRequest{
			Number:  p.Number,
			Author:  p.User.Login,
			HeadSHA: p.Head.SHA,
			BaseRef: p.Base.Ref,
			Draft:   p.Draft,
		}
		// GitHub reports mergeable as null while it computes it. One read of
		// the individual pull request settles most of them; one that is still
		// null is reported as not mergeable, which keeps the PR out of the
		// train until the next tick rather than letting it in on a guess.
		mergeable := p.Mergeable
		if mergeable == nil {
			mergeable, err = c.mergeable(ctx, repo, rp, p.Number)
			if err != nil {
				return nil, err
			}
		}
		pr.Mergeable = mergeable != nil && *mergeable
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

// mergeable re-reads one pull request's mergeable flag, which GitHub leaves
// null on the list endpoint while it computes it.
func (c *Client) mergeable(ctx context.Context, repo, rp string, n int) (*bool, error) {
	var p apiPull
	if err := c.do(ctx, "ListOpenPRs", repo, http.MethodGet, rp+"/pulls/"+strconv.Itoa(n), nil, &p); err != nil {
		return nil, err
	}
	return p.Mergeable, nil
}

func (c *Client) reviews(ctx context.Context, repo, rp string, n int) ([]forge.Review, error) {
	var out []forge.Review
	for page := 1; ; page++ {
		var batch []apiReview
		q := "?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page)
		if err := c.do(ctx, "ListOpenPRs", repo, http.MethodGet, rp+"/pulls/"+strconv.Itoa(n)+"/reviews"+q, nil, &batch); err != nil {
			return nil, err
		}
		for _, r := range batch {
			out = append(out, forge.Review{
				Author: r.User.Login, State: reviewState(r.State), CommitID: r.CommitID, SubmittedAt: r.SubmittedAt,
				// GitHub reports no officiality per review, so the train
				// reads silence, which never blocks a merge (ADR-0050);
				// the merge refusal is the backstop.
			})
		}
		if len(batch) < perPage {
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
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := c.do(ctx, "BranchHead", repo, http.MethodGet, rp+"/branches/"+escapeSegments(branch), nil, &b); err != nil {
		return "", notFound(err)
	}
	if b.Commit.SHA == "" {
		return "", fmt.Errorf("github: BranchHead on %s: response has no commit sha", repo)
	}
	return b.Commit.SHA, nil
}

// DeleteBranch deletes name and, with it, the draft gate PR the train opened
// for that branch when the repo is gated by pull request. A branch that is
// already gone is not an error; neither is a gate PR that was never opened.
func (c *Client) DeleteBranch(ctx context.Context, repo, name string) error {
	rp, err := repoPath(repo)
	if err != nil {
		return err
	}
	if c.gate == GateDraftPR && strings.HasPrefix(name, "train/") {
		if err := c.closeGatePR(ctx, repo, rp, name); err != nil {
			return err
		}
	}
	err = c.do(ctx, "DeleteBranch", repo, http.MethodDelete, rp+"/git/refs/heads/"+escapeSegments(name), nil, nil)
	var se *forge.StatusError
	if errors.As(err, &se) && se.Status == http.StatusNotFound {
		return nil
	}
	return err
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
		q := "?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page) + "&since=1970-01-01T00:00:00Z"
		if err := c.do(ctx, "ListComments", repo, http.MethodGet, rp+"/issues/"+strconv.Itoa(pr)+"/comments"+q, nil, &batch); err != nil {
			return nil, err
		}
		for _, cm := range batch {
			out = append(out, cm.Body)
		}
		if len(batch) < perPage {
			return out, nil
		}
	}
}

// SquashMerge squash-merges pr with its head pinned, and returns the SHA of
// the commit GitHub created on the base branch.
func (c *Client) SquashMerge(ctx context.Context, repo string, pr int, headSHA, title, message string) (string, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return "", err
	}
	if headSHA == "" {
		return "", errors.New("github: SquashMerge needs the expected head SHA")
	}
	body := map[string]any{"merge_method": "squash", "sha": headSHA}
	if title != "" {
		body["commit_title"] = title
	}
	if message != "" {
		body["commit_message"] = message
	}
	var out struct {
		SHA    string `json:"sha"`
		Merged bool   `json:"merged"`
	}
	n := strconv.Itoa(pr)
	if err := c.do(ctx, "SquashMerge", repo, http.MethodPut, rp+"/pulls/"+n+"/merge", body, &out); err != nil {
		return "", err
	}
	if out.SHA == "" {
		return "", fmt.Errorf("github: SquashMerge on %s: pull %d reported no merge commit", repo, pr)
	}
	return out.SHA, nil
}

// whoami reads the token's user, for the train commit's identity. A success is
// cached; a failure is not, so a transient error does not stick.
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
		return identity{}, errors.New("github: /user returned no login")
	}
	email := u.Email
	if email == "" {
		// GitHub's noreply address, which is the address that resolves back to
		// the account without revealing the private email.
		if u.ID != 0 {
			email = strconv.FormatInt(u.ID, 10) + "+" + u.Login + "@users.noreply.github.com"
		} else {
			email = u.Login + "@users.noreply.github.com"
		}
	}
	c.id = identity{name: u.Login, email: email}
	return c.id, nil
}

// Login returns the login the token authenticates as, so the daemon can log who
// the train will act as.
func (c *Client) Login(ctx context.Context) (string, error) {
	id, err := c.whoami(ctx)
	return id.name, err
}

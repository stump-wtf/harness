package github

// The Train Branch and Its Gate
//
// CreateTrainBranch is the shared git plumbing plus, in the draft-PR topology,
// the step that makes the gate run at all: GitHub runs a repository's
// pull_request workflows when a pull request exists, not when a commit
// appears, so a pushed train/<pr> branch would never be tested. The train
// therefore opens a draft pull request for its own branch against the base
// branch, which runs the same gate every other pull request runs — including
// the checks a repository declares only for pull requests, such as
// dependency-review — and DeleteBranch closes it again when the attempt ends
// (ADR-0061).
//
// The draft flag matters twice: a draft pull request runs CI but cannot itself
// be merged, and the train's own eligibility rules keep drafts out of the
// queue, so the gate PR can never be mistaken for work.
//
// Governing: ADR-0032, ADR-0061; SPEC-0025 REQ-4, REQ-6.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/stump-wtf/harness/internal/forge"
	"github.com/stump-wtf/harness/internal/forge/gitcache"
)

// identity is who the train commit is authored and committed by.
type identity struct{ name, email string }

// basicAuth is the header the cache clone authenticates with. GitHub accepts a
// token as the password of HTTP Basic auth; it never appears in a URL or in
// argv.
func basicAuth(token string) string {
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}

// CreateTrainBranch builds the train commit, pushes it to spec.Branch, and in
// the draft-PR topology opens the pull request that makes the gate run on it.
func (c *Client) CreateTrainBranch(ctx context.Context, repo string, spec forge.TrainSpec) (forge.TrainBranch, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return forge.TrainBranch{}, err
	}
	if c.gate == GateDraftPR && spec.BaseRef == "" {
		return forge.TrainBranch{}, errors.New("github: the draft-PR CI gate needs TrainSpec.BaseRef; configure the train's base branch")
	}
	id, err := c.whoami(ctx)
	if err != nil {
		return forge.TrainBranch{}, err
	}
	tb, err := c.git.CreateTrainBranch(ctx, repo, spec, gitcache.Identity{Name: id.name, Email: id.email})
	if err != nil {
		return forge.TrainBranch{}, err
	}
	if c.gate != GateDraftPR {
		return tb, nil
	}
	if _, err := c.openGatePR(ctx, repo, rp, spec); err != nil {
		// The branch exists; DeleteBranch is what removes it, and the driver's
		// cleanup runs on every path, so leaving the error to the caller is
		// enough to also clean the branch up.
		return forge.TrainBranch{}, err
	}
	return tb, nil
}

// TreeOf returns the git tree id of commit sha, fetching it if needed.
func (c *Client) TreeOf(ctx context.Context, repo, sha string) (string, error) {
	if _, err := repoPath(repo); err != nil {
		return "", err
	}
	return c.git.TreeOf(ctx, repo, sha)
}

// TreePaths lists the files under prefixes at commit sha, sorted (ADR-0063).
func (c *Client) TreePaths(ctx context.Context, repo, sha string, prefixes []string) ([]string, error) {
	if _, err := repoPath(repo); err != nil {
		return nil, err
	}
	return c.git.TreePaths(ctx, repo, sha, prefixes)
}

// openGatePR opens (or reuses) the draft pull request that gates spec.Branch.
func (c *Client) openGatePR(ctx context.Context, repo, rp string, spec forge.TrainSpec) (int, error) {
	num, err := c.gatePRNumber(ctx, repo, rp, spec.Branch)
	if err != nil {
		return 0, err
	}
	if num != 0 {
		return num, nil
	}
	body := map[string]any{
		"title": gateTitle(spec),
		"head":  spec.Branch,
		"base":  spec.BaseRef,
		"draft": true,
		"body":  gateBody(spec),
	}
	var out apiPull
	if err := c.do(ctx, "CreateTrainBranch", repo, http.MethodPost, rp+"/pulls", body, &out); err != nil {
		var se *forge.StatusError
		if errors.As(err, &se) && se.Status == http.StatusUnprocessableEntity {
			// A gate PR for this branch survived a crashed attempt; reuse it
			// rather than failing the attempt over a duplicate.
			if num, ferr := c.gatePRNumber(ctx, repo, rp, spec.Branch); ferr == nil && num != 0 {
				return num, nil
			}
		}
		return 0, err
	}
	return out.Number, nil
}

// closeGatePR closes the gate PR for branch, if one is open. Deleting the
// branch closes it anyway, but closing it first keeps a finished attempt from
// leaving an open pull request in the list even for the moment the delete takes.
func (c *Client) closeGatePR(ctx context.Context, repo, rp, branch string) error {
	num, err := c.gatePRNumber(ctx, repo, rp, branch)
	if err != nil {
		return err
	}
	if num == 0 {
		return nil
	}
	return c.do(ctx, "DeleteBranch", repo, http.MethodPatch, rp+"/pulls/"+strconv.Itoa(num),
		map[string]string{"state": "closed"}, nil)
}

// gatePRNumber finds the open pull request whose head is branch, or 0 when
// there is none.
func (c *Client) gatePRNumber(ctx context.Context, repo, rp, branch string) (int, error) {
	owner, _, ok := strings.Cut(repo, "/")
	if !ok {
		return 0, fmt.Errorf("github: repo %q is not owner/name", repo)
	}
	q := "?state=open&per_page=" + strconv.Itoa(perPage) + "&head=" + url.QueryEscape(owner+":"+branch)
	var pulls []apiPull
	if err := c.do(ctx, "CreateTrainBranch", repo, http.MethodGet, rp+"/pulls"+q, nil, &pulls); err != nil {
		var se *forge.StatusError
		if errors.As(err, &se) && se.Status == http.StatusNotFound {
			return 0, nil
		}
		return 0, err
	}
	for _, p := range pulls {
		if p.Head.Ref == branch && strings.EqualFold(p.Head.User.Login, owner) {
			return p.Number, nil
		}
	}
	return 0, nil
}

// gateTitle names the gate pull request so a reader of the PR list knows at a
// glance that it is the train's, not a change awaiting review.
func gateTitle(spec forge.TrainSpec) string {
	if spec.PR > 0 && len(spec.Heads) == 0 {
		return fmt.Sprintf("merge train gate: #%d", spec.PR)
	}
	nums := make([]string, 0, len(spec.Heads))
	if len(spec.Heads) == 0 {
		nums = append(nums, strconv.Itoa(spec.PR))
	}
	for _, h := range spec.Heads {
		nums = append(nums, strconv.Itoa(h.PR))
	}
	return "merge train gate: " + strings.Join(nums, ", ")
}

// gateBody says why the pull request exists and that nobody needs to review it.
func gateBody(spec forge.TrainSpec) string {
	var b strings.Builder
	b.WriteString("Merge train gate for `")
	b.WriteString(spec.Branch)
	b.WriteString("`.\n\n")
	b.WriteString("This pull request exists so that this commit runs the repository's pull request checks. ")
	b.WriteString("It is opened and closed by the merge train; **nothing here needs reviewing**, and the train ")
	b.WriteString("lands the pull requests it gates by their own merge, not through this one.\n")
	return b.String()
}

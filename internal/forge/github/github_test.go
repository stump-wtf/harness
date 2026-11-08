package github

// GitHub REST Tests
//
// Governing tests: ADR-0061 and SPEC-0025 REQ-5, REQ-12 — the gate reads check
// runs rather than trusting combined status, review states arrive in the
// train's vocabulary, SquashMerge pins the head, a rate limit is transient, and
// the token rides only in a header. No test contacts github.com.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stump-wtf/harness/internal/forge"
)

const (
	token = "ghp_2f7cNotARealToken_9b4d1e0a"
	repo  = "octo/widgets"
	sha   = "1111111111111111111111111111111111111111"
)

type recorded struct {
	Method, Path, Query, Auth, Accept string
	Body                              map[string]any
}

// server records every request and answers from routes, keyed "VERB /path". An
// unrouted request is a test failure: the backend asked for something nobody
// told it to ask for.
type server struct {
	t        *testing.T
	mu       sync.Mutex
	reqs     []recorded
	routes   map[string]func(w http.ResponseWriter, r *http.Request)
	prefixes map[string]func(w http.ResponseWriter, r *http.Request)
	base     string
}

func newServer(t *testing.T) (*server, *Client) {
	return newServerGate(t, "")
}

// newServerGate starts a faked GitHub and a Client pointed at it with the given
// CI gate.
func newServerGate(t *testing.T, ciGate string) (*server, *Client) {
	t.Helper()
	s := &server{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){},
		prefixes: map[string]func(http.ResponseWriter, *http.Request){}}
	ts := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(ts.Close)
	s.base = ts.URL
	c, err := New(Options{BaseURL: ts.URL, Token: token, CacheDir: t.TempDir(), CIGate: ciGate})
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

// prefix routes any request whose path starts with p, for endpoints whose URL
// contains a SHA the test only learns at run time.
func (s *server) prefix(p string, h func(w http.ResponseWriter, r *http.Request)) {
	s.prefixes[p] = h
}

func (s *server) serve(w http.ResponseWriter, r *http.Request) {
	rec := recorded{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery,
		Auth: r.Header.Get("Authorization"), Accept: r.Header.Get("Accept")}
	b, _ := io.ReadAll(r.Body)
	if len(b) > 0 {
		_ = json.Unmarshal(b, &rec.Body)
		r.Body = io.NopCloser(bytes.NewReader(b)) // handlers read the body too
	}
	s.mu.Lock()
	s.reqs = append(s.reqs, rec)
	h := s.routes[r.Method+" "+rec.Path]
	s.mu.Unlock()
	if h == nil {
		for p, ph := range s.prefixes {
			if strings.HasPrefix(r.Method+" "+rec.Path, p) {
				ph(w, r)
				return
			}
		}
		s.t.Errorf("unrouted request: %s %s?%s", r.Method, rec.Path, rec.Query)
		http.NotFound(w, r)
		return
	}
	h(w, r)
}

func (s *server) route(key string, status int, body any) {
	s.routes[key] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		switch b := body.(type) {
		case nil:
		case string:
			_, _ = io.WriteString(w, b)
		default:
			_ = json.NewEncoder(w).Encode(b)
		}
	}
}

// raw routes a path to fixed bytes, the way the contents endpoint answers the
// raw media type.
func (s *server) raw(key string, body string) {
	s.routes[key] = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.github.raw+json")
		_, _ = io.WriteString(w, body)
	}
}

func (s *server) calls(key string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.reqs {
		if r.Method+" "+r.Path == key {
			n++
		}
	}
	return n
}

func (s *server) last(key string) recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.reqs) - 1; i >= 0; i-- {
		if r := s.reqs[i]; r.Method+" "+r.Path == key {
			return r
		}
	}
	s.t.Fatalf("no request recorded for %s", key)
	return recorded{}
}

var ctx = context.Background()

// checkRoutes answers the two CI endpoints for sha.
func (s *server) checkRuns(status, conclusion string) {
	s.route("GET /repos/octo/widgets/commits/"+sha+"/check-runs", 200, map[string]any{
		"total_count": 1,
		"check_runs": []map[string]any{{
			"id": 1, "name": "test", "status": status, "conclusion": conclusion,
		}},
	})
	s.route("GET /repos/octo/widgets/commits/"+sha+"/status", 200, map[string]any{"state": "success", "total_count": 0})
}

func TestCombinedStatusReadsCheckRuns(t *testing.T) {
	cases := []struct {
		name, status, conclusion, want string
	}{
		{"running", "in_progress", "", "pending"},
		{"queued", "queued", "", "pending"},
		{"green", "completed", "success", "success"},
		{"red", "completed", "failure", "failure"},
		{"timed out", "completed", "timed_out", "failure"},
		{"needs action", "completed", "action_required", "failure"},
		{"skipped", "completed", "skipped", "success"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, c := newServer(t)
			// Combined status says "success" with nothing in it: the exact
			// trap issue #906 names. Only the check run can say otherwise.
			s.route("GET /repos/octo/widgets/commits/"+sha+"/status", 200,
				map[string]any{"state": "success", "total_count": 0, "statuses": []any{}})
			s.checkRuns(tc.status, tc.conclusion)
			got, err := c.CombinedStatus(ctx, repo, sha)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("CombinedStatus = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestCombinedStatusNoCIIsPending(t *testing.T) {
	s, c := newServer(t)
	s.route("GET /repos/octo/widgets/commits/"+sha+"/check-runs", 200, map[string]any{"total_count": 0, "check_runs": []any{}})
	s.route("GET /repos/octo/widgets/commits/"+sha+"/status", 200, map[string]any{"state": "success", "total_count": 0, "statuses": []any{}})
	if got, err := c.CombinedStatus(ctx, repo, sha); err != nil || got != "pending" {
		t.Fatalf("CombinedStatus = %q, %v; a commit nothing ran on is not green", got, err)
	}
}

func TestCombinedStatusUsesCommitStatuses(t *testing.T) {
	s, c := newServer(t)
	s.route("GET /repos/octo/widgets/commits/"+sha+"/check-runs", 200, map[string]any{"total_count": 0, "check_runs": []any{}})
	s.route("GET /repos/octo/widgets/commits/"+sha+"/status", 200, map[string]any{
		"state": "pending", "total_count": 1, "statuses": []map[string]any{{"state": "pending"}},
	})
	if got, err := c.CombinedStatus(ctx, repo, sha); err != nil || got != "pending" {
		t.Fatalf("CombinedStatus = %q, %v", got, err)
	}
}

func TestListOpenPRs(t *testing.T) {
	s, c := newServer(t)
	s.route("GET /repos/octo/widgets/pulls", 200, []map[string]any{{
		"number": 7, "draft": false, "mergeable": nil,
		"user": map[string]any{"login": "joestump"},
		"head": map[string]any{"sha": sha, "ref": "feat/x", "user": map[string]any{"login": "octo"}},
		"base": map[string]any{"ref": "main"},
	}})
	// mergeable is null while GitHub computes it; the individual read answers.
	s.route("GET /repos/octo/widgets/pulls/7", 200, map[string]any{"number": 7, "mergeable": true})
	s.route("GET /repos/octo/widgets/pulls/7/reviews", 200, []map[string]any{
		{"user": map[string]any{"login": "joestump-agent"}, "state": "APPROVED", "commit_id": sha},
		{"user": map[string]any{"login": "someone"}, "state": "CHANGES_REQUESTED", "commit_id": "2222"},
	})
	s.checkRuns("completed", "success")

	prs, err := c.ListOpenPRs(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 1 {
		t.Fatalf("len(prs) = %d", len(prs))
	}
	pr := prs[0]
	if !pr.Mergeable {
		t.Fatal("Mergeable = false; a null mergeable that resolves true must be read as mergeable")
	}
	if pr.CIState != "success" || pr.BaseRef != "main" || pr.HeadSHA != sha {
		t.Fatalf("pr = %+v", pr)
	}
	if pr.Reviews[0].State != "APPROVED" || pr.Reviews[1].State != "REQUEST_CHANGES" {
		t.Fatalf("reviews = %+v; GitHub's CHANGES_REQUESTED must reach the train as REQUEST_CHANGES", pr.Reviews)
	}
}

func TestSquashMergePinsHead(t *testing.T) {
	s, c := newServer(t)
	s.route("PUT /repos/octo/widgets/pulls/7/merge", 200, map[string]any{"merged": true, "sha": "3333"})
	got, err := c.SquashMerge(ctx, repo, 7, sha, "title", "body")
	if err != nil {
		t.Fatal(err)
	}
	if got != "3333" {
		t.Fatalf("merged sha = %q", got)
	}
	rec := s.last("PUT /repos/octo/widgets/pulls/7/merge")
	if rec.Body["merge_method"] != "squash" || rec.Body["sha"] != sha {
		t.Fatalf("merge body = %v", rec.Body)
	}
	if rec.Auth != "Bearer "+token {
		t.Fatalf("auth = %q", rec.Auth)
	}
}

func TestSquashMergeApprovalRefusal(t *testing.T) {
	s, c := newServer(t)
	s.route("PUT /repos/octo/widgets/pulls/7/merge", 405, map[string]any{
		"message": "Pull Request is not mergeable: this branch must have at least one approving review",
	})
	_, err := c.SquashMerge(ctx, repo, 7, sha, "", "")
	if err == nil {
		t.Fatal("merge succeeded")
	}
	if !forge.InsufficientApprovals(err) {
		t.Fatalf("InsufficientApprovals(%v) = false; the author needs the approval advice", err)
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("error leaks the token: %v", err)
	}
}

func TestRateLimitIsTransient(t *testing.T) {
	s, c := newServer(t)
	s.route("GET /repos/octo/widgets/commits/"+sha+"/check-runs", 403,
		map[string]any{"message": "API rate limit exceeded for user 1."})
	_, err := c.CombinedStatus(ctx, repo, sha)
	if err == nil {
		t.Fatal("no error")
	}
	if !forge.Transient(err) {
		t.Fatalf("Transient(%v) = false; a rate limit is worth another tick", err)
	}
}

func TestBranchHeadAndDelete(t *testing.T) {
	s, c := newServer(t)
	s.route("GET /repos/octo/widgets/branches/main", 200, map[string]any{"name": "main", "commit": map[string]any{"sha": sha}})
	s.route("GET /repos/octo/widgets/branches/nope", 404, map[string]any{"message": "Branch not found"})
	got, err := c.BranchHead(ctx, repo, "main")
	if err != nil || got != sha {
		t.Fatalf("BranchHead = %q, %v", got, err)
	}
	if _, err := c.BranchHead(ctx, repo, "nope"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("BranchHead(missing) = %v, want ErrNotFound", err)
	}
	s.route("GET /repos/octo/widgets/pulls", 200, []map[string]any{{
		"number": 99, "head": map[string]any{"ref": "train/1", "user": map[string]any{"login": "octo"}},
	}})
	s.route("PATCH /repos/octo/widgets/pulls/99", 200, map[string]any{"number": 99, "state": "closed"})
	s.route("DELETE /repos/octo/widgets/git/refs/heads/train/1", 204, nil)
	if err := c.DeleteBranch(ctx, repo, "train/1"); err != nil {
		t.Fatal(err)
	}
	if s.calls("PATCH /repos/octo/widgets/pulls/99") != 1 {
		t.Fatal("the gate PR was left open")
	}
}

func TestDeleteBranchGoneIsNotAnError(t *testing.T) {
	s, c := newServer(t)
	s.route("GET /repos/octo/widgets/pulls", 200, []any{})
	s.route("DELETE /repos/octo/widgets/git/refs/heads/train/1", 404, map[string]any{"message": "No git object with that name"})
	if err := c.DeleteBranch(ctx, repo, "train/1"); err != nil {
		t.Fatalf("DeleteBranch of a gone branch = %v", err)
	}
}

func TestFileContentAtRef(t *testing.T) {
	s, c := newServer(t)
	s.raw("GET /repos/octo/widgets/contents/docs/a.md", "# hi\n")
	s.route("GET /repos/octo/widgets/contents/docs/missing.md", 404, map[string]any{"message": "Not Found"})
	b, err := c.FileContentAtRef(ctx, repo, "docs/a.md", sha)
	if err != nil || string(b) != "# hi\n" {
		t.Fatalf("content = %q, %v", b, err)
	}
	if rec := s.last("GET /repos/octo/widgets/contents/docs/a.md"); !strings.HasPrefix(rec.Accept, "application/vnd.github.raw") {
		t.Fatalf("Accept = %q", rec.Accept)
	}
	if _, err := c.FileContentAtRef(ctx, repo, "docs/missing.md", sha); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("missing file = %v, want ErrNotFound", err)
	}
}

func TestFileContentAtRefLargeFileFallsBackToBlob(t *testing.T) {
	s, c := newServer(t)
	s.route("GET /repos/octo/widgets/contents/go.sum", 403,
		map[string]any{"message": "This repository file is too large to download using this endpoint."})
	s.route("GET /repos/octo/widgets/git/trees/"+sha, 200, map[string]any{
		"truncated": false,
		"tree": []map[string]any{
			{"path": "go.sum", "type": "blob", "sha": "bbb"},
			{"path": "docs", "type": "tree", "sha": "t1"},
		},
	})
	s.raw("GET /repos/octo/widgets/git/blobs/bbb", "line\n")
	b, err := c.FileContentAtRef(ctx, repo, "/go.sum", sha)
	if err != nil || string(b) != "line\n" {
		t.Fatalf("content = %q, %v", b, err)
	}
	// The second read reuses the tree listing rather than walking it again.
	b, err = c.FileContentAtRef(ctx, repo, "go.sum", sha)
	if err != nil || string(b) != "line\n" {
		t.Fatalf("content = %q, %v", b, err)
	}
	if n := s.calls("GET /repos/octo/widgets/git/trees/" + sha); n != 1 {
		t.Fatalf("tree listed %d times, want 1", n)
	}
}

func TestCommentAndListComments(t *testing.T) {
	s, c := newServer(t)
	s.route("POST /repos/octo/widgets/issues/7/comments", 201, map[string]any{"body": "hi"})
	if err := c.Comment(ctx, repo, 7, "hi"); err != nil {
		t.Fatal(err)
	}
	if s.last("POST /repos/octo/widgets/issues/7/comments").Body["body"] != "hi" {
		t.Fatal("comment body not sent")
	}
	s.route("GET /repos/octo/widgets/issues/7/comments", 200, []map[string]any{{"body": "one"}, {"body": "two"}})
	got, err := c.ListComments(ctx, repo, 7)
	if err != nil || strings.Join(got, "|") != "one|two" {
		t.Fatalf("comments = %q, %v", got, err)
	}
}

func TestLoginAndNoreplyEmail(t *testing.T) {
	s, c := newServer(t)
	s.route("GET /user", 200, map[string]any{"login": "joestump-agent", "id": 42, "email": nil})
	login, err := c.Login(ctx)
	if err != nil || login != "joestump-agent" {
		t.Fatalf("Login = %q, %v", login, err)
	}
	id, err := c.whoami(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if id.email != "42+joestump-agent@users.noreply.github.com" {
		t.Fatalf("email = %q; a train commit needs an address that resolves to the account", id.email)
	}
}

func TestNewRejectsBadOptions(t *testing.T) {
	if _, err := New(Options{BaseURL: "https://api.github.com", Token: ""}); err == nil {
		t.Fatal("empty token accepted")
	}
	if _, err := New(Options{BaseURL: "https://api.github.com", Token: token, CIGate: "yolo"}); err == nil {
		t.Fatal("unknown CI gate accepted")
	}
	if _, err := New(Options{BaseURL: "https://user:secret@api.github.com", Token: token}); err == nil {
		t.Fatal("credentials in BaseURL accepted")
	}
	c, err := New(Options{BaseURL: "", Token: token})
	if err != nil || c.api != DefaultBaseURL {
		t.Fatalf("default BaseURL = %q, %v", c.api, err)
	}
	if c.gate != GateDraftPR {
		t.Fatalf("default gate = %q, want draft-pr", c.gate)
	}
}

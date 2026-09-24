package gitea

// Gitea REST Tests
//
// Governing tests: #600 and SPEC-0025 REQ-12 — each method issues the
// documented verb and path; a 500 becomes an error with the status and no
// token; the token rides only in the Authorization header; SquashMerge sends
// "Do":"squash" with the head pinned. No test contacts a real host.

import (
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
	token = "tok-3f9a1c0de2b7-not-a-real-token"
	repo  = "stump.wtf/harness"
)

type recorded struct {
	Method, Path, Query, Auth string
	Body                      map[string]any
}

// server records every request and answers from routes, keyed "VERB /path".
// An unrouted request gets a 404.
type server struct {
	t      *testing.T
	mu     sync.Mutex
	reqs   []recorded
	routes map[string]func(w http.ResponseWriter, r *http.Request)
}

func newServer(t *testing.T) (*server, *Client) {
	s := &server{t: t, routes: map[string]func(http.ResponseWriter, *http.Request){}}
	ts := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(ts.Close)
	c, err := New(Options{BaseURL: ts.URL, Token: token, CacheDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	return s, c
}

func (s *server) serve(w http.ResponseWriter, r *http.Request) {
	rec := recorded{Method: r.Method, Path: r.URL.EscapedPath(), Query: r.URL.RawQuery, Auth: r.Header.Get("Authorization")}
	if b, _ := io.ReadAll(r.Body); len(b) > 0 {
		_ = json.Unmarshal(b, &rec.Body)
	}
	s.mu.Lock()
	s.reqs = append(s.reqs, rec)
	h := s.routes[r.Method+" "+rec.Path]
	s.mu.Unlock()
	if h == nil {
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

func (s *server) requests() []recorded {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]recorded(nil), s.reqs...)
}

func (s *server) last() recorded {
	r := s.requests()
	return r[len(r)-1]
}

var ctx = context.Background()

func TestEndpoints(t *testing.T) {
	s, c := newServer(t)
	s.route("GET /api/v1/repos/stump.wtf/harness/branches/main", 200, map[string]any{"commit": map[string]any{"id": "abc"}})
	s.route("GET /api/v1/repos/stump.wtf/harness/commits/abc/status", 200, map[string]any{"state": "success", "total_count": 3})
	s.route("DELETE /api/v1/repos/stump.wtf/harness/branches/train/12", 204, nil)
	s.route("POST /api/v1/repos/stump.wtf/harness/issues/12/comments", 201, map[string]any{"id": 1})
	s.route("GET /api/v1/repos/stump.wtf/harness/issues/12/comments", 200, []map[string]any{{"body": "a"}, {"body": "b"}})
	s.route("GET /api/v1/repos/stump.wtf/harness/raw/docs/a%20b.md", 200, "bytes")
	s.route("POST /api/v1/repos/stump.wtf/harness/pulls/12/merge", 200, nil)
	s.route("GET /api/v1/repos/stump.wtf/harness/pulls/12", 200, map[string]any{"number": 12, "merge_commit_sha": "m3rg3d"})

	cases := []struct {
		name  string
		call  func() error
		verb  string
		path  string
		query string
	}{
		{"BranchHead", func() error {
			sha, err := c.BranchHead(ctx, repo, "main")
			if err == nil && sha != "abc" {
				t.Errorf("BranchHead = %q", sha)
			}
			return err
		}, "GET", "/api/v1/repos/stump.wtf/harness/branches/main", ""},
		{"CombinedStatus", func() error {
			st, err := c.CombinedStatus(ctx, repo, "abc")
			if err == nil && st != "success" {
				t.Errorf("CombinedStatus = %q", st)
			}
			return err
		}, "GET", "/api/v1/repos/stump.wtf/harness/commits/abc/status", ""},
		{"DeleteBranch", func() error { return c.DeleteBranch(ctx, repo, "train/12") },
			"DELETE", "/api/v1/repos/stump.wtf/harness/branches/train/12", ""},
		{"Comment", func() error { return c.Comment(ctx, repo, 12, "hi") },
			"POST", "/api/v1/repos/stump.wtf/harness/issues/12/comments", ""},
		{"ListComments", func() error {
			got, err := c.ListComments(ctx, repo, 12)
			if err == nil && strings.Join(got, ",") != "a,b" {
				t.Errorf("ListComments = %q", got)
			}
			return err
		}, "GET", "/api/v1/repos/stump.wtf/harness/issues/12/comments", "limit=50&page=1"},
		{"FileContentAtRef", func() error {
			b, err := c.FileContentAtRef(ctx, repo, "docs/a b.md", "train/12")
			if err == nil && string(b) != "bytes" {
				t.Errorf("FileContentAtRef = %q", b)
			}
			return err
		}, "GET", "/api/v1/repos/stump.wtf/harness/raw/docs/a%20b.md", "ref=train%2F12"},
		{"SquashMerge", func() error {
			sha, err := c.SquashMerge(ctx, repo, 12, "h34d", "title", "msg")
			if err == nil && sha != "m3rg3d" {
				t.Errorf("SquashMerge = %q", sha)
			}
			return err
		}, "GET", "/api/v1/repos/stump.wtf/harness/pulls/12", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			got := s.last()
			if got.Method != tc.verb || got.Path != tc.path || got.Query != tc.query {
				t.Fatalf("request = %s %s?%s, want %s %s?%s", got.Method, got.Path, got.Query, tc.verb, tc.path, tc.query)
			}
			if got.Auth != "token "+token {
				t.Fatalf("Authorization = %q", got.Auth)
			}
		})
	}
}

func TestSquashMergeBody(t *testing.T) {
	s, c := newServer(t)
	s.route("POST /api/v1/repos/stump.wtf/harness/pulls/7/merge", 200, nil)
	s.route("GET /api/v1/repos/stump.wtf/harness/pulls/7", 200, map[string]any{"merge_commit_sha": "m"})
	if _, err := c.SquashMerge(ctx, repo, 7, "h34d", "the title", "the message"); err != nil {
		t.Fatal(err)
	}
	merge := s.requests()[0]
	if merge.Method != "POST" || merge.Path != "/api/v1/repos/stump.wtf/harness/pulls/7/merge" {
		t.Fatalf("first request = %s %s", merge.Method, merge.Path)
	}
	want := map[string]any{"Do": "squash", "MergeTitleField": "the title", "MergeMessageField": "the message", "head_commit_id": "h34d"}
	for k, v := range want {
		if merge.Body[k] != v {
			t.Errorf("body[%s] = %v, want %v", k, merge.Body[k], v)
		}
	}
	if _, err := c.SquashMerge(ctx, repo, 7, "", "t", "m"); err == nil {
		t.Fatal("SquashMerge without a head SHA succeeded")
	}
}

func TestSquashMergeWithoutMergeCommitFails(t *testing.T) {
	s, c := newServer(t)
	s.route("POST /api/v1/repos/stump.wtf/harness/pulls/7/merge", 200, nil)
	s.route("GET /api/v1/repos/stump.wtf/harness/pulls/7", 200, map[string]any{"merge_commit_sha": ""})
	if _, err := c.SquashMerge(ctx, repo, 7, "h", "t", "m"); err == nil {
		t.Fatal("a merge with no merge commit was reported as success")
	}
}

func TestListOpenPRs(t *testing.T) {
	s, c := newServer(t)
	page1 := make([]map[string]any, 0, pageSize)
	for n := 1; n <= pageSize; n++ {
		page1 = append(page1, map[string]any{"number": n, "user": map[string]any{"login": "a"}, "head": map[string]any{"sha": "h"}, "mergeable": true})
	}
	s.routes["GET /api/v1/repos/stump.wtf/harness/pulls"] = func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			_ = json.NewEncoder(w).Encode(page1)
		case "2":
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"number": 99, "draft": true, "mergeable": false,
				"user": map[string]any{"login": "joestump"}, "head": map[string]any{"sha": "h99"},
			}})
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
		}
	}
	s.routes["GET /api/v1/repos/stump.wtf/harness/commits/h/status"] = func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "success", "total_count": 2})
	}
	s.routes["GET /api/v1/repos/stump.wtf/harness/commits/h99/status"] = func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "pending", "total_count": 0})
	}
	s.routes["GET /api/v1/repos/stump.wtf/harness/pulls/99/reviews"] = func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"user": map[string]any{"login": "joestump-agent"}, "state": "APPROVED", "commit_id": "h99", "submitted_at": "2026-09-23T01:35:09-04:00"},
			{"user": map[string]any{"login": "rev"}, "state": "APPROVED", "commit_id": "h99", "submitted_at": "2026-09-23T01:36:00-04:00", "dismissed": true},
		})
	}
	for n := 1; n <= pageSize; n++ {
		s.route("GET /api/v1/repos/stump.wtf/harness/pulls/"+itoa(n)+"/reviews", 200, []any{})
	}

	prs, err := c.ListOpenPRs(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != pageSize+1 {
		t.Fatalf("got %d PRs, want %d (two pages)", len(prs), pageSize+1)
	}
	last := prs[len(prs)-1]
	if last.Number != 99 || !last.Draft || last.Mergeable || last.Author != "joestump" || last.HeadSHA != "h99" {
		t.Fatalf("PR 99 = %+v", last)
	}
	if last.CIState != "pending" {
		t.Fatalf("zero statuses CIState = %q, want pending", last.CIState)
	}
	if len(last.Reviews) != 2 || last.Reviews[0].State != "APPROVED" || last.Reviews[0].Author != "joestump-agent" ||
		last.Reviews[0].CommitID != "h99" || last.Reviews[0].SubmittedAt.IsZero() {
		t.Fatalf("reviews = %+v", last.Reviews)
	}
	if last.Reviews[1].State == "APPROVED" {
		t.Fatal("a dismissed approval kept state APPROVED")
	}
	if prs[0].CIState != "success" {
		t.Fatalf("PR 1 CIState = %q", prs[0].CIState)
	}
}

func TestErrorsCarryStatusNotTokenOrBody(t *testing.T) {
	s, c := newServer(t)
	// A body that echoes the credential, as some proxies do.
	leak := `{"message":"bad header: token ` + token + `"}`
	for _, key := range []string{
		"GET /api/v1/repos/stump.wtf/harness/branches/main",
		"GET /api/v1/repos/stump.wtf/harness/commits/x/status",
		"POST /api/v1/repos/stump.wtf/harness/pulls/1/merge",
		"POST /api/v1/repos/stump.wtf/harness/issues/1/comments",
		"GET /api/v1/repos/stump.wtf/harness/raw/f",
		"GET /api/v1/repos/stump.wtf/harness/pulls",
	} {
		s.route(key, 500, leak)
	}
	calls := map[string]func() error{
		"BranchHead":       func() error { _, err := c.BranchHead(ctx, repo, "main"); return err },
		"CombinedStatus":   func() error { _, err := c.CombinedStatus(ctx, repo, "x"); return err },
		"SquashMerge":      func() error { _, err := c.SquashMerge(ctx, repo, 1, "h", "t", "m"); return err },
		"Comment":          func() error { return c.Comment(ctx, repo, 1, "b") },
		"FileContentAtRef": func() error { _, err := c.FileContentAtRef(ctx, repo, "f", "main"); return err },
		"ListOpenPRs":      func() error { _, err := c.ListOpenPRs(ctx, repo); return err },
	}
	for name, call := range calls {
		err := call()
		if err == nil {
			t.Fatalf("%s: a 500 succeeded", name)
		}
		msg := err.Error()
		if !strings.Contains(msg, "500") || !strings.Contains(msg, name) || !strings.Contains(msg, repo) {
			t.Errorf("%s: error %q does not name method, status and repo", name, msg)
		}
		if strings.Contains(msg, token) || strings.Contains(msg, "bad header") {
			t.Errorf("%s: error leaks the token or the body: %q", name, msg)
		}
		if !forge.Transient(err) {
			t.Errorf("%s: a 500 is not Transient", name)
		}
	}
	for _, r := range s.requests() {
		if r.Auth != "token "+token {
			t.Fatalf("%s %s: Authorization = %q", r.Method, r.Path, r.Auth)
		}
		if strings.Contains(r.Path+"?"+r.Query, token) {
			t.Fatalf("token in URL: %s?%s", r.Path, r.Query)
		}
	}
}

func TestNotFoundMapping(t *testing.T) {
	_, c := newServer(t) // every route 404s
	if _, err := c.FileContentAtRef(ctx, repo, "missing", "main"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("FileContentAtRef 404 = %v, want ErrNotFound", err)
	}
	if _, err := c.BranchHead(ctx, repo, "gone"); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("BranchHead 404 = %v, want ErrNotFound", err)
	}
	if err := c.DeleteBranch(ctx, repo, "train/1"); err != nil {
		t.Fatalf("DeleteBranch 404 = %v, want nil (already gone)", err)
	}
}

func TestNewValidates(t *testing.T) {
	for _, o := range []Options{
		{BaseURL: "", Token: token},
		{BaseURL: "gitea.stump.rocks", Token: token},
		{BaseURL: "https://user:pw@gitea.example", Token: token},
		{BaseURL: "https://gitea.example", Token: ""},
	} {
		if _, err := New(o); err == nil {
			t.Errorf("New(%+v) succeeded", Options{BaseURL: o.BaseURL})
		} else if strings.Contains(err.Error(), token) || strings.Contains(err.Error(), "pw") {
			t.Errorf("New error leaks a credential: %v", err)
		}
	}
	c, err := New(Options{BaseURL: "https://gitea.example/", Token: token})
	if err != nil {
		t.Fatal(err)
	}
	if c.http.Timeout != requestTimeout {
		t.Fatalf("timeout = %v, want %v", c.http.Timeout, requestTimeout)
	}
}

func TestBadRepo(t *testing.T) {
	_, c := newServer(t)
	for _, r := range []string{"", "noslash", "a/b/c", "/b"} {
		if _, err := c.BranchHead(ctx, r, "main"); err == nil {
			t.Errorf("repo %q accepted", r)
		}
	}
}

func itoa(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}

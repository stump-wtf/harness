package github

// The Gate, End To End
//
// Governing tests: ADR-0061 and SPEC-0025 REQ-4, REQ-6 — a train commit pushed
// to train/<pr> gets a draft pull request so its gate runs, that pull request
// is closed when the attempt ends, and the whole attempt (build, wait, read the
// tested tree, clean up) runs against a local repository and a faked API.
// Nothing here reaches github.com.

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/forge"
	"github.com/stump-wtf/harness/internal/mergetrain"
)

// fixture is an origin repository with main and one PR head, the same shape the
// Gitea plumbing tests use.
type fixture struct {
	origin, base, main2, head string
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=f@x",
		"GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=f@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func gitLoose(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	work := filepath.Join(root, "work")
	var fx fixture
	fx.origin = filepath.Join(root, "origin.git")
	runGit(t, root, "init", "-q", "-b", "main", work)
	write(t, filepath.Join(work, "f.txt"), "a\nb\nc\n")
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-qm", "base")
	fx.base = runGit(t, work, "rev-parse", "HEAD")

	runGit(t, work, "checkout", "-qb", "pr")
	write(t, filepath.Join(work, "f.txt"), "a\nB\nc\n")
	write(t, filepath.Join(work, "new.txt"), "n\n")
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-qm", "pr")
	fx.head = runGit(t, work, "rev-parse", "HEAD")

	runGit(t, work, "checkout", "-q", "main")
	write(t, filepath.Join(work, "other.txt"), "z\n")
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-qm", "main2")
	fx.main2 = runGit(t, work, "rev-parse", "HEAD")

	runGit(t, root, "init", "-q", "--bare", fx.origin)
	runGit(t, work, "push", "-q", fx.origin, "main", fx.head+":refs/pull/1/head")
	return fx
}

// gate is a faked GitHub whose pull-request endpoints remember what the train
// asked for: the gate pull request it opened, and whether it was closed and its
// branch deleted.
type gate struct {
	s        *server
	mu       sync.Mutex
	openPR   bool
	openHead string
	created  []map[string]any
	closed   int
	deleted  int
}

func newGate(t *testing.T, fx fixture, ciGate string) (*gate, *Client) {
	t.Helper()
	s, c := newServerGate(t, ciGate)
	g := &gate{s: s}
	s.route("GET /user", 200, map[string]any{"login": "joestump-agent", "id": 7, "email": "agent@example.invalid"})
	s.routes["GET /repos/octo/widgets/pulls"] = func(w http.ResponseWriter, r *http.Request) {
		out := []any{}
		head := r.URL.Query().Get("head")
		g.mu.Lock()
		if g.openPR && strings.HasSuffix(head, ":"+g.openHead) {
			out = []any{map[string]any{"number": 99,
				"head": map[string]any{"ref": g.openHead, "user": map[string]any{"login": "octo"}}}}
		}
		g.mu.Unlock()
		_ = json.NewEncoder(w).Encode(out)
	}
	s.routes["POST /repos/octo/widgets/pulls"] = func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		g.mu.Lock()
		g.created = append(g.created, body)
		g.openPR = true
		g.openHead, _ = body["head"].(string)
		g.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 99, "state": "open", "draft": true})
	}
	s.routes["PATCH /repos/octo/widgets/pulls/99"] = func(w http.ResponseWriter, _ *http.Request) {
		g.mu.Lock()
		g.closed++
		g.openPR = false
		g.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 99, "state": "closed"})
	}
	s.routes["DELETE /repos/octo/widgets/git/refs/heads/train/1"] = func(w http.ResponseWriter, _ *http.Request) {
		// DeleteBranch is what removes the branch, so the fake removes it from
		// the origin the way GitHub would. The attempt also deletes any
		// leftover before building, so a missing ref is not an error.
		_, _ = gitLoose(fx.origin, "update-ref", "-d", "refs/heads/train/1")
		g.mu.Lock()
		g.deleted++
		g.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}
	c.git.GitURL = func(string) string { return fx.origin }
	return g, c
}

func (g *gate) createdBodies() []map[string]any {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]map[string]any(nil), g.created...)
}

func (g *gate) counts() (closed, deleted int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.closed, g.deleted
}

func TestCreateTrainBranchOpensDraftGatePR(t *testing.T) {
	fx := newFixture(t)
	g, c := newGate(t, fx, GateDraftPR)
	tb, err := c.CreateTrainBranch(ctx, repo, forge.TrainSpec{
		Branch: "train/1", BaseSHA: fx.main2, BaseRef: "main", PR: 1, HeadSHA: fx.head,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := runGit(t, fx.origin, "rev-parse", "refs/heads/train/1"); got != tb.SHA {
		t.Fatalf("train/1 = %s, want %s", got, tb.SHA)
	}
	created := g.createdBodies()
	if len(created) != 1 {
		t.Fatalf("gate pull requests opened = %d, want 1; Actions runs nothing without one", len(created))
	}
	if created[0]["draft"] != true {
		t.Fatalf("gate PR draft = %v; a mergeable gate PR could be landed by mistake", created[0]["draft"])
	}
	if created[0]["head"] != "train/1" || created[0]["base"] != "main" {
		t.Fatalf("gate PR head/base = %v/%v", created[0]["head"], created[0]["base"])
	}
}

func TestCreateTrainBranchTrainPushOpensNothing(t *testing.T) {
	fx := newFixture(t)
	g, c := newGate(t, fx, GateTrainPush)
	if _, err := c.CreateTrainBranch(ctx, repo, forge.TrainSpec{
		Branch: "train/1", BaseSHA: fx.main2, BaseRef: "main", PR: 1, HeadSHA: fx.head,
	}); err != nil {
		t.Fatal(err)
	}
	if len(g.createdBodies()) != 0 {
		t.Fatal("the train-push gate opened a pull request; it gates the push itself")
	}
}

func TestCreateTrainBranchNeedsBaseRefForDraftGate(t *testing.T) {
	fx := newFixture(t)
	_, c := newGate(t, fx, GateDraftPR)
	if _, err := c.CreateTrainBranch(ctx, repo, forge.TrainSpec{
		Branch: "train/1", BaseSHA: fx.main2, PR: 1, HeadSHA: fx.head,
	}); err == nil {
		t.Fatal("a draft-PR gate with no base branch was accepted; it would gate nothing")
	}
}

func TestRunTrainGatesWaitsAndCleansUp(t *testing.T) {
	fx := newFixture(t)
	g, c := newGate(t, fx, GateDraftPR)

	// The train commit's SHA is only known once it is built, so its CI
	// endpoints are routed by prefix: pending twice, then green.
	var polls atomic.Int32
	g.s.prefix("GET /repos/octo/widgets/commits/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/status") {
			_, _ = io.WriteString(w, `{"state":"","total_count":0}`)
			return
		}
		status, conclusion := "in_progress", ""
		if polls.Add(1) >= 2 {
			status, conclusion = "completed", "success"
		}
		_, _ = io.WriteString(w, `{"total_count":1,"check_runs":[{"id":1,"name":"test","status":"`+status+`","conclusion":"`+conclusion+`"}]}`)
	})
	g.s.raw("GET /repos/octo/widgets/contents/f.txt", "a\nB\nc\n")
	g.s.raw("GET /repos/octo/widgets/contents/new.txt", "n\n")

	pr := mergetrain.PullRequest{Number: 1, Author: "joestump", HeadSHA: fx.head, BaseRef: "main",
		CIState: "success", Mergeable: true}
	res := mergetrain.RunTrain(ctx, c, mergetrain.TrainConfig{
		Repo: repo, BaseBranch: "main",
		PollMin: 5 * time.Millisecond, PollMax: 10 * time.Millisecond,
		Timeout: 20 * time.Second, DeleteRetry: time.Millisecond,
	}, pr, fx.main2)
	if res.Outcome != mergetrain.OutcomeGreen {
		t.Fatalf("Outcome = %q (%v), want green", res.Outcome, res.Err)
	}
	if len(g.createdBodies()) != 1 {
		t.Fatalf("gate pull requests opened = %d, want 1", len(g.createdBodies()))
	}
	if closed, deleted := g.counts(); closed != 1 || deleted < 1 {
		t.Fatalf("gate PR closed = %d, branch deleted = %d; an attempt cleans up after itself", closed, deleted)
	}
	// rev-parse exits non-zero on a missing ref, which is the wanted answer.
	got, err := gitLoose(fx.origin, "rev-parse", "-q", "--verify", "refs/heads/train/1")
	if err == nil && got != "" {
		t.Fatalf("train/1 still exists on the remote: %s", got)
	}
	if string(res.Want["f.txt"]) != "a\nB\nc\n" || string(res.Want["new.txt"]) != "n\n" {
		t.Fatalf("Want = %v", res.Want)
	}
}

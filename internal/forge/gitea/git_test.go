package gitea

// Git Plumbing Tests
//
// Governing tests: SPEC-0025 REQ-4 (the train commit is base + a squash of the
// head, on a new branch, with conflicts and a moved head refused), REQ-8
// (TreeOf is git's real tree id), and REQ-12 (the token reaches git through
// its environment, never argv). The "remote" is a local bare repository, so
// nothing leaves the machine.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/forge"
)

// fixture is an origin repository with main and two PR heads:
//
//	base ── main2 (adds other.txt)                       ← main
//	  ├──── head  (f.txt b→B, adds exec n.txt, rm gone)   ← refs/pull/1/head
//	  └──── clash (f.txt b→C)                            ← refs/pull/2/head
type fixture struct {
	origin                   string
	base, main2, head, clash string
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=fixture", "GIT_AUTHOR_EMAIL=f@x", "GIT_COMMITTER_NAME=fixture", "GIT_COMMITTER_EMAIL=f@x")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func newFixture(t *testing.T) fixture {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}
	root := t.TempDir()
	work := filepath.Join(root, "work")
	var fx fixture
	fx.origin = filepath.Join(root, "origin.git")
	runGit(t, root, "init", "-q", "-b", "main", work)
	write(t, filepath.Join(work, "f.txt"), "a\nb\nc\n", 0o644)
	write(t, filepath.Join(work, "gone.txt"), "x", 0o644)
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-qm", "base")
	fx.base = runGit(t, work, "rev-parse", "HEAD")

	runGit(t, work, "checkout", "-qb", "pr")
	write(t, filepath.Join(work, "f.txt"), "a\nB\nc\n", 0o644)
	write(t, filepath.Join(work, "n.txt"), "#!/bin/sh\n", 0o755)
	runGit(t, work, "rm", "-q", "gone.txt")
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-qm", "pr")
	fx.head = runGit(t, work, "rev-parse", "HEAD")

	runGit(t, work, "checkout", "-qb", "clash", fx.base)
	write(t, filepath.Join(work, "f.txt"), "a\nC\nc\n", 0o644)
	runGit(t, work, "commit", "-qam", "clash")
	fx.clash = runGit(t, work, "rev-parse", "HEAD")

	runGit(t, work, "checkout", "-q", "main")
	write(t, filepath.Join(work, "other.txt"), "z\n", 0o644)
	runGit(t, work, "add", ".")
	runGit(t, work, "commit", "-qm", "main2")
	fx.main2 = runGit(t, work, "rev-parse", "HEAD")

	runGit(t, root, "init", "-q", "--bare", fx.origin)
	runGit(t, work, "push", "-q", fx.origin, "main", fx.head+":refs/pull/1/head", fx.clash+":refs/pull/2/head")
	return fx
}

// gitClient returns a Client whose REST side serves /user and whose git side
// talks to fx.origin, optionally through a wrapper binary.
func gitClient(t *testing.T, fx fixture, gitBin string) *Client {
	s, c := newServer(t)
	s.route("GET /api/v1/user", 200, map[string]any{"login": "joestump-agent", "email": "agent@example.invalid"})
	c.gitURL = func(string) string { return fx.origin }
	if gitBin != "" {
		c.gitBin = gitBin
	}
	return c
}

func TestCreateTrainBranch(t *testing.T) {
	fx := newFixture(t)
	c := gitClient(t, fx, "")
	spec := forge.TrainSpec{Branch: "train/1", BaseSHA: fx.main2, PR: 1, HeadSHA: fx.head, Message: "merge train: #1"}
	tb, err := c.CreateTrainBranch(ctx, repo, spec)
	if err != nil {
		t.Fatal(err)
	}

	// The branch exists on the remote and points at the train commit.
	if got := runGit(t, fx.origin, "rev-parse", "refs/heads/train/1"); got != tb.SHA {
		t.Fatalf("train/1 = %s, want %s", got, tb.SHA)
	}
	// One commit, parent = base, tree = what TrainBranch says.
	if parents := runGit(t, fx.origin, "rev-list", "--parents", "-n1", tb.SHA); parents != tb.SHA+" "+fx.main2 {
		t.Fatalf("parents = %q, want only %s", parents, fx.main2)
	}
	if tree := runGit(t, fx.origin, "rev-parse", tb.SHA+"^{tree}"); tree != tb.Tree {
		t.Fatalf("tree = %s, TrainBranch.Tree = %s", tree, tb.Tree)
	}
	// The tree is the three-way merge: main's other.txt, the PR's f.txt, the
	// PR's executable n.txt (mode kept), gone.txt removed.
	if got := runGit(t, fx.origin, "show", tb.SHA+":f.txt"); got != "a\nB\nc" {
		t.Fatalf("f.txt = %q", got)
	}
	if got := runGit(t, fx.origin, "show", tb.SHA+":other.txt"); got != "z" {
		t.Fatalf("other.txt = %q", got)
	}
	if ls := runGit(t, fx.origin, "ls-tree", tb.SHA, "n.txt"); !strings.HasPrefix(ls, "100755 ") {
		t.Fatalf("n.txt lost its mode: %q", ls)
	}
	if !slices.Equal(tb.Changed, []string{"f.txt", "n.txt"}) || !slices.Equal(tb.Deleted, []string{"gone.txt"}) {
		t.Fatalf("Changed = %q, Deleted = %q", tb.Changed, tb.Deleted)
	}
	// Authored as the token's user, and nobody's branch but train/1 moved.
	if who := runGit(t, fx.origin, "log", "-1", "--format=%an <%ae>|%cn", tb.SHA); who != "joestump-agent <agent@example.invalid>|joestump-agent" {
		t.Fatalf("identity = %q", who)
	}
	if main := runGit(t, fx.origin, "rev-parse", "refs/heads/main"); main != fx.main2 {
		t.Fatalf("main moved to %s", main)
	}
	if pr := runGit(t, fx.origin, "rev-parse", "refs/pull/1/head"); pr != fx.head {
		t.Fatalf("PR head moved to %s", pr)
	}

	// TreeOf answers with git's real tree id — for the train commit, and for a
	// commit the cache has never seen.
	if tree, err := c.TreeOf(ctx, repo, tb.SHA); err != nil || tree != tb.Tree {
		t.Fatalf("TreeOf(train) = %q, %v; want %s", tree, err, tb.Tree)
	}
	fresh := gitClient(t, fx, "")
	want := runGit(t, fx.origin, "rev-parse", fx.main2+"^{tree}")
	if tree, err := fresh.TreeOf(ctx, repo, fx.main2); err != nil || tree != want {
		t.Fatalf("TreeOf(main) on a cold cache = %q, %v; want %s", tree, err, want)
	}
	if want == fx.main2 {
		t.Fatal("fixture broken: tree id equals commit id")
	}
}

func TestCreateTrainBranchConflict(t *testing.T) {
	fx := newFixture(t)
	c := gitClient(t, fx, "")
	// Land PR 1 on main by hand, so PR 2's edit of the same line conflicts.
	landed := runGit(t, fx.origin, "rev-parse", fx.head)
	spec := forge.TrainSpec{Branch: "train/2", BaseSHA: landed, PR: 2, HeadSHA: fx.clash}
	runGit(t, fx.origin, "update-ref", "refs/heads/main", landed)
	_, err := c.CreateTrainBranch(ctx, repo, spec)
	if !errors.Is(err, forge.ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	if out := runGit(t, fx.origin, "for-each-ref", "refs/heads/train/"); out != "" {
		t.Fatalf("a conflicting build left a branch: %s", out)
	}
}

func TestCreateTrainBranchStaleHead(t *testing.T) {
	fx := newFixture(t)
	c := gitClient(t, fx, "")
	_, err := c.CreateTrainBranch(ctx, repo, forge.TrainSpec{Branch: "train/1", BaseSHA: fx.main2, PR: 1, HeadSHA: fx.clash})
	if !errors.Is(err, forge.ErrStale) {
		t.Fatalf("err = %v, want ErrStale", err)
	}
	if out := runGit(t, fx.origin, "for-each-ref", "refs/heads/train/"); out != "" {
		t.Fatalf("a stale build left a branch: %s", out)
	}
}

// TestTokenNeverInArgv runs git through a wrapper that records every argv and
// whether the Authorization header arrived through the environment.
func TestTokenNeverInArgv(t *testing.T) {
	fx := newFixture(t)
	real, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not installed")
	}
	dir := t.TempDir()
	logf := filepath.Join(dir, "argv.log")
	wrapper := filepath.Join(dir, "git")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> '" + logf + "'\n" +
		"[ \"$GIT_CONFIG_KEY_0\" = http.extraHeader ] && [ \"$GIT_CONFIG_VALUE_0\" = 'Authorization: token " + token + "' ] && echo HEADER-VIA-ENV >> '" + logf + "'\n" +
		"exec '" + real + "' \"$@\"\n"
	write(t, wrapper, script, 0o755)

	c := gitClient(t, fx, wrapper)
	tb, err := c.CreateTrainBranch(ctx, repo, forge.TrainSpec{Branch: "train/1", BaseSHA: fx.main2, PR: 1, HeadSHA: fx.head})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.TreeOf(ctx, repo, tb.SHA); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(logf)
	if err != nil {
		t.Fatal(err)
	}
	log := string(b)
	if strings.Contains(log, token) {
		t.Fatalf("token appeared in git argv:\n%s", log)
	}
	if !strings.Contains(log, "push") || !strings.Contains(log, "merge-tree") {
		t.Fatalf("wrapper did not see the build:\n%s", log)
	}
	if !strings.Contains(log, "HEADER-VIA-ENV") {
		t.Fatalf("the Authorization header never reached git's environment:\n%s", log)
	}
}

func TestGitErrorsCarryNoStderr(t *testing.T) {
	fx := newFixture(t)
	c := gitClient(t, fx, "")
	c.gitURL = func(string) string { return filepath.Join(t.TempDir(), "no-such-remote-"+token) }
	_, err := c.CreateTrainBranch(ctx, repo, forge.TrainSpec{Branch: "train/1", BaseSHA: fx.main2, PR: 1, HeadSHA: fx.head})
	if err == nil {
		t.Fatal("fetch from a missing remote succeeded")
	}
	if strings.Contains(err.Error(), token) {
		t.Fatalf("git error leaks what the remote said: %v", err)
	}
}

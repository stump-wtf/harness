package mergetrain

// Driver Tests
//
// Governing tests: SPEC-0025 REQ-1, REQ-7..REQ-11, REQ-16; #603 — three PRs
// merge in Order order; a red middle PR is commented once and skipped while
// the third merges; a head that moves before the merge is not merged; a
// verification failure halts; the same failure twice on one head is one
// comment; a second driver on the repo is refused.

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/forge"
	"github.com/stump-wtf/harness/internal/forge/fake"
)

// prAt returns an eligible PR n whose head is "head<n>" and whose qualifying
// approval is at t0 + approvedAfter.
func prAt(n int, approvedAfter time.Duration) PullRequest {
	head := fmt.Sprintf("%040x", 0xa0000+n)
	return PullRequest{
		Number: n, Author: fmt.Sprintf("author%d", n), HeadSHA: head, CIState: "success", Mergeable: true,
		Reviews: []Review{{Author: "joestump-agent", State: StateApproved, CommitID: head, SubmittedAt: t0.Add(approvedAfter)}},
	}
}

// trainFor names the train commit the fake builds for pr on a given base.
func trainFor(pr int, base string) string { return fmt.Sprintf("train-%d-on-%s", pr, base) }

// world is a fake forge wired so every build yields trainFor(pr, base) with
// one changed file whose bytes are "pr<n>", and CI is green unless red says
// otherwise.
type world struct {
	*fake.Fake
	mu  sync.Mutex
	red map[int]bool
}

func newWorld(t *testing.T, prs ...PullRequest) *world {
	t.Helper()
	w := &world{Fake: fake.New(), red: map[int]bool{}}
	w.SetBranch(repo, "main", "base0")
	for _, p := range prs {
		w.AddPR(repo, p)
	}
	w.OnTrain(func(s forge.TrainSpec) (forge.TrainBranch, error) {
		sha := trainFor(s.PR, s.BaseSHA)
		path := fmt.Sprintf("pr%d.txt", s.PR)
		w.SetFile(repo, sha, path, []byte(fmt.Sprintf("pr%d", s.PR)))
		return forge.TrainBranch{SHA: sha, Tree: "tree-" + sha, Changed: []string{path}}, nil
	})
	w.OnStatus(func(sha string, _ int) string {
		w.mu.Lock()
		defer w.mu.Unlock()
		for n, isRed := range w.red {
			if isRed && strings.HasPrefix(sha, fmt.Sprintf("train-%d-on-", n)) {
				return "failure"
			}
		}
		return "success"
	})
	return w
}

func (w *world) setRed(n int, red bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.red[n] = red
}

func driverCfg(t *testing.T, mode Mode, log Logger) DriverConfig {
	return DriverConfig{
		Repo: repo, Mode: mode, PollInterval: time.Hour, CITimeout: time.Second,
		LockDir: t.TempDir(), Log: log, TrainPollMin: time.Millisecond, RetryPause: time.Millisecond,
	}
}

func newDriver(t *testing.T, f forge.Forge, cfg DriverConfig) *Driver {
	t.Helper()
	d, err := NewDriver(cfg, f)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return d
}

func ticks(t *testing.T, d *Driver, n int) {
	t.Helper()
	for i := range n {
		if err := d.Tick(context.Background()); err != nil {
			t.Fatalf("tick %d: %v", i+1, err)
		}
	}
}

func mergedOrder(f *fake.Fake) []int {
	var out []int
	for _, c := range f.Calls() {
		if c.Method == "SquashMerge" {
			out = append(out, c.Args[1].(int))
		}
	}
	return out
}

func markers(comments []string) int {
	n := 0
	for _, c := range comments {
		n += strings.Count(c, "<!-- harness-mergetrain v1 ")
	}
	return n
}

func TestDriverMergesInOrder(t *testing.T) {
	// Approval order 3, 1, 2 — deliberately not number order.
	w := newWorld(t, prAt(1, time.Minute), prAt(2, 2*time.Minute), prAt(3, 0))
	log := &logRec{}
	d := newDriver(t, w, driverCfg(t, ModeMerge, log))
	ticks(t, d, 4) // three merges, then an idle tick

	if got := mergedOrder(w.Fake); !slices.Equal(got, []int{3, 1, 2}) {
		t.Fatalf("merge order = %v, want [3 1 2]", got)
	}
	for _, n := range []int{1, 2, 3} {
		if !w.Merged(repo, n) {
			t.Fatalf("#%d not merged", n)
		}
		if len(w.Comments(repo, n)) != 0 {
			t.Fatalf("#%d was commented on: %q", n, w.Comments(repo, n))
		}
	}
	// Each merge was verified, pinned to the head, and the forge's default
	// title was used.
	for _, c := range w.Calls() {
		if c.Method == "SquashMerge" {
			if c.Args[2] != prAt(c.Args[1].(int), 0).HeadSHA || c.Args[3] != "" || !strings.HasPrefix(c.Args[4].(string), "Merge-Train: tested as train-") {
				t.Fatalf("SquashMerge args = %v", c.Args)
			}
		}
	}
	if !log.has("INFO mergetrain verified") || log.has("WARN mergetrain bypass detected") {
		t.Fatalf("log:\n%s", strings.Join(log.lines, "\n"))
	}
	noTrainBranches(t, w.Fake)
}

func TestDriverRedMiddlePRCommentedAndSkipped(t *testing.T) {
	w := newWorld(t, prAt(1, 0), prAt(2, time.Minute), prAt(3, 2*time.Minute))
	w.setRed(2, true)
	d := newDriver(t, w, driverCfg(t, ModeMerge, nil))
	ticks(t, d, 5)

	if got := mergedOrder(w.Fake); !slices.Equal(got, []int{1, 3}) {
		t.Fatalf("merged = %v, want [1 3]", got)
	}
	if w.Merged(repo, 2) {
		t.Fatal("the red PR was merged")
	}
	cs := w.Comments(repo, 2)
	if len(cs) != 1 || markers(cs) != 1 {
		t.Fatalf("#2 comments = %q, want exactly one with a marker", cs)
	}
	head := prAt(2, 0).HeadSHA
	if !strings.HasPrefix(cs[0], "@author2 ") || !strings.Contains(cs[0], Marker(2, head, CauseRed)) {
		t.Fatalf("comment does not @mention the author or carry the red marker:\n%s", cs[0])
	}
	noTrainBranches(t, w.Fake)
}

func TestDriverSameFailureTwiceOneComment(t *testing.T) {
	w := newWorld(t, prAt(1, 0))
	w.setRed(1, true)
	d := newDriver(t, w, driverCfg(t, ModeMerge, nil))
	ticks(t, d, 1)
	ticks(t, d, 2) // same (head, base): not rebuilt
	if n := strings.Count(strings.Join(w.Methods(), " "), "CreateTrainBranch"); n != 1 {
		t.Fatalf("rebuilt the same (head, base) %d times", n)
	}
	// main moves: the PR is attempted again, red again — and still one comment.
	w.SetBranch(repo, "main", "base1")
	ticks(t, d, 1)
	if n := strings.Count(strings.Join(w.Methods(), " "), "CreateTrainBranch"); n != 2 {
		t.Fatalf("not retried on the new base: %d builds", n)
	}
	if cs := w.Comments(repo, 1); len(cs) != 1 {
		t.Fatalf("comments = %d, want 1: %q", len(cs), cs)
	}
}

func TestDriverRedThenGreenOnNewBase(t *testing.T) {
	w := newWorld(t, prAt(1, 0))
	w.setRed(1, true)
	d := newDriver(t, w, driverCfg(t, ModeMerge, nil))
	ticks(t, d, 1)
	w.setRed(1, false) // a flake
	w.SetBranch(repo, "main", "base1")
	ticks(t, d, 1)
	if !w.Merged(repo, 1) {
		t.Fatal("a red flake was not retried when main moved")
	}
}

// moving changes a PR's head the moment its train's CI is first polled —
// after eligibility and the build, before the merge.
func TestDriverHeadMovedBeforeMergeIsSkipped(t *testing.T) {
	pr := prAt(1, 0)
	w := newWorld(t, pr)
	var once sync.Once
	w.OnStatus(func(string, int) string {
		once.Do(func() {
			moved := prAt(1, 0)
			moved.HeadSHA = strings.Repeat("b", 40)
			moved.Reviews[0].CommitID = moved.HeadSHA
			w.AddPR(repo, moved)
		})
		return "success"
	})
	log := &logRec{}
	d := newDriver(t, w, driverCfg(t, ModeMerge, log))
	ticks(t, d, 1)
	if w.Merged(repo, 1) || slices.Contains(w.Methods(), "SquashMerge") {
		t.Fatal("merged a PR whose head moved after its train was built")
	}
	if len(w.Comments(repo, 1)) != 0 {
		t.Fatal("a stale head was commented on")
	}
	if !log.has("INFO mergetrain stale") {
		t.Fatal("no stale log line")
	}
	// The next tick sees the new head and lands it.
	ticks(t, d, 1)
	if !w.Merged(repo, 1) {
		t.Fatal("the new head was not merged on the next tick")
	}
}

func TestDriverMainMovedBeforeMergeIsSkipped(t *testing.T) {
	w := newWorld(t, prAt(1, 0))
	var once sync.Once
	w.OnStatus(func(string, int) string {
		once.Do(func() { w.SetBranch(repo, "main", "someone-else") })
		return "success"
	})
	d := newDriver(t, w, driverCfg(t, ModeMerge, nil))
	ticks(t, d, 1)
	if slices.Contains(w.Methods(), "SquashMerge") {
		t.Fatal("merged although main moved after the train was built")
	}
}

func TestDriverTreeMismatchHalts(t *testing.T) {
	w := newWorld(t, prAt(1, 0), prAt(2, time.Minute))
	w.OnMerge(func(int, forge.TrainBranch) string { return "some-other-tree" })
	log := &logRec{}
	d := newDriver(t, w, driverCfg(t, ModeMerge, log))
	err := d.Tick(context.Background())
	if !errors.Is(err, ErrHalted) || !strings.Contains(err.Error(), "some-other-tree") {
		t.Fatalf("Tick = %v, want ErrHalted naming the trees", err)
	}
	if !log.has("ERROR mergetrain halted") {
		t.Fatal("halt not logged at error level")
	}
	if cs := w.Comments(repo, 1); len(cs) != 1 || !strings.Contains(cs[0], "cause="+CauseVerifyFailed) {
		t.Fatalf("comments = %q, want one verify-failed", cs)
	}
	// Halted means halted: no further forge calls, #2 untouched.
	before := len(w.Calls())
	if err := d.Tick(context.Background()); !errors.Is(err, ErrHalted) {
		t.Fatalf("second Tick = %v", err)
	}
	if len(w.Calls()) != before || w.Merged(repo, 2) {
		t.Fatal("a halted driver kept working")
	}
}

// contentLies serves different bytes for main than the train had, with the
// tree check passing — so only VerifyLanded can catch it.
type contentLies struct{ *world }

func (c contentLies) FileContentAtRef(ctx context.Context, repo, path, ref string) ([]byte, error) {
	if ref == "main" {
		return []byte("not what was tested"), nil
	}
	return c.world.FileContentAtRef(ctx, repo, path, ref)
}

func TestDriverContentMismatchHalts(t *testing.T) {
	w := newWorld(t, prAt(1, 0))
	d := newDriver(t, contentLies{w}, driverCfg(t, ModeMerge, nil))
	err := d.Tick(context.Background())
	if !errors.Is(err, ErrHalted) || !strings.Contains(err.Error(), "pr1.txt") {
		t.Fatalf("Tick = %v, want ErrHalted naming pr1.txt", err)
	}
}

func TestDriverSecondDriverRefused(t *testing.T) {
	w := newWorld(t, prAt(1, 0))
	cfg := driverCfg(t, ModeMerge, nil)
	first := newDriver(t, w, cfg)
	if _, err := NewDriver(cfg, w); !errors.Is(err, ErrLocked) || !strings.Contains(err.Error(), repo) {
		t.Fatalf("second NewDriver = %v, want ErrLocked naming the repo", err)
	}
	if n := len(w.Calls()); n != 0 {
		t.Fatalf("the refused driver made %d forge calls", n)
	}
	_ = first.Close()
	again, err := NewDriver(cfg, w)
	if err != nil {
		t.Fatalf("lock not released by Close: %v", err)
	}
	_ = again.Close()
}

func TestDriverReportModeNeverWrites(t *testing.T) {
	w := newWorld(t, prAt(1, 0), prAt(2, time.Minute))
	w.setRed(2, true)
	log := &logRec{}
	d := newDriver(t, w, driverCfg(t, ModeReport, log))
	ticks(t, d, 4)
	for _, m := range w.Methods() {
		if m == "SquashMerge" || m == "Comment" {
			t.Fatalf("report mode called %s", m)
		}
	}
	if !log.has("INFO mergetrain would merge") || !log.has("INFO mergetrain would comment") {
		t.Fatalf("log:\n%s", strings.Join(log.lines, "\n"))
	}
	// Each (head, base) is built once, not every tick.
	if n := strings.Count(strings.Join(w.Methods(), " "), "CreateTrainBranch"); n != 2 {
		t.Fatalf("built %d trains over 4 ticks, want 2", n)
	}
	noTrainBranches(t, w.Fake)
}

func TestDriverDefaultsToReport(t *testing.T) {
	w := newWorld(t, prAt(1, 0))
	cfg := driverCfg(t, "", nil)
	d := newDriver(t, w, cfg)
	ticks(t, d, 1)
	if slices.Contains(w.Methods(), "SquashMerge") {
		t.Fatal("an unset mode merged")
	}
}

func TestDriverBypassDetected(t *testing.T) {
	w := newWorld(t)
	log := &logRec{}
	d := newDriver(t, w, driverCfg(t, ModeMerge, log))
	ticks(t, d, 1)
	w.SetBranch(repo, "main", "hand-merged")
	ticks(t, d, 1)
	if !log.has("WARN mergetrain bypass detected") {
		t.Fatal("a main head the train did not produce went unreported")
	}
}

// ambiguous merges, then answers 502, as a proxy timing out would.
type ambiguous struct{ *world }

func (a ambiguous) SquashMerge(ctx context.Context, repo string, pr int, head, title, msg string) (string, error) {
	if _, err := a.world.SquashMerge(ctx, repo, pr, head, title, msg); err != nil {
		return "", err
	}
	return "", &forge.StatusError{Method: "SquashMerge", Status: 502, Repo: repo}
}

func TestDriverAmbiguousMergeIsVerified(t *testing.T) {
	w := newWorld(t, prAt(1, 0))
	log := &logRec{}
	d := newDriver(t, ambiguous{w}, driverCfg(t, ModeMerge, log))
	ticks(t, d, 1)
	if !w.Merged(repo, 1) || !log.has("INFO mergetrain verified") {
		t.Fatalf("an ambiguous 502 merge was not verified:\n%s", strings.Join(log.lines, "\n"))
	}
}

func TestDriverMergeRefusedIsCommented(t *testing.T) {
	w := newWorld(t, prAt(1, 0))
	w.Err("SquashMerge", &forge.StatusError{Method: "SquashMerge", Status: 405, Repo: repo})
	d := newDriver(t, w, driverCfg(t, ModeMerge, nil))
	ticks(t, d, 2)
	cs := w.Comments(repo, 1)
	if len(cs) != 1 || !strings.Contains(cs[0], "cause="+CauseMergeRefused) {
		t.Fatalf("comments = %q, want one merge-refused", cs)
	}
	if n := strings.Count(strings.Join(w.Methods(), " "), "SquashMerge"); n != 1 {
		t.Fatalf("retried a refused merge on the same (head, base): %d calls", n)
	}
}

func TestDriverCommentRetriedWhenPostingFails(t *testing.T) {
	w := newWorld(t, prAt(1, 0))
	w.setRed(1, true)
	w.Err("Comment", &forge.StatusError{Method: "Comment", Status: 503, Repo: repo})
	d := newDriver(t, w, driverCfg(t, ModeMerge, nil))
	ticks(t, d, 1)
	if len(w.Comments(repo, 1)) != 0 {
		t.Fatal("comment posted despite the forge refusing")
	}
	w.Err("Comment", nil)
	ticks(t, d, 1)
	if cs := w.Comments(repo, 1); len(cs) != 1 {
		t.Fatalf("the author was never told: comments = %q", cs)
	}
	if n := strings.Count(strings.Join(w.Methods(), " "), "CreateTrainBranch"); n != 1 {
		t.Fatalf("re-ran CI just to comment: %d builds", n)
	}
}

func TestDriverForgeErrorIsNotAComment(t *testing.T) {
	w := newWorld(t, prAt(1, 0))
	w.Err("CreateTrainBranch", &forge.StatusError{Method: "CreateTrainBranch", Status: 500, Repo: repo})
	d := newDriver(t, w, driverCfg(t, ModeMerge, nil))
	if err := d.Tick(context.Background()); err == nil {
		t.Fatal("a forge error was not reported by Tick")
	}
	if len(w.Comments(repo, 1)) != 0 {
		t.Fatal("the author was blamed for a forge 500")
	}
	w.Err("CreateTrainBranch", nil)
	ticks(t, d, 1)
	if !w.Merged(repo, 1) {
		t.Fatal("not retried after the forge recovered")
	}
}

func TestDriverRunStopsOnCancel(t *testing.T) {
	w := newWorld(t, prAt(1, 0))
	d := newDriver(t, w, driverCfg(t, ModeMerge, nil))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	deadline := time.After(5 * time.Second)
	for !w.Merged(repo, 1) {
		select {
		case <-deadline:
			t.Fatal("Run never merged")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run = %v, want nil on cancel", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

func TestNewDriverValidates(t *testing.T) {
	good := driverCfg(t, ModeMerge, nil)
	for name, mut := range map[string]func(*DriverConfig){
		"bad mode":    func(c *DriverConfig) { c.Mode = "yolo" },
		"no interval": func(c *DriverConfig) { c.PollInterval = 0 },
		"no timeout":  func(c *DriverConfig) { c.CITimeout = 0 },
		"no lock dir": func(c *DriverConfig) { c.LockDir = "" },
		"bad repo":    func(c *DriverConfig) { c.Repo = "harness" },
	} {
		c := good
		mut(&c)
		if d, err := NewDriver(c, fake.New()); err == nil {
			_ = d.Close()
			t.Errorf("%s: NewDriver succeeded", name)
		}
	}
}

// cancelOnVerify wraps a forge and cancels ctx the first time verify reads a
// file — the window between a successful merge and its verification, where a
// daemon shutdown lands. Deterministic: no sleeping, no racing the scheduler.
type cancelOnVerify struct {
	*fake.Fake
	cancel   context.CancelFunc
	armed    bool
	verifies int
}

func (c *cancelOnVerify) FileContentAtRef(ctx context.Context, repo, path, ref string) ([]byte, error) {
	if c.armed && ref == "main" {
		c.armed = false
		c.verifies++
		c.cancel()
	}
	return c.Fake.FileContentAtRef(ctx, repo, path, ref)
}

// TestDriverCancelDuringVerifyDoesNotHalt is the regression for a shutdown
// arriving between the merge and its verification. verify's reads then fail
// with context.Canceled, and treating any verify error as a halt stopped the
// driver permanently for a clean stop — and told the author their merge was
// unverified when the daemon had simply gone away.
//
// The merge itself stands: it already returned. Run must report the
// cancellation, not ErrHalted, and must not comment.
func TestDriverCancelDuringVerifyDoesNotHalt(t *testing.T) {
	w := newWorld(t, prAt(1, 0))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &cancelOnVerify{Fake: w.Fake, cancel: cancel}
	log := &logRec{}
	d := newDriver(t, c, driverCfg(t, ModeMerge, log))

	c.armed = true
	err := d.Tick(ctx)
	if errors.Is(err, ErrHalted) {
		t.Fatalf("a cancelled shutdown halted the driver: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Tick = %v, want context.Canceled", err)
	}
	if c.verifies == 0 {
		t.Fatal("the cancel never fired; the test proved nothing")
	}
	if !w.Merged(repo, 1) {
		t.Fatal("the merge did not happen, so this is not the cancel-after-merge window")
	}
	if n := len(w.Comments(repo, 1)); n != 0 {
		t.Fatalf("a cancellation commented on the PR (%d comments)", n)
	}
	if log.has("ERROR mergetrain halted") {
		t.Fatal("a cancellation was logged as a halt")
	}
	// A later tick with a live context is not blocked: the driver is not halted.
	if err := d.Tick(context.Background()); errors.Is(err, ErrHalted) {
		t.Fatalf("the driver halted anyway: %v", err)
	}
}

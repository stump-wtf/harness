package mergetrain

// Train Attempt Tests
//
// Governing tests: SPEC-0025 REQ-4..REQ-6; #602 — green, red, timeout,
// cancellation mid-poll, conflict without polling, a delete failure that does
// not mask the outcome, and a panic. Every case ends by asserting no train/*
// branch is left in the fake.

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

// logRec records log lines as "LEVEL message".
type logRec struct {
	mu    sync.Mutex
	lines []string
}

func (l *logRec) add(level string, msg interface{}, kv []interface{}) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf("%s %v %v", level, msg, kv))
}
func (l *logRec) Info(m interface{}, kv ...interface{})  { l.add("INFO", m, kv) }
func (l *logRec) Warn(m interface{}, kv ...interface{})  { l.add("WARN", m, kv) }
func (l *logRec) Error(m interface{}, kv ...interface{}) { l.add("ERROR", m, kv) }

func (l *logRec) has(prefix string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, s := range l.lines {
		if strings.HasPrefix(s, prefix) {
			return true
		}
	}
	return false
}

const trainSHA = "7777777777777777777777777777777777777777"

// trainSetup returns a fake with PR 7 open at head, main at "base", and a
// build that yields trainSHA changing a.go (whose bytes on the train commit
// are "A").
func trainSetup(t *testing.T) (*fake.Fake, PullRequest) {
	t.Helper()
	f := fake.New()
	pr := ready()
	f.AddPR(repo, pr)
	f.SetBranch(repo, "main", "base")
	f.OnTrain(func(forge.TrainSpec) (forge.TrainBranch, error) {
		return forge.TrainBranch{SHA: trainSHA, Tree: "T", Changed: []string{"a.go"}}, nil
	})
	f.SetFile(repo, trainSHA, "a.go", []byte("A"))
	return f, pr
}

func cfg(log Logger) TrainConfig {
	return TrainConfig{Repo: repo, PollMin: time.Millisecond, PollMax: 4 * time.Millisecond, Timeout: time.Second, DeleteRetry: time.Millisecond, Log: log}
}

// noTrainBranches is the acceptance check every case ends with.
func noTrainBranches(t *testing.T, f forge.Forge) {
	t.Helper()
	var bs map[string]string
	switch v := f.(type) {
	case *fake.Fake:
		bs = v.Branches(repo)
	case *flaky:
		bs = v.Branches(repo)
	}
	for name := range bs {
		if strings.HasPrefix(name, "train/") {
			t.Fatalf("branch %s left behind", name)
		}
	}
}

func count(methods []string, m string) int {
	n := 0
	for _, x := range methods {
		if x == m {
			n++
		}
	}
	return n
}

func TestTrainGreen(t *testing.T) {
	f, pr := trainSetup(t)
	f.OnStatus(func(_ string, n int) string {
		if n < 3 {
			return "pending"
		}
		return "success"
	})
	log := &logRec{}
	res := RunTrain(context.Background(), f, cfg(log), pr, "base")
	if res.Outcome != OutcomeGreen || res.Err != nil {
		t.Fatalf("Result = %+v, want green", res)
	}
	if res.Train.SHA != trainSHA || res.Base != "base" || string(res.Want["a.go"]) != "A" {
		t.Fatalf("Result = %+v", res)
	}
	m := f.Methods()
	if n := count(m, "CombinedStatus"); n != 3 {
		t.Fatalf("polled %d times, want 3: %v", n, m)
	}
	// Build before poll, poll before delete, and the polled SHA is the train
	// commit, not the PR head.
	if i, j := slices.Index(m, "CreateTrainBranch"), slices.Index(m, "CombinedStatus"); i < 0 || j < i {
		t.Fatalf("order: %v", m)
	}
	if last := m[len(m)-1]; last != "DeleteBranch" {
		t.Fatalf("last call %s, want DeleteBranch: %v", last, m)
	}
	for _, c := range f.Calls() {
		if c.Method == "CombinedStatus" && c.Args[1] != trainSHA {
			t.Fatalf("polled %v, want the train commit", c.Args[1])
		}
	}
	for _, ev := range []string{"INFO mergetrain building", "INFO mergetrain built", "INFO mergetrain green", "INFO mergetrain deleted"} {
		if !log.has(ev) {
			t.Errorf("no %q log line", ev)
		}
	}
	noTrainBranches(t, f)
}

func TestTrainRed(t *testing.T) {
	for _, st := range []string{"failure", "error"} {
		f, pr := trainSetup(t)
		f.SetStatus(repo, trainSHA, st)
		res := RunTrain(context.Background(), f, cfg(nil), pr, "base")
		if res.Outcome != OutcomeRed || res.Status != st {
			t.Fatalf("%s: Result = %+v, want red", st, res)
		}
		if res.Want != nil {
			t.Fatalf("%s: a red train captured Want", st)
		}
		noTrainBranches(t, f)
	}
}

func TestTrainTimeout(t *testing.T) {
	f, pr := trainSetup(t) // status never set: pending forever
	c := cfg(nil)
	c.Timeout = 30 * time.Millisecond
	start := time.Now()
	res := RunTrain(context.Background(), f, c, pr, "base")
	if res.Outcome != OutcomeTimeout {
		t.Fatalf("Result = %+v, want timeout", res)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("timeout took %v", took)
	}
	noTrainBranches(t, f)
}

func TestTrainCancelledMidPoll(t *testing.T) {
	f, pr := trainSetup(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.OnStatus(func(_ string, n int) string {
		if n == 2 {
			cancel()
		}
		return "pending"
	})
	res := RunTrain(ctx, f, cfg(nil), pr, "base")
	if res.Outcome != OutcomeCancelled || !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("Result = %+v, want cancelled", res)
	}
	noTrainBranches(t, f)
}

func TestTrainConflictDoesNotPoll(t *testing.T) {
	f, pr := trainSetup(t)
	f.OnTrain(func(forge.TrainSpec) (forge.TrainBranch, error) { return forge.TrainBranch{}, forge.ErrConflict })
	res := RunTrain(context.Background(), f, cfg(nil), pr, "base")
	if res.Outcome != OutcomeConflict || !errors.Is(res.Err, forge.ErrConflict) {
		t.Fatalf("Result = %+v, want conflict", res)
	}
	if n := count(f.Methods(), "CombinedStatus"); n != 0 {
		t.Fatalf("a conflict polled CI %d times", n)
	}
	noTrainBranches(t, f)
}

func TestTrainStaleHead(t *testing.T) {
	f, pr := trainSetup(t)
	pr.HeadSHA = stale // the forge still has the newer head
	res := RunTrain(context.Background(), f, cfg(nil), pr, "base")
	if res.Outcome != OutcomeStale {
		t.Fatalf("Result = %+v, want stale", res)
	}
	noTrainBranches(t, f)
}

// flaky wraps the fake: it can fail DeleteBranch a number of times after the
// build, fail CombinedStatus with a chosen error once, or panic in it.
type flaky struct {
	*fake.Fake
	mu            sync.Mutex
	built         bool
	deleteFails   int
	statusErrOnce error
	panicOnStatus bool
}

func (f *flaky) CreateTrainBranch(ctx context.Context, repo string, s forge.TrainSpec) (forge.TrainBranch, error) {
	tb, err := f.Fake.CreateTrainBranch(ctx, repo, s)
	f.mu.Lock()
	f.built = err == nil
	f.mu.Unlock()
	return tb, err
}

func (f *flaky) DeleteBranch(ctx context.Context, repo, name string) error {
	f.mu.Lock()
	fail := f.built && f.deleteFails > 0
	if fail {
		f.deleteFails--
	}
	f.mu.Unlock()
	if fail {
		return &forge.StatusError{Method: "DeleteBranch", Status: 500, Repo: repo}
	}
	return f.Fake.DeleteBranch(ctx, repo, name)
}

func (f *flaky) CombinedStatus(ctx context.Context, repo, sha string) (string, error) {
	f.mu.Lock()
	p, e := f.panicOnStatus, f.statusErrOnce
	f.statusErrOnce = nil
	f.mu.Unlock()
	if p {
		panic("forge exploded")
	}
	if e != nil {
		return "", e
	}
	return f.Fake.CombinedStatus(ctx, repo, sha)
}

func TestTrainDeleteFailureDoesNotMaskOutcome(t *testing.T) {
	base, pr := trainSetup(t)
	base.SetStatus(repo, trainSHA, "success")
	f := &flaky{Fake: base, deleteFails: 1}
	log := &logRec{}
	res := RunTrain(context.Background(), f, cfg(log), pr, "base")
	if res.Outcome != OutcomeGreen || res.Err != nil {
		t.Fatalf("Result = %+v, want green despite the delete failure", res)
	}
	if !log.has("WARN mergetrain delete failed") {
		t.Fatal("the delete failure was not logged")
	}
	if !log.has("INFO mergetrain deleted") {
		t.Fatal("the retry did not delete the branch")
	}
	noTrainBranches(t, f)
}

func TestTrainPanicStillDeletes(t *testing.T) {
	base, pr := trainSetup(t)
	f := &flaky{Fake: base, panicOnStatus: true}
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("the panic was swallowed")
			}
		}()
		RunTrain(context.Background(), f, cfg(nil), pr, "base")
	}()
	noTrainBranches(t, f)
}

func TestTrainTransientPollErrorKeepsWaiting(t *testing.T) {
	base, pr := trainSetup(t)
	base.SetStatus(repo, trainSHA, "success")
	f := &flaky{Fake: base, statusErrOnce: &forge.StatusError{Method: "CombinedStatus", Status: 502, Repo: repo}}
	res := RunTrain(context.Background(), f, cfg(nil), pr, "base")
	if res.Outcome != OutcomeGreen {
		t.Fatalf("Result = %+v, want green after a 502 blip", res)
	}
	noTrainBranches(t, f)
}

func TestTrainRefusedPollIsForgeError(t *testing.T) {
	base, pr := trainSetup(t)
	f := &flaky{Fake: base, statusErrOnce: &forge.StatusError{Method: "CombinedStatus", Status: 403, Repo: repo}}
	res := RunTrain(context.Background(), f, cfg(nil), pr, "base")
	if res.Outcome != OutcomeForgeError {
		t.Fatalf("Result = %+v, want forge error on a 403", res)
	}
	noTrainBranches(t, f)
}

func TestTrainRemovesLeftoverBeforeBuilding(t *testing.T) {
	f, pr := trainSetup(t)
	f.SetBranch(repo, TrainBranchName(pr.Number), "crashed-attempt")
	f.SetStatus(repo, trainSHA, "success")
	if res := RunTrain(context.Background(), f, cfg(nil), pr, "base"); res.Outcome != OutcomeGreen {
		t.Fatalf("Result = %+v, want green over a leftover branch", res)
	}
	noTrainBranches(t, f)
}

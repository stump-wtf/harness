package mergetrain

// Infra Flake Retry Tests
//
// Governing tests: SPEC-0025 REQ-18, ADR-0056; #1016 — a train that goes red
// only on jobs no runner ever started waits a cool-down and tries that tree
// once more instead of waiting for `main` or the head to move; a real test
// failure keeps today's stop-and-comment; the retry is strictly once, so a tree
// that fails the same way twice cannot loop the train.

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/forge"
	"github.com/stump-wtf/harness/internal/forge/fake"
)

// flakeCfg is driverCfg with the cool-down on; the driver's clock is driven by
// the test, so no test sleeps.
func flakeCfg(t *testing.T, mode Mode, coolDown time.Duration) DriverConfig {
	t.Helper()
	cfg := driverCfg(t, mode, nil)
	cfg.FlakeCoolDown = coolDown
	return cfg
}

// buildsOf counts the train commits the fake was asked to build.
func buildsOf(f *fake.Fake) int {
	return strings.Count(strings.Join(f.Methods(), " "), "CreateTrainBranch")
}

var errForge = errors.New("forge: unavailable")

func TestRedOnNeverStartedJobIsRetriedAfterCoolDown(t *testing.T) {
	w := newWorld(t, prAt(1, 0))
	w.setRed(1, true)
	w.neverStarted(1)

	now := t0
	d := newDriver(t, w, flakeCfg(t, ModeMerge, 10*time.Minute))
	d.now = func() time.Time { return now }

	ticks(t, d, 1)
	if cs := w.Comments(repo, 1); len(cs) != 0 {
		t.Fatalf("an infra failure was commented on before its retry: %q", cs)
	}

	// Still inside the cool-down: the tree is not re-run yet, and the tick is
	// not wasted on it either.
	ticks(t, d, 1)
	if n := buildsOf(w.Fake); n != 1 {
		t.Fatalf("rebuilt the tree during the cool-down: %d builds", n)
	}

	// The cool-down passes and the runner has come back: the same tree lands.
	w.setRed(1, false)
	now = t0.Add(11 * time.Minute)
	ticks(t, d, 1)
	if !w.Merged(repo, 1) {
		t.Fatalf("the retried tree did not land; comments: %q", w.Comments(repo, 1))
	}
	if n := buildsOf(w.Fake); n != 2 {
		t.Fatalf("builds = %d, want the attempt and its one retry", n)
	}
	if cs := w.Comments(repo, 1); len(cs) != 0 {
		t.Fatalf("a tree that landed was commented on: %q", cs)
	}
	noTrainBranches(t, w.Fake)
}

func TestRedOnARealFailureIsNotRetried(t *testing.T) {
	w := newWorld(t, prAt(1, 0))
	w.setRed(1, true) // failed with a log: the tree is on trial

	now := t0
	d := newDriver(t, w, flakeCfg(t, ModeMerge, 10*time.Minute))
	d.now = func() time.Time { return now }

	ticks(t, d, 1)
	cs := w.Comments(repo, 1)
	if len(cs) != 1 || markers(cs) != 1 {
		t.Fatalf("#1 comments = %q, want exactly one with a marker", cs)
	}
	if strings.Contains(cs[0], "no runner") {
		t.Fatalf("a real failure was described as an infra one:\n%s", cs[0])
	}
	now = t0.Add(time.Hour)
	ticks(t, d, 3)
	if n := buildsOf(w.Fake); n != 1 {
		t.Fatalf("a real failure was re-run %d times, want the attempt alone", n)
	}
}

func TestInfraFailureIsRetriedExactlyOnce(t *testing.T) {
	w := newWorld(t, prAt(1, 0))
	w.setRed(1, true)
	w.neverStarted(1)

	now := t0
	d := newDriver(t, w, flakeCfg(t, ModeMerge, 10*time.Minute))
	d.now = func() time.Time { return now }

	ticks(t, d, 1) // red, classified infra: retry scheduled, nothing said
	for i := range 4 {
		now = t0.Add(time.Duration(i+1) * 10 * time.Minute)
		ticks(t, d, 1) // red again, same signature, forever
	}

	if n := buildsOf(w.Fake); n != 2 {
		t.Fatalf("builds = %d, want the attempt and one retry, no more", n)
	}
	cs := w.Comments(repo, 1)
	if len(cs) != 1 || markers(cs) != 1 {
		t.Fatalf("#1 comments = %q, want exactly one with a marker", cs)
	}
	if !strings.Contains(cs[0], "no runner") {
		t.Fatalf("the comment does not say the tree was re-run on the infra rule:\n%s", cs[0])
	}
	if got := d.flake[attemptKey{1, prAt(1, 0).HeadSHA, "base0"}]; !got.tried {
		t.Fatal("the retry was not recorded as spent")
	}
}

// Without a cool-down configured the driver is exactly what it was before: a
// red attempt is remembered and commented, and nothing is re-run.
func TestRetryOffByDefault(t *testing.T) {
	w := newWorld(t, prAt(1, 0))
	w.setRed(1, true)
	w.neverStarted(1)
	d := newDriver(t, w, driverCfg(t, ModeMerge, nil))
	if d.cfg.FlakeCoolDown != 0 {
		t.Fatalf("FlakeCoolDown = %s, want 0 in a plain DriverConfig", d.cfg.FlakeCoolDown)
	}
	ticks(t, d, 1)
	if len(w.Comments(repo, 1)) != 1 {
		t.Fatalf("comments = %q, want the one failure comment", w.Comments(repo, 1))
	}
	ticks(t, d, 2)
	if n := buildsOf(w.Fake); n != 1 {
		t.Fatalf("builds = %d, want no retry while the cool-down is off", n)
	}
}

// A forge that reports no per-job detail cannot classify anything, and a forge
// that reports jobs answers through infraFailure alone.
func TestInfraFailureRules(t *testing.T) {
	tests := []struct {
		name string
		jobs []forge.Job
		err  error
		want string
	}{
		{
			name: "every failure never started",
			jobs: []forge.Job{{Name: "secrets", Conclusion: "failure", NeverStarted: true},
				{Name: "build", Conclusion: "skipped"},
				{Name: "pages", Conclusion: "success"}},
			want: ruleNeverStarted,
		},
		{
			name: "one failure has a log",
			jobs: []forge.Job{{Name: "secrets", Conclusion: "failure", NeverStarted: true},
				{Name: "lint + test", Conclusion: "failure"}},
			want: "",
		},
		{
			name: "only skipped jobs never started",
			jobs: []forge.Job{{Name: "pages", Conclusion: "skipped"}, {Name: "lint + test", Conclusion: "success"}},
			want: "",
		},
		{
			name: "a cancelled job says nothing about runners",
			jobs: []forge.Job{{Name: "lint + test", Conclusion: "cancelled", NeverStarted: true}},
			want: "",
		},
		{
			name: "no jobs reported",
			jobs: nil,
			want: "",
		},
		{
			name: "the forge cannot say",
			jobs: []forge.Job{{Name: "lint + test", Conclusion: "failure", NeverStarted: true}},
			err:  errForge,
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := newWorld(t)
			w.OnJobs(func(string) ([]forge.Job, error) { return tt.jobs, tt.err })
			if got := infraFailure(t.Context(), w, repo, "train-1-on-base0"); got != tt.want {
				t.Fatalf("infraFailure = %q, want %q", got, tt.want)
			}
		})
	}
}

// A forge without per-job detail is read as reporting real failures, whatever
// the combined status says. Wrapping the forge in its own interface value
// keeps CommitJobs off it, which is what such a forge looks like to the train.
func TestInfraFailureWithoutJobDetail(t *testing.T) {
	w := newWorld(t)
	w.SetJobs(repo, "train-1-on-base0", forge.Job{Name: "lint + test", Conclusion: "failure", NeverStarted: true})
	plain := struct{ forge.Forge }{w}
	if _, ok := any(plain).(forge.JobReporter); ok {
		t.Fatal("the wrapper reports per-job detail; it must not")
	}
	if got := infraFailure(t.Context(), plain, repo, "train-1-on-base0"); got != "" {
		t.Fatalf("infraFailure = %q, want no rule on a forge with no per-job detail", got)
	}
}

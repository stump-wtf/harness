package mergetrain

// Batching Tests
//
// Governing tests: SPEC-0025 REQ-17 — a green batch merges all its PRs in
// order on one CI run; a red batch of four bisects, lands the green first
// half, comments the PR bisected to as the red one, and retries the rest on
// the next tick against the new base; a build conflict is attributed to the
// first rejecting head and the batch shrinks to the heads before it. With
// batch unset or 1 the whole preceding suite is the regression test: it runs
// as written on the batched driver.

import (
	"slices"
	"strings"
	"testing"
	"time"
)

func TestBatchGreenMergesAllInOrder(t *testing.T) {
	w := newWorld(t, prAt(1, time.Minute), prAt(2, 2*time.Minute), prAt(3, 3*time.Minute))
	cfg := driverCfg(t, ModeMerge, nil)
	cfg.Batch = 3
	d := newDriver(t, w, cfg)

	ticks(t, d, 1)

	if got := mergedOrder(w.Fake); !slices.Equal(got, []int{1, 2, 3}) {
		t.Fatalf("merged %v, want 1,2,3 in order", got)
	}
	if n := strings.Count(strings.Join(w.Methods(), " "), "CreateTrainBranch"); n != 1 {
		t.Fatalf("CreateTrainBranch called %d times, want one build for the batch", n)
	}
	for _, n := range []int{1, 2, 3} {
		if len(w.Comments(repo, n)) != 0 {
			t.Fatalf("#%d was commented on: %q", n, w.Comments(repo, n))
		}
	}
	noTrainBranches(t, w.Fake)
}

func TestBatchRedBisectsLandsGreenHalfAndCommentsTheRedOne(t *testing.T) {
	w := newWorld(t,
		prAt(1, time.Minute), prAt(2, 2*time.Minute), prAt(3, 3*time.Minute), prAt(4, 4*time.Minute))
	w.setRed(3, true) // the second half is red because PR 3 is
	cfg := driverCfg(t, ModeMerge, nil)
	cfg.Batch = 4
	d := newDriver(t, w, cfg)

	ticks(t, d, 1)

	if got := mergedOrder(w.Fake); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("after the red tick merged %v, want the green first half 1,2", got)
	}
	if w.Merged(repo, 3) {
		t.Fatal("the red PR merged")
	}
	if cs := w.Comments(repo, 3); markers(cs) != 1 || !strings.Contains(cs[0], "cause="+CauseRed) {
		t.Fatalf("PR 3 comments = %q, want exactly one red marker", cs)
	}
	if w.Merged(repo, 4) || markers(w.Comments(repo, 4)) != 0 {
		t.Fatal("PR 4 must be untouched: it is retried against the new base")
	}
	noTrainBranches(t, w.Fake)

	ticks(t, d, 1)
	if got := mergedOrder(w.Fake); !slices.Equal(got, []int{1, 2, 4}) {
		t.Fatalf("after the retry merged %v, want 4 on the new base", got)
	}
	if markers(w.Comments(repo, 3)) != 1 {
		t.Fatalf("PR 3 got a second comment: %q", w.Comments(repo, 3))
	}
	noTrainBranches(t, w.Fake)
}

func TestBatchConflictAttributesFirstRejectingHead(t *testing.T) {
	w := newWorld(t, prAt(1, time.Minute), prAt(2, 2*time.Minute), prAt(3, 3*time.Minute))
	w.conflictOn(2)
	cfg := driverCfg(t, ModeMerge, nil)
	cfg.Batch = 3
	d := newDriver(t, w, cfg)

	ticks(t, d, 1)

	if got := mergedOrder(w.Fake); !slices.Equal(got, []int{1}) {
		t.Fatalf("merged %v, want the heads before the rejecting one", got)
	}
	if cs := w.Comments(repo, 2); markers(cs) != 1 || !strings.Contains(cs[0], "cause="+CauseConflict) {
		t.Fatalf("PR 2 comments = %q, want exactly one conflict marker", cs)
	}
	if w.Merged(repo, 3) || markers(w.Comments(repo, 3)) != 0 {
		t.Fatal("PR 3 must wait for the next tick")
	}
	noTrainBranches(t, w.Fake)

	// PR 2 stays conflicting; its marker keeps it to one comment, and the
	// train gets past it to land PR 3.
	ticks(t, d, 2)
	if got := mergedOrder(w.Fake); !slices.Equal(got, []int{1, 3}) {
		t.Fatalf("merged %v, want PR 3 landed despite PR 2", got)
	}
	if markers(w.Comments(repo, 2)) != 1 {
		t.Fatalf("PR 2 got a second comment: %q", w.Comments(repo, 2))
	}
	noTrainBranches(t, w.Fake)
}

func TestBatchRedOfTwoSplitsToSingletons(t *testing.T) {
	w := newWorld(t, prAt(1, time.Minute), prAt(2, 2*time.Minute))
	w.setRed(1, true)
	cfg := driverCfg(t, ModeMerge, nil)
	cfg.Batch = 2
	d := newDriver(t, w, cfg)

	ticks(t, d, 1)

	if w.Merged(repo, 1) {
		t.Fatal("the red PR merged")
	}
	if cs := w.Comments(repo, 1); markers(cs) != 1 || !strings.Contains(cs[0], "cause="+CauseRed) {
		t.Fatalf("PR 1 comments = %q, want exactly one red marker", cs)
	}
	if w.Merged(repo, 2) {
		t.Fatal("PR 2 was tested alone only after PR 1 was ruled out; it waits for the next tick")
	}

	ticks(t, d, 1)
	if !w.Merged(repo, 2) {
		t.Fatal("PR 2 should land on the next tick")
	}
	noTrainBranches(t, w.Fake)
}

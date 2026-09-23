package supervisor

// Skip-coalescing tests.
//
// Every assertion is against the PERSISTED run history — m.Runs, and for one
// test the reloaded state.json — never against the in-memory decision the
// caller got back. A burst that coalesced correctly in the decision path and
// wrote 199 records anyway would pass the second and fail the first, and the
// records are what flush `keep_runs` and take the real history with them.
//
// Governing: ADR-0021; SPEC-0014 REQ "Overlap Skip Coalescing", REQ "Run
// Record Fields"; amends SPEC-0008 REQ "Overlap Policy".
//
// @joestump 09/22/2026 - Introduced with SPEC-0014 firing fan-out (#457).

import (
	"os"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// coalesceSweep is a long-running scheduled harness with room in its history
// for every record a burst could produce — so a test that failed to coalesce
// would show 199 records rather than silently pruning them away.
func coalesceSweep(name string) core.Harness {
	h := sweep(name, "sleep 30")
	h.KeepRuns = 500
	return h
}

func skippedRecords(rs []RunRecord) []RunRecord {
	var out []RunRecord
	for _, r := range rs {
		if r.Outcome == OutcomeSkipped {
			out = append(out, r)
		}
	}
	return out
}

// TestBurstDuringOneRunCoalesces is REQ "Overlap Skip Coalescing"'s headline
// scenario, at its stated size: 200 firings during one run leave one held
// firing and exactly one skipped record with coalesced = 199.
func TestBurstDuringOneRunCoalesces(t *testing.T) {
	e := newRunsEnv(t)
	h := coalesceSweep("busy")
	h.OnOverlap = core.OverlapQueue
	m, closeM := e.manager(t, sweepCfg(h), fastPolicy())

	req := RunRequest{Trigger: TriggerWebhook, Source: "webhook.gh"}
	m.StartRun("busy", req)
	waitRuns(t, m, "busy", "the run is in flight", outcomesAre(OutcomeRunning))

	queued, skipped := 0, 0
	for i := 0; i < 200; i++ {
		d, _ := m.StartRun("busy", req)
		switch d.Kind {
		case DecisionQueued:
			queued++
		case DecisionSkipped:
			skipped++
		}
	}
	if queued != 1 || skipped != 199 {
		t.Fatalf("decisions = %d queued, %d skipped; want 1 and 199", queued, skipped)
	}

	recs := skippedRecords(m.Runs("busy"))
	if len(recs) != 1 {
		t.Fatalf("the history holds %d skipped records, want exactly 1 — 200 firings must not flush keep_runs", len(recs))
	}
	if recs[0].Coalesced != 199 {
		t.Errorf("coalesced = %d, want 199", recs[0].Coalesced)
	}
	if recs[0].Reason != ReasonOverlap {
		t.Errorf("reason = %q, want %q", recs[0].Reason, ReasonOverlap)
	}
	if recs[0].Source != "webhook.gh" || recs[0].Trigger != TriggerWebhook {
		t.Errorf("record = %+v, want the firing's trigger and source", recs[0])
	}

	// And it survives a restart. Asserted by RESTORING a second Manager from
	// the same state rather than by grepping the file: the count is only
	// really durable if it decodes back into a record, and a string match
	// would pass on a field that no longer parses.
	closeM()
	next, _ := e.manager(t, sweepCfg(h), fastPolicy())
	restored := skippedRecords(next.Runs("busy"))
	if len(restored) != 1 || restored[0].Coalesced != 199 || restored[0].Reason != ReasonOverlap {
		t.Errorf("after a restart the skip record is %+v, want one with coalesced 199 and reason overlap", restored)
	}
}

// TestCoalescingEmitsOneEvent covers the lifecycle half: the first skip emits
// job_run_finished, later increments emit nothing. 200 doorbells for one
// harness must not become 200 notifications, which is the entire point.
func TestCoalescingEmitsOneEvent(t *testing.T) {
	e := newRunsEnv(t)
	h := coalesceSweep("busy")
	h.OnOverlap = core.OverlapSkip
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	sub, unsub := m.Events()
	defer unsub()

	req := RunRequest{Trigger: TriggerWebhook, Source: "webhook.gh"}
	m.StartRun("busy", req)
	waitRuns(t, m, "busy", "the run is in flight", outcomesAre(OutcomeRunning))
	for i := 0; i < 20; i++ {
		m.StartRun("busy", req)
	}
	waitRuns(t, m, "busy", "the skip is recorded", func(rs []RunRecord) bool {
		return len(skippedRecords(rs)) == 1
	})

	// Drain what has been published so far and count the finish events for
	// skipped records.
	finished := 0
	deadline := time.After(500 * time.Millisecond)
drain:
	for {
		select {
		case ev := <-sub:
			if ev.Kind == EventRunFinished && ev.Run.Outcome == OutcomeSkipped {
				finished++
			}
		case <-deadline:
			break drain
		}
	}
	if finished != 1 {
		t.Errorf("job_run_finished for skipped records = %d, want 1 — later increments emit none", finished)
	}
}

// TestANewRunOpensANewSkipRecord covers the scenario "A new run opens a new
// record". Without it a harness skipping once a day would keep incrementing
// one record forever, and the history would never show WHEN the skips
// happened.
func TestANewRunOpensANewSkipRecord(t *testing.T) {
	e := newRunsEnv(t)
	h := coalesceSweep("busy")
	h.OnOverlap = core.OverlapSkip
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())
	req := RunRequest{Trigger: TriggerWebhook, Source: "webhook.gh"}

	// Run 1, two skips during it.
	m.StartRun("busy", req)
	waitRuns(t, m, "busy", "run 1 in flight", outcomesAre(OutcomeRunning))
	m.StartRun("busy", req)
	m.StartRun("busy", req)
	waitRuns(t, m, "busy", "the first skip record exists", func(rs []RunRecord) bool {
		return len(skippedRecords(rs)) == 1 && skippedRecords(rs)[0].Coalesced == 2
	})

	// End run 1, start run 2, skip again.
	if ok := m.Stop("busy"); !ok {
		t.Fatal("Stop returned false for a known harness")
	}
	waitFor(t, 5*time.Second, "run 1 ends", func() bool {
		for _, r := range m.Runs("busy") {
			if r.Outcome == OutcomeRunning {
				return false
			}
		}
		return true
	})
	m.StartRun("busy", req)
	waitRuns(t, m, "busy", "run 2 in flight", func(rs []RunRecord) bool {
		return rs[len(rs)-1].Outcome == OutcomeRunning
	})
	m.StartRun("busy", req)

	recs := waitRuns(t, m, "busy", "a second skip record opens", func(rs []RunRecord) bool {
		return len(skippedRecords(rs)) == 2
	})
	skips := skippedRecords(recs)
	if skips[0].Coalesced != 2 || skips[1].Coalesced != 1 {
		t.Errorf("coalesced counts = %d and %d, want 2 then 1", skips[0].Coalesced, skips[1].Coalesced)
	}
	if skips[0].RunID == skips[1].RunID {
		t.Error("the second skip incremented the first run's record instead of opening its own")
	}
}

// TestSkipsFromDifferentSourcesDoNotCoalesce: the key is (trigger, source,
// reason), so a burst from one webhook and a burst from another stay
// distinguishable — which is the whole reason an operator reads the history.
func TestSkipsFromDifferentSourcesDoNotCoalesce(t *testing.T) {
	e := newRunsEnv(t)
	h := coalesceSweep("busy")
	h.OnOverlap = core.OverlapSkip
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	m.StartRun("busy", RunRequest{Trigger: TriggerWebhook, Source: "webhook.a"})
	waitRuns(t, m, "busy", "the run is in flight", outcomesAre(OutcomeRunning))
	for i := 0; i < 3; i++ {
		m.StartRun("busy", RunRequest{Trigger: TriggerWebhook, Source: "webhook.a"})
		m.StartRun("busy", RunRequest{Trigger: TriggerWebhook, Source: "webhook.b"})
		m.StartRun("busy", RunRequest{Trigger: TriggerSchedule})
	}

	recs := waitRuns(t, m, "busy", "three distinct skip records", func(rs []RunRecord) bool {
		return len(skippedRecords(rs)) == 3
	})
	bySource := map[string]RunRecord{}
	for _, r := range skippedRecords(recs) {
		bySource[string(r.Trigger)+"/"+r.Source] = r
	}
	for _, key := range []string{"webhook/webhook.a", "webhook/webhook.b", "schedule/"} {
		r, ok := bySource[key]
		if !ok {
			t.Errorf("no skip record for %s; got %v", key, bySource)
			continue
		}
		if r.Coalesced != 3 {
			t.Errorf("%s coalesced = %d, want 3", key, r.Coalesced)
		}
	}
}

// TestConcurrentFiringsStartOneProcessAndHoldOne covers REQ "Concurrency
// Safety"'s scenario: 50 concurrent firings for one harness leave at most one
// process and at most one held firing.
//
// It counts the DECISIONS, which is what the requirement is about, and then
// checks the persisted history agrees — a decision path that answered
// correctly and wrote the wrong records would pass only the first.
func TestConcurrentFiringsStartOneProcessAndHoldOne(t *testing.T) {
	e := newRunsEnv(t)
	h := coalesceSweep("busy")
	h.OnOverlap = core.OverlapQueue
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	var mu sync.Mutex
	counts := map[RunDecisionKind]int{}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			d, ok := m.StartRun("busy", RunRequest{Trigger: TriggerWebhook, Source: "webhook.gh"})
			if !ok {
				t.Error("StartRun returned false for a known harness")
				return
			}
			mu.Lock()
			counts[d.Kind]++
			mu.Unlock()
		}()
	}
	wg.Wait()

	if counts[DecisionStarted] != 1 {
		t.Errorf("started = %d, want exactly 1 process", counts[DecisionStarted])
	}
	if counts[DecisionQueued] != 1 {
		t.Errorf("queued = %d, want at most one held firing", counts[DecisionQueued])
	}
	if got := counts[DecisionStarted] + counts[DecisionQueued] + counts[DecisionSkipped]; got != 50 {
		t.Errorf("decisions total %d, want 50 — a firing was lost", got)
	}

	running := 0
	for _, r := range m.Runs("busy") {
		if r.Outcome == OutcomeRunning {
			running++
		}
	}
	if running != 1 {
		t.Errorf("the history holds %d running records, want 1", running)
	}
	// Reported as a count rather than a dump: a failure here means 48
	// records, and printing all of them buries the one number that matters.
	skips := skippedRecords(m.Runs("busy"))
	switch {
	case len(skips) != 1:
		t.Errorf("the history holds %d skipped records, want exactly 1", len(skips))
	case skips[0].Coalesced != 48:
		t.Errorf("coalesced = %d, want 48", skips[0].Coalesced)
	}
}

// TestCoalescingSurvivesAStateSaveFailure: a skip whose increment landed but
// whose state.json write failed must stay coalesced. CoalesceRun used to
// answer a failed Save with the same error it gives for a record that no
// longer exists, and recordSkip reads that as "the record went away" — so it
// opened a NEW record for a firing the old one had already counted. On a
// state dir that stays unwritable, every other firing did it again: the
// burst came back as one record per two firings, each double-counting one.
func TestCoalescingSurvivesAStateSaveFailure(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions, so the save cannot be made to fail")
	}
	e := newRunsEnv(t)
	h := coalesceSweep("busy")
	h.OnOverlap = core.OverlapSkip
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	req := RunRequest{Trigger: TriggerWebhook, Source: "webhook.gh"}
	m.StartRun("busy", req)
	waitRuns(t, m, "busy", "the run is in flight", outcomesAre(OutcomeRunning))

	// state.json lives directly in e.dir; the run's own log is already open
	// under e.logs, so only Save is affected.
	if err := os.Chmod(e.dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(e.dir, 0o700) })
	if err := m.Save(); err == nil {
		t.Fatal("Save succeeded on a read-only state dir, so this test proves nothing")
	}

	for i := 0; i < 10; i++ {
		m.StartRun("busy", req)
	}
	skips := skippedRecords(m.Runs("busy"))
	switch {
	case len(skips) != 1:
		t.Errorf("the history holds %d skipped records after a failed save, want exactly 1", len(skips))
	case skips[0].Coalesced != 10:
		t.Errorf("coalesced = %d, want 10", skips[0].Coalesced)
	}
}

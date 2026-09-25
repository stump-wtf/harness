package supervisor

// Firing-gate tests: the supervisor's half of operating hours on a triggered
// harness (firing_hours.go).
//
// As in runs_coalesce_test.go, every assertion about a skip is made against
// the PERSISTED history — m.Runs, and a second Manager restored from the same
// state.json — rather than the decision the caller got back, because the
// records are what flush keep_runs. The catch-up assertions count run records
// AND the processes that actually ran (a marker line per spawn), so a record
// written for a run that never started cannot pass for one that did.
//
// Governing: ADR-0021; SPEC-0014 REQ "Operating Hours On Triggered Harnesses",
// REQ "Overlap Skip Coalescing".
//
// @joestump 09/23/2026 - Added for stump.wtf/harness#484.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/core"
)

// gatedTrigger is a webhook-triggered harness whose every spawn appends one
// line to marker, so a test can count the processes that really ran.
func gatedTrigger(name, marker string, catchUp bool) core.Harness {
	h := shHarness(name, "echo ran >> '"+marker+"'", 0)
	h.Restart = core.RestartNo
	h.Triggers = []string{"webhook.gh"}
	h.OnOverlap = core.OverlapQueue
	h.KeepRuns = 500
	h.OperatingHours = "TZ=UTC Mon-Fri 09:00-18:00"
	h.CatchUp = catchUp
	return h
}

// spawns counts the lines the harness's processes wrote to marker.
func spawns(t *testing.T, marker string) int {
	t.Helper()
	b, err := os.ReadFile(marker)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(string(b), "ran\n")
}

// TestOutsideHoursSkipsCoalesceOnDisk is the acceptance criterion at its
// stated size: 50 out-of-hours firings are ONE persisted record, with reason
// outside_hours and coalesced = 50, and no process ran.
func TestOutsideHoursSkipsCoalesceOnDisk(t *testing.T) {
	e := newRunsEnv(t)
	marker := filepath.Join(e.dir, "marker")
	h := gatedTrigger("gated", marker, true)
	m, closeM := e.manager(t, sweepCfg(h), fastPolicy())

	req := RunRequest{Trigger: TriggerWebhook, Source: "webhook.gh"}
	for i := 0; i < 50; i++ {
		d, ok := m.SkipRun("gated", req, ReasonOutsideHours)
		if !ok || d.Kind != DecisionSkipped {
			t.Fatalf("firing %d: SkipRun = %+v, ok=%v", i, d, ok)
		}
	}
	if !m.HoursSkipped("gated") {
		t.Error("HoursSkipped = false after outside_hours skips; the gate pass would never catch up")
	}

	// Restored from state.json by a second daemon, not read off the first:
	// the count is only durable if it decodes back into a record.
	closeM()
	next, _ := e.manager(t, sweepCfg(h), fastPolicy())
	runs := next.Runs("gated")
	if len(runs) != 1 {
		t.Fatalf("persisted history = %+v, want exactly one record for 50 firings", runs)
	}
	r := runs[0]
	if r.Outcome != OutcomeSkipped || r.Reason != ReasonOutsideHours || r.Coalesced != 50 {
		t.Errorf("record = %+v, want skipped / outside_hours / coalesced 50", r)
	}
	if r.Trigger != TriggerWebhook || r.Source != "webhook.gh" {
		t.Errorf("record = %+v, want the firing's trigger and source", r)
	}
	if n := spawns(t, marker); n != 0 {
		t.Errorf("%d processes ran for out-of-hours firings, want 0", n)
	}
	// The restored daemon still owes the catch-up (Manager.seedHoursSkipped):
	// a restart over the weekend must not lose Monday's run.
	if !next.HoursSkipped("gated") {
		t.Error("HoursSkipped = false after a restart; the weekend's catch-up was lost")
	}
}

// TestOpenFiringsCatchesUpExactlyOnce: the first in-hours settle-up after
// skips starts one catch_up run, and a second finds nothing owed.
func TestOpenFiringsCatchesUpExactlyOnce(t *testing.T) {
	e := newRunsEnv(t)
	marker := filepath.Join(e.dir, "marker")
	m, _ := e.manager(t, sweepCfg(gatedTrigger("gated", marker, true)), fastPolicy())

	req := RunRequest{Trigger: TriggerWebhook, Source: "webhook.gh"}
	for i := 0; i < 3; i++ {
		m.SkipRun("gated", req, ReasonOutsideHours)
	}

	d, ok := m.OpenFirings("gated")
	if !ok || d.Kind != DecisionStarted || d.Run.Trigger != TriggerCatchUp {
		t.Fatalf("OpenFirings = %+v, ok=%v; want a started catch_up run", d, ok)
	}
	if d2, _ := m.OpenFirings("gated"); d2.Kind != "" {
		t.Errorf("second OpenFirings = %+v, want nothing: the skips were already settled", d2)
	}
	if m.HoursSkipped("gated") {
		t.Error("HoursSkipped still set after the settle-up")
	}
	runs := waitRuns(t, m, "gated", "the catch-up run finishes", outcomesAre(OutcomeSkipped, OutcomeSuccess))
	if runs[1].Trigger != TriggerCatchUp {
		t.Errorf("run = %+v, want trigger catch_up", runs[1])
	}
	if n := spawns(t, marker); n != 1 {
		t.Errorf("%d processes ran, want exactly one catch-up", n)
	}
}

// TestOpenFiringsWithoutCatchUp: catch_up = false starts nothing, but still
// settles the skips — so the next closed window opens a new record instead of
// incrementing last week's forever.
func TestOpenFiringsWithoutCatchUp(t *testing.T) {
	e := newRunsEnv(t)
	marker := filepath.Join(e.dir, "marker")
	m, _ := e.manager(t, sweepCfg(gatedTrigger("gated", marker, false)), fastPolicy())

	req := RunRequest{Trigger: TriggerWebhook, Source: "webhook.gh"}
	m.SkipRun("gated", req, ReasonOutsideHours)
	m.SkipRun("gated", req, ReasonOutsideHours)

	if d, _ := m.OpenFirings("gated"); d.Kind != "" {
		t.Fatalf("OpenFirings = %+v, want no run with catch_up = false", d)
	}
	if m.HoursSkipped("gated") {
		t.Error("HoursSkipped still set after the settle-up")
	}

	m.SkipRun("gated", req, ReasonOutsideHours)
	runs := m.Runs("gated")
	if len(runs) != 2 || runs[0].Coalesced != 2 || runs[1].Coalesced != 1 {
		t.Errorf("history = %+v, want the old window's record (2) and a new one (1)", runs)
	}
	if n := spawns(t, marker); n != 0 {
		t.Errorf("%d processes ran, want 0", n)
	}
}

// TestSeedNeedsTheNewestRecord: a restart re-derives the owed catch-up only
// when nothing has run since the skip — an outside_hours record followed by a
// run is already caught up.
func TestSeedNeedsTheNewestRecord(t *testing.T) {
	e := newRunsEnv(t)
	marker := filepath.Join(e.dir, "marker")
	h := gatedTrigger("gated", marker, true)
	m, closeM := e.manager(t, sweepCfg(h), fastPolicy())

	m.SkipRun("gated", RunRequest{Trigger: TriggerWebhook, Source: "webhook.gh"}, ReasonOutsideHours)
	m.OpenFirings("gated")
	waitRuns(t, m, "gated", "the catch-up run finishes", outcomesAre(OutcomeSkipped, OutcomeSuccess))

	closeM()
	next, _ := e.manager(t, sweepCfg(h), fastPolicy())
	if next.HoursSkipped("gated") {
		t.Error("HoursSkipped = true after a restart, though the catch-up already ran")
	}
}

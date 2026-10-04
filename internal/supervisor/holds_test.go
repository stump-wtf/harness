package supervisor

// Hold Reason Tests
//
// What SPEC-0021 REQ-14 adds on top of SPEC-0012's hold, asserted against the
// real supervisor and Manager: a hold is a set of reasons and only the last
// one clearing starts the harness, and only through admission; a disabled
// harness stays down when its park clears; a park stops at once even where
// hours would close gracefully; `harness stop` clears every reason; and
// harness_hold_changed fires on each change (REQ-19, event half). The
// SPEC-0012 behaviour itself is pinned by hours_test.go, hours_close_test.go
// and hours_lease_test.go, unchanged but for reading Holds instead of Held.
//
// These tests inject the reasons through Manager.Hold directly, to pin the
// hold machinery on its own; quota_test.go drives real parks through it.
//
// Governing: ADR-0027, SPEC-0021 REQ-14 "Release and hold reasons", REQ-19;
// design.md § "Holds become a reason set".
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#468.
//
// @joestump 10/04/2026 - The Manager now clears quota itself, from its park
// store (stump.wtf/harness#477).

import (
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// The release rule: clearing a reason while another remains starts nothing;
// clearing the last one starts the harness, whichever reason went last.
func TestReleaseStartsOnlyWhenTheLastReasonClears(t *testing.T) {
	h := gated(shHarness("g", loopScript, 0))
	m := newTestManager(t, managerCfg(h))
	m.Start("g")
	waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("g"); return s.State == core.StateRunning })

	m.Hold("g", core.HoldHours, core.HoursShutdownImmediate, time.Time{})
	m.Hold("g", core.HoldQuota, core.HoursShutdownImmediate, time.Time{})
	held, _ := m.Snapshot("g")
	if held.State != core.StateStopped || held.Holds != core.HoldSetOf(core.HoldHours, core.HoldQuota) || !held.Enabled {
		t.Fatalf("after two holds: state=%s holds=%s enabled=%v, want stopped, hours+quota, enabled", held.State, held.Holds, held.Enabled)
	}

	// Hours open first: the park still holds it.
	m.Release("g", core.HoldHours)
	time.Sleep(100 * time.Millisecond) // give a wrong start every chance
	snap, _ := m.Snapshot("g")
	if snap.State != core.StateStopped || snap.Holds != core.HoldSetOf(core.HoldQuota) || snap.LastStarted != held.LastStarted {
		t.Fatalf("after hours cleared under a park: state=%s holds=%s started %v → %v, want stopped, quota, not restarted",
			snap.State, snap.Holds, held.LastStarted, snap.LastStarted)
	}

	// The park clears last, and that release is the one that starts it.
	m.Release("g", core.HoldQuota)
	waitFor(t, 3*time.Second, "running after the last reason cleared", func() bool {
		s, _ := m.Snapshot("g")
		return s.State == core.StateRunning
	})
	if snap, _ := m.Snapshot("g"); !snap.Holds.Empty() || !snap.Enabled {
		t.Errorf("after the last release: holds=%s enabled=%v, want none, enabled", snap.Holds, snap.Enabled)
	}
}

// SPEC-0021 REQ-14 Scenario "A disabled parked harness": the park expires,
// the quota reason clears, and the harness stays down.
func TestDisabledParkedHarnessStaysDown(t *testing.T) {
	m := newTestManager(t, managerCfg(shHarness("p", loopScript, 0)))
	m.Start("p")
	waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("p"); return s.State == core.StateRunning })
	m.Stop("p")
	stopped := waitStopped(t, m, "p")

	// A park reaches a disabled harness (a quota group parks every member):
	// it is held for quota with its intent untouched.
	m.Hold("p", core.HoldQuota, core.HoursShutdownImmediate, time.Time{})
	parked, _ := m.Snapshot("p")
	if parked.Holds != core.HoldSetOf(core.HoldQuota) || parked.Enabled || parked.State != core.StateStopped {
		t.Fatalf("parked: holds=%s enabled=%v state=%s, want quota, disabled, stopped", parked.Holds, parked.Enabled, parked.State)
	}

	m.Release("p", core.HoldQuota)
	time.Sleep(100 * time.Millisecond) // give a wrong start every chance
	snap, _ := m.Snapshot("p")
	if !snap.Holds.Empty() {
		t.Errorf("holds = %s after the park cleared, want none", snap.Holds)
	}
	if snap.State != core.StateStopped || snap.Enabled || snap.PID != 0 || snap.LastStarted != stopped.LastStarted {
		t.Errorf("after the park cleared: state=%s enabled=%v pid=%d started %v → %v, want it still down",
			snap.State, snap.Enabled, snap.PID, stopped.LastStarted, snap.LastStarted)
	}
	log := waitLogContains(t, m.LogDir(), "p", "hold cleared")
	if !strings.Contains(log, "reason=quota") {
		t.Errorf("the durable log does not say the park cleared: tail:\n%s", tailOf(log, 400))
	}
}

// A park stops a running harness at once, even one whose hours close
// gracefully, and ends a graceful close already in flight (SPEC-0021 REQ-13:
// "stop a running resident harness at once ... with no graceful close").
func TestQuotaHoldStopsAtOnceEvenMidGracefulClose(t *testing.T) {
	m, _, _, logDir := newCloseRig(t, gracefulH(shHarness("g", loopScript, 0), 15*time.Minute))
	m.Start("g")
	waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("g"); return s.State == core.StateRunning })

	// Mid hours close: running, closing, waiting on a turn that never ends.
	m.Hold("g", core.HoldHours, core.HoursShutdownGraceful, closeAt())
	if snap, _ := m.Snapshot("g"); !snap.Closing || snap.State != core.StateRunning {
		t.Fatalf("after the hours hold: closing=%v state=%s, want a graceful close in flight", snap.Closing, snap.State)
	}
	before, _ := m.Snapshot("g")

	// The mode passed is graceful; a park ignores it.
	m.Hold("g", core.HoldQuota, core.HoursShutdownGraceful, closeAt())
	snap := waitStopped(t, m, "g")
	if snap.Closing {
		t.Error("still closing after a park stopped the harness")
	}
	if snap.Holds != core.HoldSetOf(core.HoldHours, core.HoldQuota) || !snap.Enabled || snap.RestartCount != before.RestartCount {
		t.Errorf("after the park: holds=%s enabled=%v restarts %d → %d, want hours+quota, enabled, unchanged",
			snap.Holds, snap.Enabled, before.RestartCount, snap.RestartCount)
	}
	log := waitLogContains(t, logDir, "g", "reason=quota")
	if !strings.Contains(log, "hold_reasons=hours,quota") {
		t.Errorf("the park's held line does not name both reasons: tail:\n%s", tailOf(log, 400))
	}
}

// A failed harness stays failed for every reason (SPEC-0021 REQ-14).
func TestFailedHarnessIsNeverHeld(t *testing.T) {
	p := fastPolicy()
	p.MaxRestarts = 1
	failed := newTestSupervisor(t, shHarness("f", "exit 1", 0), p)
	failed.Start()
	waitState(t, failed, core.StateFailed)
	for _, r := range []core.HoldReason{core.HoldHours, core.HoldQuota, core.HoldBudget} {
		failed.Hold(r, core.HoursShutdownImmediate, time.Time{})
		if snap := failed.Snapshot(); snap.State != core.StateFailed || !snap.Holds.Empty() {
			t.Errorf("hold %s on a failed harness: state=%s holds=%s, want failed, not held", r, snap.State, snap.Holds)
		}
	}
}

// SPEC-0021 REQ-14: the harness whose last reason clears goes back through
// admission, and a reason admission still finds holds it with no start in
// between. The Manager's admission is a pass-through until #470, so the seam
// is driven here with a stand-in that answers what the test sets.
func TestLastReleaseGoesBackThroughAdmission(t *testing.T) {
	var answer atomic.Uint32 // the HoldSet admission answers
	var asked atomic.Int32
	h := gated(shHarness("a", loopScript, 0))
	s := New(h, Options{Policy: fastPolicy(), Bus: NewBus(), LogCfg: LogConfig{Dir: t.TempDir()}, Admit: func(name string) core.HoldSet {
		asked.Add(1)
		if name != "a" {
			t.Errorf("admission asked about %q, want a", name)
		}
		return core.HoldSet(answer.Load())
	}})
	t.Cleanup(s.Shutdown)
	s.Start()
	waitState(t, s, core.StateRunning)
	s.Hold(core.HoldHours, core.HoursShutdownImmediate, time.Time{})
	held := s.Snapshot()

	// Admission refuses for budget: the hours reason clears, budget takes
	// its place, and nothing starts.
	answer.Store(uint32(core.HoldSetOf(core.HoldBudget)))
	s.Release(core.HoldHours)
	time.Sleep(100 * time.Millisecond)
	snap := s.Snapshot()
	if asked.Load() != 1 {
		t.Fatalf("admission asked %d times, want once", asked.Load())
	}
	if snap.State != core.StateStopped || snap.Holds != core.HoldSetOf(core.HoldBudget) || snap.LastStarted != held.LastStarted {
		t.Fatalf("after a refused admission: state=%s holds=%s, started %v → %v, want stopped, budget, not started",
			snap.State, snap.Holds, held.LastStarted, snap.LastStarted)
	}

	// A reason clearing while another remains does not ask admission.
	s.Hold(core.HoldQuota, core.HoursShutdownImmediate, time.Time{})
	s.Release(core.HoldQuota)
	if asked.Load() != 1 {
		t.Errorf("admission asked %d times after a non-final release, want still once", asked.Load())
	}

	// Admission passes: the last release starts it.
	answer.Store(0)
	s.Release(core.HoldBudget)
	waitState(t, s, core.StateRunning)
	if asked.Load() != 2 {
		t.Errorf("admission asked %d times, want twice", asked.Load())
	}
}

// harness stop clears every reason and enabled, and harness_hold_changed
// fires on each change of the set (SPEC-0021 REQ-19, event half), carrying
// `next` only while the hold's clearing instant is known.
func TestStopClearsEveryReasonAndEachChangeIsAnEvent(t *testing.T) {
	h := gatedExpr(t, shHarness("g", loopScript, 0), "Mon 09:00-13:00", core.HoursShutdownImmediate)
	m := newTestManager(t, managerCfg(h))
	events, cancel := m.Events()
	defer cancel()

	m.Start("g")
	waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("g"); return s.State == core.StateRunning })
	m.Hold("g", core.HoldHours, core.HoursShutdownImmediate, time.Time{})
	m.Hold("g", core.HoldQuota, core.HoursShutdownImmediate, time.Time{})
	m.Release("g", core.HoldQuota)
	m.Hold("g", core.HoldBudget, core.HoursShutdownImmediate, time.Time{})
	m.Stop("g")

	snap, _ := m.Snapshot("g")
	if !snap.Holds.Empty() || snap.Enabled || snap.State != core.StateStopped {
		t.Fatalf("after stop: holds=%s enabled=%v state=%s, want none, disabled, stopped", snap.Holds, snap.Enabled, snap.State)
	}

	want := []core.HoldSet{
		core.HoldSetOf(core.HoldHours),
		core.HoldSetOf(core.HoldHours, core.HoldQuota),
		core.HoldSetOf(core.HoldHours),
		core.HoldSetOf(core.HoldHours, core.HoldBudget),
		0,
	}
	var got []Event
	deadline := time.After(3 * time.Second)
	for len(got) < len(want) {
		select {
		case ev := <-events:
			if ev.Kind == EventHoldChanged && ev.Name == "g" {
				got = append(got, ev)
			}
		case <-deadline:
			t.Fatalf("saw %d harness_hold_changed events, want %d: %v", len(got), len(want), got)
		}
	}
	sets := make([]core.HoldSet, len(got))
	for i, ev := range got {
		sets[i] = ev.Holds
		// Hours alone have a known clearing instant (the next open); any
		// reason whose instant the loop does not know makes next unknown.
		if wantNext := ev.Holds == core.HoldSetOf(core.HoldHours); ev.HoldNext.IsZero() == wantNext {
			t.Errorf("event %d (%s): next = %v, want known=%v", i, ev.Holds, ev.HoldNext, wantNext)
		}
	}
	if !slices.Equal(sets, want) {
		t.Errorf("harness_hold_changed sets = %v, want %v", sets, want)
	}
	// No event for a change that did not happen: nothing further is queued.
drain:
	for {
		select {
		case ev := <-events:
			if ev.Kind == EventHoldChanged && ev.Name == "g" {
				t.Errorf("an extra harness_hold_changed: %+v", ev)
			}
		default:
			break drain
		}
	}
}

// HoldsCleared is the gate pass's clearing hook: it reports a non-hours
// reason once that reason's clearer says it has cleared, never hours, and
// never a reason the harness is not held for. The Manager's own quota clearer
// answers from the park store, so a quota hold with no park behind it clears
// at the next tick.
func TestHoldsClearedAsksTheClearers(t *testing.T) {
	until := time.Date(2026, 9, 21, 15, 0, 0, 0, time.UTC)
	var calls atomic.Int32
	dir := t.TempDir()
	cfg := managerCfg(gated(shHarness("parked", loopScript, 0)), shHarness("free", loopScript, 0))
	m := NewManager(cfg, ManagerOptions{
		Policy: fastPolicy(), StatePath: filepath.Join(dir, "state.json"), LogDir: filepath.Join(dir, "logs"),
		HoldClearers: map[core.HoldReason]HoldClearer{
			core.HoldQuota: func(name string, now time.Time) bool {
				calls.Add(1)
				return !now.Before(until) // a park until 15:00
			},
		},
	})
	t.Cleanup(m.Close)
	m.Start("parked")
	m.Hold("parked", core.HoldHours, core.HoursShutdownImmediate, time.Time{})
	m.Hold("parked", core.HoldQuota, core.HoursShutdownImmediate, time.Time{})

	if got := m.HoldsCleared(until.Add(-time.Minute)); len(got) != 0 {
		t.Errorf("14:59: cleared = %v, want nothing", got)
	}
	got := m.HoldsCleared(until)
	if len(got) != 1 || got["parked"] != core.HoldSetOf(core.HoldQuota) {
		t.Errorf("15:00: cleared = %v, want parked: quota only (hours are the pass's own)", got)
	}
	if calls.Load() != 2 {
		t.Errorf("the quota clearer was asked %d times, want twice (only for the harness held for quota)", calls.Load())
	}

	bare := newTestManager(t, managerCfg(shHarness("x", loopScript, 0)))
	bare.Hold("x", core.HoldQuota, core.HoursShutdownImmediate, time.Time{})
	if got := bare.HoldsCleared(until); len(got) != 1 || got["x"] != core.HoldSetOf(core.HoldQuota) {
		t.Errorf("the Manager's own quota clearer, no park in force: cleared = %v, want x: quota", got)
	}
}

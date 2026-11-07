package supervisor

// The Operator Drain Gate Tests
//
// `harness drain` promises four things, and each is asserted here against the
// real supervisor and Manager: no source starts a new run while the gate is
// armed (the funnel's first check, so cron, a channel, a webhook, catch_up and
// an operator trigger are refused the same way); the run already in flight
// finishes untouched, with its own exit code, never a SIGTERM; a firing the
// drain is holding starts on `undrain` and is dropped by `--discard`; and
// `drain --wait` answers non-zero while a drained harness is still running and
// nil once it is idle, which is what makes it usable as a restart gate.
//
// The gate is runtime state, so the last test restarts the Manager over the
// same on-disk state: the daemon that comes back up serves every harness
// again, which is exactly what a packaging script that drained and then
// restarted wants.
//
// Governing: stump.wtf/harness#828; ADR-0062 (proposed); SPEC-0021 REQ-4
// "Admission", REQ-14 "Release and hold reasons".
//
// @joestump-agent 11/06/2026 - Added for stump.wtf/harness#828.

import (
	"errors"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// TestDrainGateRefusesEverySource: with the gate armed, a firing from every
// source is refused at admission and no process starts.
func TestDrainGateRefusesEverySource(t *testing.T) {
	e := newRunsEnv(t)
	m, _ := e.manager(t, sweepCfg(sweep("gated", "sleep 0.2")), fastPolicy())
	waitDrained(t, m, "gated")

	for _, trig := range []RunTrigger{TriggerSchedule, TriggerCatchUp, TriggerChannel, TriggerWebhook, TriggerManual} {
		d, ok := m.StartRun("gated", RunRequest{Trigger: trig})
		if !ok {
			t.Fatalf("StartRun(%s): unknown harness", trig)
		}
		if d.Kind != DecisionSkipped {
			t.Errorf("firing from %s while drained: decision %q, want skipped", trig, d.Kind)
		}
	}
	if snap := drainSnapshot(t, m, "gated"); snapUp(snap.State) {
		t.Errorf("a drained harness started a process: state %s", snap.State)
	}
	rs := m.Runs("gated")
	if len(rs) != 5 {
		t.Fatalf("run records = %d, want one skip per source: %+v", len(rs), rs)
	}
	for _, r := range rs {
		if r.Outcome != OutcomeSkipped || r.Reason != ReasonDraining {
			t.Errorf("skip record = %+v, want skipped with reason draining", r)
		}
	}
}

// TestDrainLeavesTheRunInFlightAlone: draining a running harness neither stops
// it nor signals it. The run ends on its own terms, with its own exit code, and
// the label moves from draining to drained.
func TestDrainLeavesTheRunInFlightAlone(t *testing.T) {
	e := newRunsEnv(t)
	m, _ := e.manager(t, sweepCfg(sweep("slow", "sleep 0.5")), fastPolicy())
	m.StartRun("slow", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "slow", "run in flight", outcomesAre(OutcomeRunning))

	if err := m.Drain([]string{"slow"}, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "the label reads draining", func() bool {
		return DrainLabel(drainSnapshot(t, m, "slow")) == DrainDraining
	})

	rs := waitRuns(t, m, "slow", "the in-flight run finishes on its own", outcomesAre(OutcomeSuccess))
	if rs[0].ExitCode == nil || *rs[0].ExitCode != 0 {
		t.Errorf("drained run's record = %+v, want exit code 0: nothing may signal it", rs[0])
	}
	if label := DrainLabel(drainSnapshot(t, m, "slow")); label != DrainDrained {
		t.Errorf("label after the run ended = %q, want drained", label)
	}
}

// TestDrainHoldsTheQueuedFiringAndUndrainStartsIt: the firing an in-flight run
// earned under on_overlap = "queue" is not run through admission while the
// drain holds it (that would only record a skip and lose it). It waits, and
// `undrain` starts it like any other firing.
func TestDrainHoldsTheQueuedFiringAndUndrainStartsIt(t *testing.T) {
	e := newRunsEnv(t)
	h := sweep("held", "sleep 0.4")
	h.OnOverlap = core.OverlapQueue
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	m.StartRun("held", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "held", "run in flight", outcomesAre(OutcomeRunning))
	if d, _ := m.StartRun("held", RunRequest{Trigger: TriggerCatchUp}); d.Kind != DecisionQueued {
		t.Fatalf("second firing = %q, want queued", d.Kind)
	}
	if err := m.Drain([]string{"held"}, false); err != nil {
		t.Fatal(err)
	}

	waitRuns(t, m, "held", "the in-flight run finishes", outcomesAre(OutcomeSuccess))
	waitFor(t, 3*time.Second, "drained and idle", func() bool {
		return DrainLabel(drainSnapshot(t, m, "held")) == DrainDrained
	})
	time.Sleep(200 * time.Millisecond)
	if rs := m.Runs("held"); len(rs) != 1 {
		t.Fatalf("the drain started the firing it was holding: %+v", rs)
	}

	if err := m.Undrain([]string{"held"}); err != nil {
		t.Fatal(err)
	}
	rs := waitRuns(t, m, "held", "undrain starts the firing the drain held",
		outcomesAre(OutcomeSuccess, OutcomeSuccess))
	if rs[1].Trigger != TriggerCatchUp {
		t.Errorf("undrain started the wrong firing: %+v", rs[1])
	}
	if holds := drainSnapshot(t, m, "held").Holds; !holds.Empty() {
		t.Errorf("after undrain: holds = %s, want none", holds)
	}
}

// TestDrainDiscardDropsTheQueuedFiring: --discard drops the held firing once,
// recorded cancelled with reason draining, and it never starts.
func TestDrainDiscardDropsTheQueuedFiring(t *testing.T) {
	e := newRunsEnv(t)
	h := sweep("dropped", "sleep 0.4")
	h.OnOverlap = core.OverlapQueue
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	m.StartRun("dropped", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "dropped", "run in flight", outcomesAre(OutcomeRunning))
	if d, _ := m.StartRun("dropped", RunRequest{Trigger: TriggerCatchUp}); d.Kind != DecisionQueued {
		t.Fatalf("second firing = %q, want queued", d.Kind)
	}
	if err := m.Drain([]string{"dropped"}, true); err != nil {
		t.Fatal(err)
	}

	waitFor(t, 3*time.Second, "the held firing is dropped", func() bool {
		for _, r := range m.Runs("dropped") {
			if r.Outcome == OutcomeCancelled && r.Reason == ReasonDraining {
				return true
			}
		}
		return false
	})
	waitRuns(t, m, "dropped", "the in-flight run finishes", outcomesAre(OutcomeSuccess, OutcomeCancelled))
	m.Undrain([]string{"dropped"})
	time.Sleep(300 * time.Millisecond)
	if rs := m.Runs("dropped"); len(rs) != 2 {
		t.Errorf("undrain started a firing the discard dropped: %+v", rs)
	}
}

// TestDrainWaitGatesOnIdleHarnesses: the wait is a machine-readable verdict.
// While a drained harness still runs it times out; once every drained harness
// is idle it returns nil, so `drain --wait && restart` is a gate and not a hope.
func TestDrainWaitGatesOnIdleHarnesses(t *testing.T) {
	tick := drainWaitTick
	drainWaitTick = 5 * time.Millisecond
	t.Cleanup(func() { drainWaitTick = tick })

	e := newRunsEnv(t)
	m, _ := e.manager(t, sweepCfg(sweep("w", "sleep 0.6")), fastPolicy())
	m.StartRun("w", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "w", "run in flight", outcomesAre(OutcomeRunning))
	if err := m.Drain([]string{"w"}, false); err != nil {
		t.Fatal(err)
	}

	if err := m.DrainWait(80 * time.Millisecond); !errors.Is(err, ErrDrainTimeout) {
		t.Fatalf("DrainWait with a run in flight = %v, want ErrDrainTimeout", err)
	}
	waitFor(t, 5*time.Second, "drained and idle", func() bool {
		return DrainLabel(drainSnapshot(t, m, "w")) == DrainDrained
	})
	if err := m.DrainWait(2 * time.Second); err != nil {
		t.Fatalf("DrainWait once the run ended = %v, want nil", err)
	}
	if err := m.DrainWait(50 * time.Millisecond); err != nil {
		t.Errorf("DrainWait with nothing draining = %v, want nil", err)
	}
}

// TestDrainKeepsAResidentUp: a resident's process is not a queued firing. The
// gate only refuses the next start, so the harness stays up through the drain
// and stays on the same process across undrain.
func TestDrainKeepsAResidentUp(t *testing.T) {
	m := newTestManager(t, managerCfg(gated(shHarness("res", loopScript, 0))))
	m.Start("res")
	waitFor(t, 3*time.Second, "running", func() bool {
		return drainSnapshot(t, m, "res").State == core.StateRunning
	})
	before := drainSnapshot(t, m, "res").LastStarted

	if err := m.Drain([]string{"res"}, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "draining", func() bool {
		return DrainLabel(drainSnapshot(t, m, "res")) == DrainDraining
	})
	time.Sleep(200 * time.Millisecond)
	snap := drainSnapshot(t, m, "res")
	if !snapUp(snap.State) || snap.LastStarted != before {
		t.Fatalf("drain stopped the resident: state %s started %v (was %v)", snap.State, snap.LastStarted, before)
	}

	if err := m.Undrain([]string{"res"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "the drain cleared", func() bool {
		return drainSnapshot(t, m, "res").Holds.Empty()
	})
	if snap := drainSnapshot(t, m, "res"); snap.LastStarted != before {
		t.Errorf("undrain restarted a resident that never went down: started %v, want %v", snap.LastStarted, before)
	}
}

// TestDrainRefusesAnUnknownNameWhole: a typo must not take half the named list
// out of service.
func TestDrainRefusesAnUnknownNameWhole(t *testing.T) {
	e := newRunsEnv(t)
	m, _ := e.manager(t, sweepCfg(sweep("real", "sleep 0.2")), fastPolicy())

	if err := m.Drain([]string{"ghost", "real"}, false); !errors.Is(err, ErrUnknownHarness) {
		t.Fatalf("Drain with an unknown name = %v, want ErrUnknownHarness", err)
	}
	time.Sleep(100 * time.Millisecond)
	if holds := drainSnapshot(t, m, "real").Holds; !holds.Empty() {
		t.Errorf("the known harness was drained although the call was refused whole: holds = %s", holds)
	}
}

// TestDrainIsRuntimeStateOnly: a daemon restart resumes every harness. The wait
// in a packaging script is the gate; the restart is the resume, so a gate that
// survived it would leave the fleet out of service for good.
func TestDrainIsRuntimeStateOnly(t *testing.T) {
	e := newRunsEnv(t)
	m, closeManager := e.manager(t, sweepCfg(sweep("p", "sleep 0.2")), fastPolicy())
	waitDrained(t, m, "p")
	closeManager()

	restarted, _ := e.manager(t, sweepCfg(sweep("p", "sleep 0.2")), fastPolicy())
	if holds := drainSnapshot(t, restarted, "p").Holds; !holds.Empty() {
		t.Errorf("a restart kept the drain gate: holds = %s, want none", holds)
	}
}

// waitDrained drains name and waits until the gate is visibly armed.
func waitDrained(t *testing.T, m *Manager, name string) {
	t.Helper()
	if err := m.Drain([]string{name}, false); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 3*time.Second, "the drain gate is armed", func() bool {
		return drainSnapshot(t, m, name).Holds.Has(core.HoldDrain)
	})
}

// drainSnapshot is m.Snapshot with the unknown harness checked once.
func drainSnapshot(t *testing.T, m *Manager, name string) Snapshot {
	t.Helper()
	snap, ok := m.Snapshot(name)
	if !ok {
		t.Fatalf("unknown harness %q", name)
	}
	return snap
}

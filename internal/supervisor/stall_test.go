package supervisor

// Governing: ADR-0045; issue #959. This is the only path in the supervisor that
// ends a resident process the watchdog found wedged, so the test covers the
// whole claim: the worker is replaced, the run record says `stalled`, a restart
// policy of "no" does not suppress the revive, and a finding about a process
// that has already been replaced kills nothing.

import (
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

func TestStallRestartReplacesAWedgedWorkerDespiteRestartNo(t *testing.T) {
	e := newRunsEnv(t)
	h := shHarness("wedged", "sleep 60", 0)
	h.Restart = core.RestartNo
	h.StallTimeout = 200 * time.Millisecond
	h.StallAction = core.StallRestart
	m, _ := e.manager(t, managerCfg(h), fastPolicy())
	m.Start("wedged")

	var first Snapshot
	waitFor(t, 5*time.Second, "running", func() bool {
		s, ok := m.Snapshot("wedged")
		if !ok || s.State != core.StateRunning {
			return false
		}
		first = s
		return true
	})

	if !m.ReportStall("wedged", first.LastStarted) {
		t.Fatal("the manager refused a finding about a live harness")
	}
	waitFor(t, 5*time.Second, "the wedged worker replaced", func() bool {
		s, _ := m.Snapshot("wedged")
		return s.State == core.StateRunning && s.PID != first.PID
	})

	rec := waitClosed(t, e, "wedged", 1, 5*time.Second)[0]
	if rec["outcome"] != string(OutcomeStalled) {
		t.Errorf("record = %v, want outcome %s", rec, OutcomeStalled)
	}
	if rec["kind"] != "resident" {
		t.Errorf("record = %v, want a resident run", rec)
	}
	if rec["trigger"] != "manual" {
		t.Errorf("record = %v, want the run the operator started", rec)
	}

	// The revive happened although `restart = "no"`: asking for
	// `stall_action = "restart"` is asking to be revived.
	second, _ := m.Snapshot("wedged")
	if second.State != core.StateRunning {
		t.Fatalf("state = %s after the stall, want the worker running again", second.State)
	}

	// A finding sampled against the process that has since been replaced is
	// dropped, not delivered to the worker that came up behind it.
	if !m.ReportStall("wedged", first.LastStarted) {
		t.Fatal("the manager refused a finding about the current process")
	}
	settle := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(settle) {
		s, _ := m.Snapshot("wedged")
		if s.PID != second.PID {
			t.Fatalf("a stale stall finding killed the replacement worker (pid %d -> %d)", second.PID, s.PID)
		}
		time.Sleep(5 * time.Millisecond)
	}
	m.Stop("wedged")
}

// TestStallFindingOnADownHarnessIsHarmless: the guard's clock runs on its own
// goroutine, so a finding can land after the worker has already exited. It must
// not resurrect anything or close a run twice.
func TestStallFindingOnADownHarnessIsHarmless(t *testing.T) {
	e := newRunsEnv(t)
	h := shHarness("down", "sleep 60", 0)
	h.StallTimeout = time.Minute
	h.StallAction = core.StallRestart
	m, _ := e.manager(t, managerCfg(h), fastPolicy())
	m.Start("down")
	snap, _ := m.Snapshot("down")
	m.Stop("down")
	waitFor(t, 5*time.Second, "stopped", func() bool {
		s, _ := m.Snapshot("down")
		return s.State == core.StateStopped
	})

	m.ReportStall("down", snap.LastStarted)
	m.ReportStall("no-such-harness", snap.LastStarted)

	settle := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(settle) {
		if s, _ := m.Snapshot("down"); s.State != core.StateStopped {
			t.Fatalf("a stall finding started a stopped harness: %v", s.State)
		}
		time.Sleep(5 * time.Millisecond)
	}
	if n := len(waitClosed(t, e, "down", 1, 2*time.Second)); n != 1 {
		t.Fatalf("closed records = %d, want the one run it lived through", n)
	}
}

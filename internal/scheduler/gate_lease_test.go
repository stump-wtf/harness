package scheduler

// Gate Pass Against The Real Manager: Leases And The Close's First Tick
//
// The gate decides on the scheduler tick's clock and nothing else (design.md
// § "A pure internal/hours package"). These tests drive the real Manager — the
// object the daemon wires in as its Gate — from the fake clock, with a
// persisted lease restored from a real state.json, so a lease judged on any
// other clock shows up as a wrong decision rather than hiding behind a
// fixture. The fake timeline is set in 2030 on purpose: a wall-clock read
// inside the Gate sees a lease that is years from ending and hours that have
// nothing to do with the test's, which is exactly the disagreement these
// tests exist to catch.
//
// Governing: ADR-0019, SPEC-0012 REQ "After-Hours Lease", REQ "Gate
// Evaluation", REQ "Graceful Shutdown".
//
// @joestump-agent 09/22/2026 - Added: the pass never asked for the lease in
// hours, asked on a wall clock, and left a close's first decision to the
// next tick.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/hours"
	"github.com/stump-wtf/harness/internal/runtrace"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// busyBridge is a turn-state watch whose agent is always mid-activity: an
// event is always arriving and the reader reports no turn boundaries, so a
// close can only end at its cap. That isolates the deadline — and so the
// instant it is anchored to — as the thing under test.
type busyBridge struct{ countingBridge }

func (b *busyBridge) Turn(string) (runtrace.TurnState, bool) {
	return runtrace.TurnState{LastEventAt: time.Date(2099, 1, 1, 0, 0, 0, 0, time.UTC)}, true
}

// mon30 is Monday 2030-09-23 at hh:mm UTC.
func mon30(hh, mm int) time.Time { return time.Date(2030, 9, 23, hh, mm, 0, 0, time.UTC) }

// realGate builds a Manager over a state.json that records name as enabled
// with the given lease end (zero: no lease), restores and autostarts it the way
// the daemon boots, and puts a fake-clock scheduler in front of it.
func realGate(t *testing.T, expr string, mode core.HoursShutdownMode, lease time.Time, bridge supervisor.TurnBridge) (*rig, *supervisor.Manager, string) {
	t.Helper()
	e, err := hours.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	h := core.Harness{
		Name:                 "w",
		Adapter:              "generic",
		Args:                 []string{"-c", "while true; do sleep 0.02; done"},
		Backend:              core.BackendNative,
		OperatingHours:       expr,
		HoursExpr:            e,
		HoursShutdown:        mode,
		HoursShutdownTimeout: 15 * time.Minute,
	}
	cfg := &core.Config{Harnesses: map[string]core.Harness{"w": h}, HarnessOrder: []string{"w"}}
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	rec := map[string]any{"enabled": true, "state": "stopped"}
	if !lease.IsZero() {
		rec["lease_until"] = lease
	}
	doc, _ := json.Marshal(map[string]any{"version": 1, "harnesses": map[string]any{"w": rec}})
	if err := os.WriteFile(statePath, doc, 0o600); err != nil {
		t.Fatal(err)
	}
	logDir := filepath.Join(dir, "logs")
	m := supervisor.NewManager(cfg, supervisor.ManagerOptions{StatePath: statePath, LogDir: logDir, Watch: bridge})
	t.Cleanup(m.Close)
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	m.Autostart()
	if lease.IsZero() {
		m.Start("w") // no lease: bring it up in hours by hand
	}
	waitRunning(t, m)

	r := &rig{clock: newFakeClock(mon30(0, 0)), store: newMemStore()}
	r.s = New(Options{Start: r.start, Clock: r.clock, Location: time.UTC, Store: r.store, Recorder: r, Gate: gateManager{m}})
	t.Cleanup(r.s.Close)
	r.s.Apply(cfg)
	return r, m, logDir
}

func waitRunning(t *testing.T, m *supervisor.Manager) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if snap, _ := m.Snapshot("w"); snap.State == core.StateRunning {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	snap, _ := m.Snapshot("w")
	t.Fatalf("never running: state=%s", snap.State)
}

// waitSnap polls until cond holds for w's snapshot — the stop a hold runs is
// synchronous, but the snapshot the test reads can trail its publish.
func waitSnap(t *testing.T, m *supervisor.Manager, desc string, cond func(supervisor.Snapshot) bool) supervisor.Snapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if snap, _ := m.Snapshot("w"); cond(snap) {
			return snap
		}
		time.Sleep(5 * time.Millisecond)
	}
	snap, _ := m.Snapshot("w")
	t.Fatalf("timed out waiting for %s: state=%s held=%v closing=%v", desc, snap.State, snap.Held, snap.Closing)
	return snap
}

// SPEC-0012 Scenario "Lease runs into hours", with a window shorter than the
// lease: a lease 08:30-09:30 under a 09:00-09:10 window is discarded when the
// window opens, so the harness closes with the window at 09:10 — not at the
// lease's own end. The pass used to ask for the lease only out of hours, so
// the discard never ran and the lease outlived the window that opened under
// it.
func TestGateDiscardsALeaseWhenItsHoursOpen(t *testing.T) {
	r, m, _ := realGate(t, "TZ=UTC Mon 09:00-09:10", core.HoursShutdownImmediate, mon30(9, 30), &countingBridge{})

	r.at(mon30(8, 59)) // out of hours, leased: left alone
	if snap, _ := m.Snapshot("w"); snap.State != core.StateRunning || snap.Held {
		t.Fatalf("08:59 under a lease: state=%s held=%v, want running", snap.State, snap.Held)
	}
	r.at(mon30(9, 0)) // hours open: the lease is discarded
	r.at(mon30(9, 5))
	r.at(mon30(9, 10)) // the window closes, and no lease covers it any more
	waitSnap(t, m, "held at the window's close", func(s supervisor.Snapshot) bool {
		return s.State == core.StateStopped && s.Held && s.Enabled
	})
	// Asked out of hours and before 09:30, a surviving lease would answer
	// live here; the discard at 09:00 is why it does not.
	if until, ok := m.LeaseAt("w", mon30(9, 10)); ok {
		t.Fatalf("the lease (until %v) outlived the window that opened under it", until)
	}
}

// SPEC-0012 Scenario "Restart mid-lease" through the gate, with a graceful
// close: a restored lease ending 21:00 is enforced at 21:00, not later, and the
// close's deadline is measured from the lease's end (SPEC-0012 REQ "Graceful
// Shutdown": "the window's end, or the lease's end") — 21:15, not the
// window's 13:15. Judged on any clock but the tick's, the lease looks live to
// the Manager and spent to the pass, the Manager never records its end, and
// the close is anchored to 13:00 and stopped on the spot.
func TestGateEndsALeaseOnTheTicksClock(t *testing.T) {
	r, m, logDir := realGate(t, "TZ=UTC Mon 09:00-13:00", core.HoursShutdownGraceful, mon30(21, 0), &busyBridge{})

	r.at(mon30(20, 30)) // booted mid-lease: running, not held
	if snap, _ := m.Snapshot("w"); snap.State != core.StateRunning || snap.Held {
		t.Fatalf("20:30 under a restored lease: state=%s held=%v, want running", snap.State, snap.Held)
	}
	r.at(mon30(21, 0)) // the lease ends at 21:00 exactly
	snap := waitSnap(t, m, "closing at the lease's end", func(s supervisor.Snapshot) bool { return s.Closing })
	if !snap.CloseAt.Equal(mon30(21, 0)) {
		t.Fatalf("close anchored at %v, want the lease's end %v", snap.CloseAt, mon30(21, 0))
	}
	if snap.State != core.StateRunning {
		t.Fatalf("stopped at 21:00 with its cap at 21:15: state=%s", snap.State)
	}
	r.at(mon30(21, 14))
	if s, _ := m.Snapshot("w"); s.State != core.StateRunning {
		t.Fatalf("stopped before the lease-anchored cap: state=%s", s.State)
	}
	r.at(mon30(21, 15))
	waitSnap(t, m, "stopped at the cap", func(s supervisor.Snapshot) bool {
		return s.State == core.StateStopped && s.Held && s.Enabled
	})
	data, err := os.ReadFile(filepath.Join(logDir, "w.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "deadline reached") {
		t.Errorf("durable log does not record the cap ending the close:\n%s", data)
	}
}

// SPEC-0012 Scenario "Sleeping through a close": running at 12:50, the host
// suspends and resumes at 15:00, and the first tick after resume finds the
// deadline (13:15) past and stops the harness at once — on that tick, not the
// one after.
func TestGateStopsOnTheFirstTickPastTheDeadline(t *testing.T) {
	r, m, logDir := realGate(t, "TZ=UTC Mon 09:00-13:00", core.HoursShutdownGraceful, time.Time{}, &busyBridge{})

	r.at(mon30(12, 50))
	r.at(mon30(15, 0)) // one tick after the resume
	waitSnap(t, m, "stopped on the first tick past the deadline", func(s supervisor.Snapshot) bool {
		return s.State == core.StateStopped && s.Held && !s.Closing
	})
	data, err := os.ReadFile(filepath.Join(logDir, "w.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "deadline reached") {
		t.Errorf("durable log does not record the cap ending the close:\n%s", data)
	}
}

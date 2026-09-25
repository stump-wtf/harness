package scheduler

// No-Trace-I/O-Unless-Near-A-Close Test
//
// SPEC-0012 REQ "Turn State Signal" binds the turn-state watch to closes: the
// daemon follows a harness only while it is closing, or within the arm lead
// of closing, and does no trace I/O the rest of the day. This test drives the
// real Manager — the object the daemon wires in as its Gate — over the fake
// clock with a counting bridge in place of the watcher, so the assertion is
// the read count itself: zero for hours, one arm per close inside the lead.
//
// Governing: ADR-0019, SPEC-0012 REQ "Turn State Signal", REQ "Graceful
// Shutdown"; issue #384's acceptance criteria ("assert the property, not a
// proxy").
//
// @joestump-agent 09/21/2026 - Added for stump.wtf/harness#384.

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/hours"
	"github.com/stump-wtf/harness/internal/runtrace"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// countingBridge counts the only calls that start or stop trace I/O.
type countingBridge struct {
	mu    sync.Mutex
	armed int
	down  int
}

func (b *countingBridge) Follow(string, runtrace.Scope, runtrace.Window, []runtrace.Scope, time.Time) {
	b.mu.Lock()
	b.armed++
	b.mu.Unlock()
}

func (b *countingBridge) Unfollow(string) {
	b.mu.Lock()
	b.down++
	b.mu.Unlock()
}

func (b *countingBridge) Turn(string) (runtrace.TurnState, bool) { return runtrace.TurnState{}, false }

func (b *countingBridge) Unavailable(string) string { return "nothing attributable" }

func (b *countingBridge) counts() (armed, down int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.armed, b.down
}

// gateManager adapts the Manager to the scheduler's Gate seam exactly as the
// daemon's hoursGate does, hiding Release's bool.
type gateManager struct{ *supervisor.Manager }

func (g gateManager) Status(name string) (up, held, closing, ok bool) {
	return g.GateStatus(name)
}

func (g gateManager) Release(name string) { g.Manager.Release(name) }

func (g gateManager) OpenFirings(name string) { g.Manager.OpenFirings(name) }

func (g gateManager) Lease(name string, now time.Time) (time.Time, bool) {
	return g.LeaseAt(name, now)
}

// TestWatcherDoesNoTraceIOFarFromAClose is the property: hours of ticks far
// from a close arm nothing, and the arm lead arms exactly once.
func TestWatcherDoesNoTraceIOFarFromAClose(t *testing.T) {
	expr, err := hours.Parse("TZ=UTC 09:00-13:00")
	if err != nil {
		t.Fatal(err)
	}
	h := core.Harness{
		Name:                 "w",
		Adapter:              "generic",
		Args:                 []string{"-c", "while true; do sleep 0.02; done"},
		Backend:              core.BackendNative,
		OperatingHours:       "TZ=UTC 09:00-13:00",
		HoursExpr:            expr,
		HoursShutdown:        core.HoursShutdownGraceful,
		HoursShutdownTimeout: 15 * time.Minute,
	}
	cfg := &core.Config{Harnesses: map[string]core.Harness{"w": h}, HarnessOrder: []string{"w"}}
	dir := t.TempDir()
	bridge := &countingBridge{}
	m := supervisor.NewManager(cfg, supervisor.ManagerOptions{
		StatePath: filepath.Join(dir, "state.json"),
		LogDir:    filepath.Join(dir, "logs"),
		Watch:     bridge,
	})
	t.Cleanup(m.Close)
	m.Start("w")
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if snap, _ := m.Snapshot("w"); snap.State == core.StateRunning {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	r := &rig{clock: newFakeClock(mon(12, 0)), store: newMemStore()}
	r.s = New(Options{Start: r.start, Clock: r.clock, Location: time.UTC, Store: r.store, Recorder: r, Gate: gateManager{m}})
	t.Cleanup(r.s.Close)
	r.s.Apply(cfg)

	// Nearly three hours of in-hours ticks, nowhere near the close: the
	// daemon does no trace I/O for this harness.
	r.runUntil(mon(12, 58), time.Second)
	if armed, _ := bridge.counts(); armed != 0 {
		t.Fatalf("the watch was armed %d times for a harness far from any close", armed)
	}

	// Inside the arm lead the watch warms once, and the next tick does not
	// arm again — proving the zero above was the property, not a broken
	// counter.
	r.at(mon(12, 59))
	r.at(mon(12, 59).Add(30 * time.Second))
	armed, down := bridge.counts()
	if armed != 1 {
		t.Errorf("arm count inside the lead = %d, want exactly 1", armed)
	}
	if down != 0 {
		t.Errorf("the watch was torn down %d times before its close", down)
	}
}

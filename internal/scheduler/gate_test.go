package scheduler

// Operating Hours Gate Tests
//
// Every scenario of SPEC-0012 REQ "Gate Evaluation" and the scheduler's half
// of REQ "Gate Enforcement" / REQ "Operating Hours Reload", driven through the
// fake clock: suspend/resume jumps and both DST transitions take microseconds.
// fakeGate models just enough of a supervisor (up / held) for the pass to act
// on, and records every Hold and Release it receives.
//
// Governing: ADR-0019, SPEC-0012 REQ "Gate Evaluation", REQ "Gate
// Enforcement", REQ "Operating Hours Reload".
//
// @joestump-agent 09/21/2026 - Added for stump.wtf/harness#382.

import (
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/hours"
)

type fakeGate struct {
	mu      sync.Mutex
	up      map[string]bool
	held    map[string]bool
	closing map[string]bool
	lease   map[string]time.Time
	// closeAt is what CloseAt answers per name; a name absent from the map
	// is unanchored (ok=false).
	closeAt map[string]time.Time
	calls   []string // "hold name mode" / "clos name" / "arm name" / "release name"
	onHold  func()
}

func newFakeGate() *fakeGate {
	return &fakeGate{
		up: map[string]bool{}, held: map[string]bool{}, closing: map[string]bool{},
		lease: map[string]time.Time{}, closeAt: map[string]time.Time{},
	}
}

func (g *fakeGate) Status(name string) (bool, bool, bool, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.up[name], g.held[name], g.closing[name], true
}

func (g *fakeGate) Lease(name string, _ time.Time) (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.lease[name]
	return t, ok
}

func (g *fakeGate) CloseAt(name string, now time.Time) (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	t, ok := g.closeAt[name]
	return t, ok
}

func (g *fakeGate) Hold(name string, mode core.HoursShutdownMode, closeAt time.Time) {
	g.mu.Lock()
	fn := g.onHold
	g.calls = append(g.calls, "hold "+name+" "+string(mode))
	g.up[name], g.held[name] = false, true
	g.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (g *fakeGate) CloseStep(name string, now time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, "clos "+name)
}

func (g *fakeGate) Arm(name string, closeAt time.Time) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, "arm "+name)
}

func (g *fakeGate) Release(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, "release "+name)
	if g.held[name] {
		g.up[name], g.held[name] = true, false
	}
}

func (g *fakeGate) set(name string, up, held bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.up[name], g.held[name] = up, held
}

func (g *fakeGate) took() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := slices.Clone(g.calls)
	g.calls = nil
	return out
}

// gateRig is a rig whose scheduler drives a fakeGate.
func gateRig(t *testing.T, now time.Time) (*rig, *fakeGate) {
	t.Helper()
	g := newFakeGate()
	r := &rig{clock: newFakeClock(now), store: newMemStore()}
	r.s = New(Options{Start: r.start, Clock: r.clock, Location: time.UTC, Store: r.store, Recorder: r, Gate: g})
	t.Cleanup(r.s.Close)
	return r, g
}

// gatedCfg is a config with one gated harness.
func gatedCfg(t *testing.T, name, expr string, mode core.HoursShutdownMode) *core.Config {
	t.Helper()
	e, err := hours.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	return &core.Config{
		Harnesses:    map[string]core.Harness{name: {Name: name, OperatingHours: expr, HoursExpr: e, HoursShutdown: mode}},
		HarnessOrder: []string{name},
	}
}

func wantCalls(t *testing.T, g *fakeGate, when string, want ...string) {
	t.Helper()
	if got := g.took(); !slices.Equal(got, want) {
		t.Errorf("%s: gate calls = %q, want %q", when, got, want)
	}
}

// 2026-09-21 is a Monday.
func mon(h, m int) time.Time { return time.Date(2026, 9, 21, h, m, 0, 0, time.UTC) }

// SPEC-0012 Scenarios "Close" and "Open", plus "does not bounce a harness that
// remains in hours": nothing happens inside the window, one hold at 13:00,
// nothing while held, one release at 09:00 the next day.
func TestGateClosesAndOpens(t *testing.T) {
	r, g := gateRig(t, mon(12, 58))
	r.s.Apply(gatedCfg(t, "a", "TZ=UTC 09:00-13:00", core.HoursShutdownImmediate))
	g.set("a", true, false)

	r.runUntil(mon(12, 59).Add(-time.Second), time.Second)
	wantCalls(t, g, "inside the window")

	// The window ends within the arm lead: the pass warms the graceful
	// close's turn-state watch once, not once per tick.
	r.at(mon(12, 59))
	wantCalls(t, g, "inside the arm lead", "arm a")

	r.at(mon(13, 0))
	wantCalls(t, g, "at the close", "hold a immediate")

	r.runUntil(mon(13, 5), time.Second)
	wantCalls(t, g, "while held")

	r.at(mon(9, 0).AddDate(0, 0, 1).Add(-time.Second))
	wantCalls(t, g, "a second before the open")
	r.at(mon(9, 0).AddDate(0, 0, 1))
	wantCalls(t, g, "at the open", "release a")
}

// SPEC-0012 Scenario "Sleeping through a close": the first tick after resume
// holds at once, and steps the graceful close on that same tick so a deadline
// already past stops the harness there (TestGateStopsOnTheFirstTickPastTheDeadline
// drives the real Manager through it).
func TestGateSuspendThroughClose(t *testing.T) {
	r, g := gateRig(t, mon(12, 50))
	r.s.Apply(gatedCfg(t, "a", "TZ=UTC 09:00-13:00", core.HoursShutdownGraceful))
	g.set("a", true, false)
	r.tick()
	wantCalls(t, g, "12:50")
	r.at(mon(15, 0)) // resume: one tick, two hours later
	wantCalls(t, g, "first tick after resume", "hold a graceful", "clos a")
}

// SPEC-0012 Scenario "Waking inside a window".
func TestGateWakeInsideWindow(t *testing.T) {
	r, g := gateRig(t, mon(8, 0))
	r.s.Apply(gatedCfg(t, "a", "TZ=UTC 09:00-13:00", core.HoursShutdownImmediate))
	g.set("a", false, true)
	r.tick()
	wantCalls(t, g, "08:00")
	r.at(mon(10, 0))
	wantCalls(t, g, "first tick after resume", "release a")
}

// SPEC-0012 Scenario "Spring forward": in hours at 01:59:59 EST, out at
// 03:00:00 EDT one second later.
func TestGateSpringForward(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	before := time.Date(2026, 3, 8, 1, 59, 59, 0, ny)
	r, g := gateRig(t, before)
	r.s.Apply(gatedCfg(t, "a", "TZ=America/New_York 01:30-02:30", core.HoursShutdownImmediate))
	g.set("a", true, false)
	r.tick()
	// The window ends one second out — inside the arm lead — so the close's
	// watch is warmed here, before the gap.
	wantCalls(t, g, "01:59:59 EST", "arm a")
	after := before.Add(time.Second)
	if after.Hour() != 3 {
		t.Fatalf("test setup: %v is not the spring-forward instant", after)
	}
	r.at(after)
	wantCalls(t, g, "03:00:00 EDT", "hold a immediate")
}

// SPEC-0012 Scenario "Fall back": 01:00-02:00 is in hours during BOTH
// occurrences of 01:00–01:59, and no release/hold churn happens between them.
func TestGateFallBack(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	first := time.Date(2026, 11, 1, 1, 0, 0, 0, ny) // EDT occurrence
	r, g := gateRig(t, first.Add(-time.Second))
	r.s.Apply(gatedCfg(t, "a", "TZ=America/New_York 01:00-02:00", core.HoursShutdownImmediate))
	g.set("a", false, true)
	r.tick()
	wantCalls(t, g, "00:59:59")
	r.at(first)
	wantCalls(t, g, "first 01:00", "release a")
	// Walk through both 01:xx hours (two real hours) a minute at a time.
	end := first.Add(2 * time.Hour)
	for r.clock.Now().Before(end.Add(-time.Minute)) {
		r.clock.advance(time.Minute)
		r.tick()
	}
	wantCalls(t, g, "both 01:00-01:59 occurrences", "arm a") // warmed once, at 01:59
	r.at(end)                                                // 02:00 EST
	wantCalls(t, g, "02:00 EST", "hold a immediate")
}

// SPEC-0012 REQ "Gate Enforcement": a harness covered by a valid lease is not
// held. The daemon's Lease seam always answers "none" until the lease story;
// this pins the pass's side of the contract.
func TestGateLeaseSeamPreventsHold(t *testing.T) {
	r, g := gateRig(t, mon(20, 0))
	r.s.Apply(gatedCfg(t, "a", "TZ=UTC 09:00-13:00", core.HoursShutdownImmediate))
	g.set("a", true, false)
	g.mu.Lock()
	g.lease["a"] = mon(21, 0)
	g.mu.Unlock()
	r.tick()
	wantCalls(t, g, "under a lease")
	r.at(mon(21, 0))
	wantCalls(t, g, "at the lease end", "hold a immediate")
}

// Neither a disabled/failed harness (down, not held) nor one already down is
// touched: the pass holds only what is up and releases only what is held.
func TestGateLeavesDownHarnessesAlone(t *testing.T) {
	r, g := gateRig(t, mon(20, 0))
	r.s.Apply(gatedCfg(t, "a", "TZ=UTC 09:00-13:00", core.HoursShutdownImmediate))
	g.set("a", false, false)
	r.tick()
	r.at(mon(10, 0).AddDate(0, 0, 1))
	wantCalls(t, g, "down and not held")
}

// SPEC-0012 Scenario "Adding hours in the evening": a reload that gates a
// running harness at 20:00 holds it on the next tick.
func TestGateReloadAddsHours(t *testing.T) {
	r, g := gateRig(t, mon(20, 0))
	r.s.Apply(&core.Config{Harnesses: map[string]core.Harness{"a": {Name: "a"}}, HarnessOrder: []string{"a"}})
	g.set("a", true, false)
	r.tick()
	wantCalls(t, g, "ungated")
	r.s.Apply(gatedCfg(t, "a", "TZ=UTC 09:00-13:00", core.HoursShutdownImmediate))
	r.tick()
	wantCalls(t, g, "next tick after reload", "hold a immediate")
}

// SPEC-0012 Scenario "Removing hours": a held harness whose hours a reload
// removes is released on the next tick, exactly once.
func TestGateReloadRemovesHours(t *testing.T) {
	r, g := gateRig(t, mon(20, 0))
	r.s.Apply(gatedCfg(t, "a", "TZ=UTC 09:00-13:00", core.HoursShutdownImmediate))
	g.set("a", true, false)
	r.tick()
	wantCalls(t, g, "held at 20:00", "hold a immediate")
	r.s.Apply(&core.Config{Harnesses: map[string]core.Harness{"a": {Name: "a"}}, HarnessOrder: []string{"a"}})
	r.tick()
	wantCalls(t, g, "next tick after reload", "release a")
	r.tick()
	wantCalls(t, g, "later ticks")
}

// An unchanged value across a reload keeps its state: a harness running in
// hours is not bounced.
func TestGateReloadUnchangedDoesNotBounce(t *testing.T) {
	r, g := gateRig(t, mon(10, 0))
	cfg := gatedCfg(t, "a", "TZ=UTC 09:00-13:00", core.HoursShutdownImmediate)
	r.s.Apply(cfg)
	g.set("a", true, false)
	r.tick()
	r.s.Apply(cfg)
	r.tick()
	wantCalls(t, g, "reload in hours")
}

// A hold still in flight (a graceful stop waiting out its grace) is not
// issued again by the next tick.
func TestGateDoesNotStackHolds(t *testing.T) {
	r, g := gateRig(t, mon(20, 0))
	r.s.Apply(gatedCfg(t, "a", "TZ=UTC 09:00-13:00", core.HoursShutdownImmediate))
	g.set("a", true, false)
	release := make(chan struct{})
	entered := make(chan struct{})
	g.onHold = func() { close(entered); <-release }
	r.s.evaluate()
	<-entered
	g.set("a", true, false) // still up while the stop runs
	r.s.evaluate()
	r.s.evaluate()
	close(release)
	r.s.firings.Wait()
	wantCalls(t, g, "three ticks during one stop", "hold a immediate")
}

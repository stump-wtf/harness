package scheduler

// Gate Pass And Hold Reasons
//
// What the pass does with hold reasons other than hours (SPEC-0021 REQ-14):
// out of hours, a harness already held for another reason gains the hours
// reason too, so that reason clearing out of hours leaves it held; and every
// non-hours reason the clearing hook reports is released, one harness action
// per tick. The fake gate tests pin the decisions; TestParkExpiresOutOfHours
// drives the real Manager through REQ-14's scenario on the fake clock, with
// a quota hold injected, and TestARealParkExpiresOutOfHours does it again
// with a real park: a provider error with a reset time, the Manager's park
// store, and its own quota clearer.
//
// Governing: ADR-0027, SPEC-0021 REQ-4, REQ-14 "Release and hold reasons";
// ADR-0019, SPEC-0012 REQ "Gate Enforcement".
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#468.
//
// @joestump 10/04/2026 - TestARealParkExpiresOutOfHours, on the park store
// (stump.wtf/harness#477).

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/supervisor"
)

func (g *fakeGate) holdFor(name string, rs ...core.HoldReason) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.up[name] = false
	for _, r := range rs {
		g.other[name] = g.other[name].With(r)
	}
}

func (g *fakeGate) clear(name string, r core.HoldReason) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.cleared[name] = g.cleared[name].With(r)
}

// Out of hours, a harness down for a park gains the hours reason, without a
// close step (nothing is up to close); once held for hours the pass leaves it
// alone. SPEC-0021 REQ-4 Scenario "Hours decide before budgets": held with
// both reasons.
func TestGateAddsHoursToAHarnessHeldForAnotherReason(t *testing.T) {
	r, g := gateRig(t, mon(13, 30))
	r.s.Apply(gatedCfg(t, "a", "TZ=UTC 09:00-14:00", core.HoursShutdownGraceful))
	g.holdFor("a", core.HoldQuota)

	r.tick()
	wantCalls(t, g, "13:30, in hours, parked")
	r.at(mon(14, 0))
	wantCalls(t, g, "14:00, out of hours, parked", "hold a graceful")
	if _, holds, _, _ := g.Status("a"); holds != core.HoldSetOf(core.HoldHours, core.HoldQuota) {
		t.Fatalf("holds = %s, want hours+quota", holds)
	}
	r.runUntil(mon(14, 5), time.Second)
	wantCalls(t, g, "held for both")
}

// A harness the supervisor will not hold for its hours (a disabled one: it is
// not the gate's to hold, SPEC-0012) is asked once per hold set, not once per
// tick, and asked again when its set changes or the next close comes round.
func TestGateAsksToAddHoursOncePerHoldSet(t *testing.T) {
	r, g := gateRig(t, mon(14, 0))
	r.s.Apply(gatedCfg(t, "a", "TZ=UTC 09:00-14:00", core.HoursShutdownImmediate))
	g.holdFor("a", core.HoldQuota)
	decline := func() { g.set("a", false, false) } // the supervisor refused the hours reason

	r.tick()
	wantCalls(t, g, "14:00, parked and disabled", "hold a immediate")
	decline()
	r.runUntil(mon(14, 10), time.Second)
	wantCalls(t, g, "ten minutes of ticks after the refusal")

	g.holdFor("a", core.HoldBudget) // the set changes: ask again, once
	r.tick()
	wantCalls(t, g, "after a budget hold joined the park", "hold a immediate")
	decline()
	r.runUntil(mon(14, 20), time.Second)
	wantCalls(t, g, "after the second refusal")

	// The window opens and closes again: the next close asks afresh.
	r.at(mon(9, 0).AddDate(0, 0, 1))
	r.at(mon(14, 0).AddDate(0, 0, 1))
	wantCalls(t, g, "the next close", "hold a immediate")
}

// The clearing hook: a reported quota clear is released, once.
func TestGateReleasesAClearedReason(t *testing.T) {
	r, g := gateRig(t, mon(10, 0))
	r.s.Apply(gatedCfg(t, "a", "TZ=UTC 09:00-14:00", core.HoursShutdownImmediate))
	g.holdFor("a", core.HoldQuota)
	r.tick()
	wantCalls(t, g, "parked in hours")

	g.clear("a", core.HoldQuota)
	r.tick()
	wantCalls(t, g, "the park cleared", "release a quota")
	r.tick()
	wantCalls(t, g, "after the release")
}

// The hook covers harnesses without operating hours too: a park is not an
// hours concept, and neither is its release.
func TestGateReleasesAClearedReasonOnAnUngatedHarness(t *testing.T) {
	r, g := gateRig(t, mon(10, 0))
	r.s.Apply(&core.Config{Harnesses: map[string]core.Harness{"plain": {Name: "plain"}}, HarnessOrder: []string{"plain"}})
	g.holdFor("plain", core.HoldQuota, core.HoldBudget)
	g.clear("plain", core.HoldQuota)
	g.clear("plain", core.HoldBudget)
	r.tick()
	wantCalls(t, g, "both cleared", "release plain quota", "release plain budget")
}

// Hours closing and a park clearing on the same tick (a host that slept
// through both): the pass adds the hours reason first and leaves the park's
// release to the next tick, so the harness is never released in between.
func TestGateClearingYieldsToAnHoursHoldOnTheSameTick(t *testing.T) {
	r, g := gateRig(t, mon(13, 0))
	r.s.Apply(gatedCfg(t, "a", "TZ=UTC 09:00-14:00", core.HoursShutdownImmediate))
	g.holdFor("a", core.HoldQuota)
	r.tick()
	wantCalls(t, g, "13:00, parked in hours")
	g.clear("a", core.HoldQuota)

	r.at(mon(16, 0)) // waking past both the close (14:00) and the park (15:00)
	wantCalls(t, g, "first tick after waking", "hold a immediate")
	r.tick()
	wantCalls(t, g, "next tick", "release a quota")
	if up, holds, _, _ := g.Status("a"); up || holds != core.HoldSetOf(core.HoldHours) {
		t.Errorf("after the park cleared out of hours: up=%v holds=%s, want down, held for hours", up, holds)
	}
}

// SPEC-0021 REQ-14 Scenario "A park expires out of hours", end to end on the
// real Manager: parked at 13:30 until 15:00, hours close at 14:00, and the
// harness stays held for hours past 15:00, starting at the next window.
func TestParkExpiresOutOfHours(t *testing.T) {
	cfg := gatedCfg(t, "w", "TZ=UTC 09:00-14:00", core.HoursShutdownImmediate)
	h := cfg.Harnesses["w"]
	h.Adapter, h.Backend = "generic", core.BackendNative
	h.Args = []string{"-c", "while true; do sleep 0.02; done"}
	cfg.Harnesses["w"] = h
	parkUntil := mon30(15, 0)
	dir := t.TempDir()
	m := supervisor.NewManager(cfg, supervisor.ManagerOptions{
		StatePath: filepath.Join(dir, "state.json"),
		LogDir:    filepath.Join(dir, "logs"),
		Watch:     &countingBridge{},
		// The test-only reason injector's other half: the park's reset
		// instant, judged on the tick's clock as the park story will.
		HoldClearers: map[core.HoldReason]supervisor.HoldClearer{
			core.HoldQuota: func(_ string, now time.Time) bool { return !now.Before(parkUntil) },
		},
	})
	t.Cleanup(m.Close)
	m.Start("w")
	waitRunning(t, m)

	r := &rig{clock: newFakeClock(mon30(13, 0)), store: newMemStore()}
	r.s = New(Options{Start: r.start, Clock: r.clock, Location: time.UTC, Store: r.store, Recorder: r, Gate: gateManager{m}})
	t.Cleanup(r.s.Close)
	r.s.Apply(cfg)
	if snap, _ := m.Snapshot("w"); snap.State != core.StateRunning || !snap.Holds.Empty() {
		t.Fatalf("13:00 in hours: state=%s holds=%s, want running, not held", snap.State, snap.Holds)
	}

	// 13:30: the provider refuses it, and it is parked until 15:00.
	r.at(mon30(13, 30))
	m.Hold("w", core.HoldQuota, core.HoursShutdownImmediate, time.Time{})
	parked := waitSnap(t, m, "parked", func(s supervisor.Snapshot) bool {
		return s.State == core.StateStopped && s.Holds == core.HoldSetOf(core.HoldQuota)
	})

	// 14:00: the window closes on a parked harness, which is now held for
	// its hours as well.
	r.at(mon30(14, 0))
	waitSnap(t, m, "held for hours and quota", func(s supervisor.Snapshot) bool {
		return s.Holds == core.HoldSetOf(core.HoldHours, core.HoldQuota)
	})

	// 15:00: the park clears, and the harness stays held for its hours.
	r.at(mon30(15, 0))
	waitSnap(t, m, "the park cleared", func(s supervisor.Snapshot) bool {
		return s.Holds == core.HoldSetOf(core.HoldHours)
	})
	r.runUntil(mon30(15, 5), time.Minute)
	snap, _ := m.Snapshot("w")
	if snap.State != core.StateStopped || snap.LastStarted != parked.LastStarted || !snap.Enabled {
		t.Fatalf("out of hours after the park: state=%s started %v → %v enabled=%v, want stopped, never restarted, enabled",
			snap.State, parked.LastStarted, snap.LastStarted, snap.Enabled)
	}

	// 09:00 the next day: the window opens, the last reason clears, and it
	// starts.
	r.at(mon30(9, 0).AddDate(0, 0, 1))
	waitSnap(t, m, "running at the next window", func(s supervisor.Snapshot) bool {
		return s.State == core.StateRunning && s.Holds.Empty()
	})
}

// SPEC-0021 REQ-14 Scenario "A park expires out of hours" with a real park: at
// 13:30 the running harness's provider refuses it with a quota error that
// names 15:00 as its reset, and the Manager parks it there (state.json, a
// quota hold, an immediate stop). Hours close at 14:00; at 15:00 the Manager's
// own clearer releases the park on the gate tick and the harness stays held
// for its hours, starting at the next window.
func TestARealParkExpiresOutOfHours(t *testing.T) {
	cfg := gatedCfg(t, "w", "TZ=UTC 09:00-14:00", core.HoursShutdownImmediate)
	h := cfg.Harnesses["w"]
	h.Adapter, h.Backend = "generic", core.BackendNative
	h.Args = []string{"-c", "while true; do sleep 0.02; done"}
	h.Restart = core.RestartAlways
	cfg.Harnesses["w"] = h
	parkUntil := mon30(15, 0)
	r := &rig{clock: newFakeClock(mon30(13, 0)), store: newMemStore()}
	dir := t.TempDir()
	m := supervisor.NewManager(cfg, supervisor.ManagerOptions{
		StatePath: filepath.Join(dir, "state.json"),
		LogDir:    filepath.Join(dir, "logs"),
		Watch:     &countingBridge{},
		Now:       r.clock.Now, // admission, the park and the clearer on the tick's clock
	})
	t.Cleanup(m.Close)
	m.Start("w")
	waitRunning(t, m)
	r.s = New(Options{Start: r.start, Clock: r.clock, Location: time.UTC, Store: r.store, Recorder: r, Gate: gateManager{m}})
	t.Cleanup(r.s.Close)
	r.s.Apply(cfg)

	r.at(mon30(13, 30))
	m.QuotaObserve("w", "crush", time.Now(), false, "429 Too Many Requests: quota resets at 2030-09-23T15:00:00Z")
	parked := waitSnap(t, m, "parked", func(s supervisor.Snapshot) bool {
		return s.State == core.StateStopped && s.Holds == core.HoldSetOf(core.HoldQuota)
	})
	if p, ok := m.ParkOf("w"); !ok || !p.Until.Equal(parkUntil) {
		t.Fatalf("park = %+v ok=%v, want until 15:00", p, ok)
	}

	r.at(mon30(14, 0))
	waitSnap(t, m, "held for hours and quota", func(s supervisor.Snapshot) bool {
		return s.Holds == core.HoldSetOf(core.HoldHours, core.HoldQuota)
	})
	r.at(mon30(14, 59))
	r.tick()
	if snap, _ := m.Snapshot("w"); !snap.Holds.Has(core.HoldQuota) {
		t.Fatalf("14:59: the park cleared early: holds=%s", snap.Holds)
	}

	r.at(mon30(15, 0))
	waitSnap(t, m, "the park cleared, held for hours", func(s supervisor.Snapshot) bool {
		return s.Holds == core.HoldSetOf(core.HoldHours)
	})
	r.runUntil(mon30(15, 5), time.Minute)
	if snap, _ := m.Snapshot("w"); snap.State != core.StateStopped || snap.LastStarted != parked.LastStarted || !snap.Enabled {
		t.Fatalf("out of hours after the park: state=%s started %v → %v enabled=%v, want stopped, never restarted, enabled",
			snap.State, parked.LastStarted, snap.LastStarted, snap.Enabled)
	}
	if _, ok := m.ParkOf("w"); ok {
		t.Fatal("the park is still in force after its reset")
	}

	r.at(mon30(9, 0).AddDate(0, 0, 1))
	waitSnap(t, m, "running at the next window", func(s supervisor.Snapshot) bool {
		return s.State == core.StateRunning && s.Holds.Empty()
	})
}

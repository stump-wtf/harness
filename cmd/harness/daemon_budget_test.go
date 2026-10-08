package main

// Daemon Budget Wiring
//
// SPEC-0021's run-count budget through the daemon's own wiring: the Manager
// daemonManagerOptions builds (its real restart Policy, #315), and the gate
// pass startDaemonScheduler starts with the daemon's hoursGate, which is the
// only thing that rolls the budget day over and clears a budget hold. A test
// that drove the Manager's hooks itself would pass with the gate adapter
// missing them; these substitute only the clock, the same one for the
// scheduler's tick and admission (ManagerOptions.Now), and shrink durations.
//
// Governing: ADR-0027; SPEC-0021 REQ-3 "The budget day", REQ-4 "Admission",
// REQ-5 "Run-count budget", REQ-14, REQ-20 "Reload"; issue #315.
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#470.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/attach"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/hours"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// budgetRig is a daemon Manager on a fake clock, with the daemon's scheduler
// not yet started.
type budgetRig struct {
	mgr    *supervisor.Manager
	clock  *stubClock
	logs   string
	policy supervisor.Policy
}

func newBudgetRig(t *testing.T, cfg *core.Config, at time.Time) *budgetRig {
	t.Helper()
	tmp := t.TempDir()
	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = filepath.Join(tmp, "state.json")
	opts.LogDir = filepath.Join(tmp, "logs")
	// Durations only: MaxRestarts and the rest of the give-up wiring are the
	// daemon's own (TestDaemonPolicyParksAReliablyFailingHarness).
	opts.Policy.CrashWindow = 20 * time.Millisecond
	opts.Policy.BackoffBase = 2 * time.Millisecond
	opts.Policy.BackoffCap = 10 * time.Millisecond
	opts.Policy.HealthyRun = 0
	opts.Policy.StopGrace = 200 * time.Millisecond
	clock := &stubClock{now: at, ticks: make(chan time.Time)}
	opts.Now = clock.Now
	mgr := supervisor.NewManager(cfg, opts)
	reg.SetController(mgr)
	t.Cleanup(mgr.Close)
	if err := mgr.Restore(); err != nil {
		t.Fatal(err)
	}
	return &budgetRig{mgr: mgr, clock: clock, logs: opts.LogDir, policy: opts.Policy}
}

// tick moves the clock to at and runs one scheduler evaluation on it.
func (r *budgetRig) tick(at time.Time) {
	r.clock.mu.Lock()
	r.clock.now = at
	r.clock.mu.Unlock()
	r.clock.ticks <- at
}

// tickUntil ticks from at, a second further each time, until pred holds of
// name's snapshot. The gate pass acts on a harness at most once per tick and
// defers one whose last action is still being carried out, so a single tick
// can land before the action it waits on has finished; the daemon's real
// ticker simply ticks again, and so does this.
func (r *budgetRig) tickUntil(t *testing.T, at time.Time, name, desc string, pred func(supervisor.Snapshot) bool) supervisor.Snapshot {
	t.Helper()
	var snap supervisor.Snapshot
	waitUntil(t, desc, func() bool {
		r.tick(at)
		at = at.Add(time.Second)
		for range 20 { // let the tick's dispatch land before ticking again
			if snap, _ = r.mgr.Snapshot(name); pred(snap) {
				return true
			}
			time.Sleep(time.Millisecond)
		}
		return false
	})
	return snap
}

func (r *budgetRig) wait(t *testing.T, name, desc string, pred func(supervisor.Snapshot) bool) supervisor.Snapshot {
	t.Helper()
	var snap supervisor.Snapshot
	waitUntil(t, desc, func() bool {
		snap, _ = r.mgr.Snapshot(name)
		return pred(snap)
	})
	return snap
}

func budgetCfg(t *testing.T, dayStarts string, hs ...core.Harness) *core.Config {
	t.Helper()
	cfg := &core.Config{Harnesses: map[string]core.Harness{}, Profiles: map[string]core.Profile{}}
	for _, h := range hs {
		cfg.Harnesses[h.Name] = h
		cfg.HarnessOrder = append(cfg.HarnessOrder, h.Name)
	}
	d, err := hours.ParseDailyInstant(dayStarts)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Budget.DayStarts = d
	return cfg
}

// firstRunFails is a resident whose first process exits non-zero and every
// later one keeps running: a restart after the first exit is the start a
// spent budget refuses.
func firstRunFails(t *testing.T, name string, max int) core.Harness {
	flag := filepath.Join(t.TempDir(), "ran-once")
	return core.Harness{
		Name:    name,
		Adapter: "generic",
		Args:    []string{"-c", "[ -f '" + flag + "' ] || { touch '" + flag + "'; exit 1; }; while true; do sleep 0.02; done"},
		Backend: core.BackendNative,
		Restart: core.RestartOnFailure,
		Budget:  core.Budget{MaxRunsPerDay: max},
	}
}

func overBudget(s supervisor.Snapshot) bool {
	return s.State == core.StateStopped && s.Holds == core.HoldSetOf(core.HoldBudget)
}

// TestDaemonBudgetHoldsACrashLoop is REQ-5 Scenario "A crash loop spends the
// budget", through the daemon's Manager and Policy: a resident with
// max_runs_per_day = 5 that exits non-zero on every start is started five
// times, then held over-budget, not failed and not started a sixth time, and
// its durable log says why.
func TestDaemonBudgetHoldsACrashLoop(t *testing.T) {
	h := core.Harness{
		Name: "crashloop", Adapter: "generic", Args: []string{"-c", "exit 1"},
		Backend: core.BackendNative, Restart: core.RestartOnFailure, RestartDelay: time.Millisecond,
		Budget: core.Budget{MaxRunsPerDay: 5},
	}
	r := newBudgetRig(t, budgetCfg(t, "TZ=UTC 00:00", h), time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	if r.policy.MaxRestarts < 5 {
		// Five failed runs must not reach give-up first, or the scenario
		// tests the policy instead of the budget.
		t.Fatalf("the daemon's MaxRestarts is %d; the scenario needs five runs before give-up", r.policy.MaxRestarts)
	}
	if !r.mgr.Start(h.Name) {
		t.Fatal("Start returned false")
	}
	snap := r.wait(t, h.Name, "held over-budget", overBudget)
	if snap.Enabled != true || snap.State == core.StateFailed {
		t.Fatalf("crash loop: state=%s enabled=%v, want stopped and enabled", snap.State, snap.Enabled)
	}
	if got := r.mgr.RunsToday(h.Name); got != 5 {
		t.Fatalf("runs today = %d, want 5", got)
	}
	time.Sleep(200 * time.Millisecond) // ample for a sixth start the backoff would make
	if got, after := r.mgr.RunsToday(h.Name), r.mustSnap(t, h.Name); got != 5 || !overBudget(after) || after.LastStarted != snap.LastStarted {
		t.Fatalf("later: runs today %d, state=%s holds=%s started %v→%v, want no sixth start", got, after.State, after.Holds, snap.LastStarted, after.LastStarted)
	}
	log, err := os.ReadFile(filepath.Join(r.logs, h.Name+".log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "reason=budget") || !strings.Contains(string(log), "5/5 runs today") {
		t.Errorf("the durable log does not say why it is held:\n%s", log)
	}
}

func (r *budgetRig) mustSnap(t *testing.T, name string) supervisor.Snapshot {
	t.Helper()
	snap, ok := r.mgr.Snapshot(name)
	if !ok {
		t.Fatalf("unknown harness %q", name)
	}
	return snap
}

// TestDaemonBudgetSleepsThroughMidnight is REQ-3 Scenario "Sleeping through
// midnight": a harness held over-budget at 23:30, on a laptop that sleeps
// from 23:45 to 08:00. The first tick after waking begins a new budget day,
// the budget reason clears, and the harness goes back through admission and
// starts, counted toward the new day.
func TestDaemonBudgetSleepsThroughMidnight(t *testing.T) {
	h := firstRunFails(t, "night", 1)
	cfg := budgetCfg(t, "TZ=UTC 00:00", h)
	r := newBudgetRig(t, cfg, time.Date(2026, 10, 4, 23, 30, 0, 0, time.UTC))
	r.mgr.Start(h.Name)
	r.wait(t, h.Name, "held over-budget at 23:30", overBudget)

	sched := startDaemonScheduler(r.mgr, cfg, r.clock)
	t.Cleanup(sched.Close)
	r.tick(time.Date(2026, 10, 4, 23, 45, 0, 0, time.UTC))
	if snap := r.mustSnap(t, h.Name); !overBudget(snap) {
		t.Fatalf("23:45, same day: state=%s holds=%s, want still held over-budget", snap.State, snap.Holds)
	}

	// The first tick after waking, and the ones after it if the pass had to
	// defer: all on the new day.
	r.tickUntil(t, time.Date(2026, 10, 5, 8, 0, 0, 0, time.UTC), h.Name, "running in the new day", func(s supervisor.Snapshot) bool {
		return s.State == core.StateRunning && s.Holds.Empty()
	})
	if start, _ := r.mgr.BudgetDay(); !start.Equal(time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("budget day starts %v, want 2026-10-05 00:00 UTC", start)
	}
	if got := r.mgr.RunsToday(h.Name); got != 1 {
		t.Errorf("runs today = %d, want 1: nothing carried over, and the release was admitted", got)
	}
}

// TestDaemonBudgetRaisingTheCapMidDay is REQ-20 Scenario "Raising the cap
// mid-day": a harness held over-budget at its cap, the operator raises
// max_runs_per_day and the config reloads, and the budget hold clears on the
// next tick and the harness goes through admission. No restart of anything
// else is involved: the reload alone does it.
func TestDaemonBudgetRaisingTheCapMidDay(t *testing.T) {
	h := firstRunFails(t, "raised", 1)
	cfg := budgetCfg(t, "TZ=UTC 00:00", h)
	r := newBudgetRig(t, cfg, time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC))
	r.mgr.Start(h.Name)
	r.wait(t, h.Name, "held over-budget at 1/1", overBudget)
	sched := startDaemonScheduler(r.mgr, cfg, r.clock)
	t.Cleanup(sched.Close)
	r.tick(time.Date(2026, 10, 4, 12, 1, 0, 0, time.UTC))
	if snap := r.mustSnap(t, h.Name); !overBudget(snap) {
		t.Fatalf("before the reload: state=%s holds=%s, want held over-budget", snap.State, snap.Holds)
	}

	h.Budget.MaxRunsPerDay = 2
	r.mgr.Reload(budgetCfg(t, "TZ=UTC 00:00", h))
	r.tickUntil(t, time.Date(2026, 10, 4, 12, 2, 0, 0, time.UTC), h.Name, "running after the raise", func(s supervisor.Snapshot) bool {
		return s.State == core.StateRunning && s.Holds.Empty()
	})
	if got := r.mgr.RunsToday(h.Name); got != 2 {
		t.Errorf("runs today = %d, want 2: the start after the raise was admitted and counted", got)
	}
}

// TestDaemonBudgetHoursDecideFirst is REQ-4 Scenario "Hours decide before
// budgets": a harness out of hours whose run budget is also spent is held
// with both reasons on the gate's ticks; when its hours open first it stays
// held for budget, the reason that clears last, rather than starting.
func TestDaemonBudgetHoursDecideFirst(t *testing.T) {
	const expr = "TZ=UTC Mon 09:00-10:00; Mon 11:00-12:00"
	e, err := hours.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	h := core.Harness{
		Name: "gated", Adapter: "generic", Args: []string{"-c", "while true; do sleep 0.02; done"},
		Backend: core.BackendNative, OperatingHours: expr, HoursExpr: e, HoursShutdown: core.HoursShutdownImmediate,
		Budget: core.Budget{MaxRunsPerDay: 1},
	}
	cfg := budgetCfg(t, "TZ=UTC 00:00", h)
	mon := func(hh, mm int) time.Time { return time.Date(2026, 9, 21, hh, mm, 0, 0, time.UTC) }
	r := newBudgetRig(t, cfg, mon(9, 30))
	r.mgr.Start(h.Name)
	r.wait(t, h.Name, "running in hours", func(s supervisor.Snapshot) bool { return s.State == core.StateRunning })

	sched := startDaemonScheduler(r.mgr, cfg, r.clock)
	t.Cleanup(sched.Close)
	r.tickUntil(t, mon(10, 0), h.Name, "held for hours and budget", func(s supervisor.Snapshot) bool {
		return s.State == core.StateStopped && s.Holds == core.HoldSetOf(core.HoldHours, core.HoldBudget)
	})

	// 11:00: the window opens, but the budget is still spent.
	r.tickUntil(t, mon(11, 0), h.Name, "held for budget alone", func(s supervisor.Snapshot) bool {
		return s.Holds == core.HoldSetOf(core.HoldBudget)
	})
	r.tick(mon(11, 30))
	if snap := r.mustSnap(t, h.Name); snap.State != core.StateStopped || !snap.Enabled || r.mgr.RunsToday(h.Name) != 1 {
		t.Fatalf("hours open, budget spent: state=%s enabled=%v runs today %d, want stopped, enabled, 1", snap.State, snap.Enabled, r.mgr.RunsToday(h.Name))
	}
}

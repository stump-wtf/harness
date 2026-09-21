package main

// Daemon Operating Hours Wiring
//
// The gate pass exists only if the daemon hands the scheduler a Gate, and the
// reload path re-applies hours only if the daemon registers the hook. A test
// that built its own scheduler would pass with both missing — the #315
// lesson (TestDaemonManagerOptionsEnableGiveUp) — so this drives the daemon's
// own startDaemonScheduler and daemonManagerOptions against a real Manager and
// a real process, substituting only the clock.
//
// Governing: ADR-0019, SPEC-0012 REQ "Gate Evaluation", REQ "Gate
// Enforcement", REQ "Operating Hours Reload"; issue #315.
//
// @joestump-agent 09/21/2026 - Added for stump.wtf/harness#382.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/attach"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/hours"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// stubClock is a fixed wall clock whose ticks the test sends by hand.
type stubClock struct {
	mu    sync.Mutex
	now   time.Time
	ticks chan time.Time
}

func (c *stubClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *stubClock) NewTicker(time.Duration) (<-chan time.Time, func()) { return c.ticks, func() {} }

func waitUntil(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for: %s", desc)
}

// persistedEnabled reads name's `enabled` from the state file on disk.
func persistedEnabled(t *testing.T, path, name string) (enabled bool, state string, ok bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return false, "", false
	}
	var ps struct {
		Harnesses map[string]struct {
			Enabled bool   `json:"enabled"`
			State   string `json:"state"`
		} `json:"harnesses"`
	}
	if json.Unmarshal(data, &ps) != nil {
		return false, "", false
	}
	h, ok := ps.Harnesses[name]
	return h.Enabled, h.State, ok
}

func TestDaemonSchedulerEnforcesOperatingHours(t *testing.T) {
	tmp := t.TempDir()
	statePath := filepath.Join(tmp, "state.json")
	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = statePath
	opts.LogDir = filepath.Join(tmp, "logs")
	opts.Policy.StopGrace = 200 * time.Millisecond

	const expr = "TZ=UTC Mon 09:00-13:00"
	e, err := hours.Parse(expr)
	if err != nil {
		t.Fatal(err)
	}
	base := core.Harness{
		Name:    "gated",
		Adapter: "generic",
		Args:    []string{"-c", "while true; do sleep 0.02; done"},
		Backend: core.BackendNative,
	}
	h := base
	h.OperatingHours, h.HoursExpr, h.HoursShutdown = expr, e, core.HoursShutdownImmediate
	cfg := &core.Config{Harnesses: map[string]core.Harness{h.Name: h}, HarnessOrder: []string{h.Name}, Profiles: map[string]core.Profile{}}

	mgr := supervisor.NewManager(cfg, opts)
	reg.SetController(mgr)
	t.Cleanup(mgr.Close)
	if !mgr.Start(h.Name) {
		t.Fatal("Start returned false")
	}
	waitUntil(t, "running before the scheduler starts", func() bool {
		s, _ := mgr.Snapshot(h.Name)
		return s.State == core.StateRunning
	})
	before, _ := mgr.Snapshot(h.Name)

	// Monday 20:00 UTC: out of hours. The scheduler's first evaluation runs
	// on Start, so no tick is needed for the hold.
	clock := &stubClock{now: time.Date(2026, 9, 21, 20, 0, 0, 0, time.UTC), ticks: make(chan time.Time)}
	sched := startDaemonScheduler(mgr, cfg, clock)
	t.Cleanup(sched.Close) // runs before mgr.Close

	waitUntil(t, "held by the daemon's gate pass", func() bool {
		s, _ := mgr.Snapshot(h.Name)
		return s.State == core.StateStopped && s.Held
	})
	snap, _ := mgr.Snapshot(h.Name)
	if !snap.Enabled || snap.RestartCount != before.RestartCount {
		t.Errorf("after hold: enabled=%v restarts=%d (was %d), want enabled and unchanged", snap.Enabled, snap.RestartCount, before.RestartCount)
	}
	waitUntil(t, "state.json records the hold", func() bool {
		_, st, ok := persistedEnabled(t, statePath, h.Name)
		return ok && st == string(core.StateStopped)
	})
	if enabled, _, _ := persistedEnabled(t, statePath, h.Name); !enabled {
		t.Error("state.json enabled = false after an operating-hours hold")
	}

	// A reload that removes operating_hours reaches the scheduler through the
	// daemon's reload hook and releases the held harness on the next tick.
	next := &core.Config{Harnesses: map[string]core.Harness{base.Name: base}, HarnessOrder: []string{base.Name}, Profiles: map[string]core.Profile{}}
	mgr.Reload(next)
	clock.ticks <- clock.Now()
	waitUntil(t, "released after hours were removed", func() bool {
		s, _ := mgr.Snapshot(h.Name)
		return s.State == core.StateRunning && !s.Held
	})
}

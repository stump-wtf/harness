package supervisor

// Operating Hours Hold And Release Tests
//
// These assert the properties SPEC-0012 REQ "Gate Enforcement" names against
// the real artifacts: `enabled` is read back from the state.json file the
// persist loop writes (polled for the whole hold, so a transient false is
// caught, not just the final value), and the restart count and respawn
// behaviour are read off the supervisor after the restart delay has had time
// to fire.
//
// Governing: ADR-0019, SPEC-0012 REQ "Gate Enforcement", REQ "Operating
// Hours Reload".
//
// @joestump-agent 09/21/2026 - Added for stump.wtf/harness#382.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// gated returns h with operating_hours set. The expression itself is never
// evaluated here: deciding in/out of hours is the scheduler's job, so these
// tests drive Hold/Release directly.
func gated(h core.Harness) core.Harness {
	h.OperatingHours = "Mon 09:00-13:00"
	h.HoursShutdown = core.HoursShutdownImmediate
	return h
}

// newStateManager is newTestManager that also returns the state.json path.
func newStateManager(t *testing.T, cfg *core.Config, p Policy) (*Manager, string) {
	t.Helper()
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	m := NewManager(cfg, ManagerOptions{Policy: p, StatePath: statePath, LogDir: filepath.Join(dir, "logs")})
	t.Cleanup(m.Close)
	return m, statePath
}

// readPersisted reads one harness's record from the state file on disk.
func readPersisted(t *testing.T, path, name string) (persistedHarness, bool) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return persistedHarness{}, false
	}
	var ps persistedState
	if err := json.Unmarshal(data, &ps); err != nil {
		return persistedHarness{}, false // mid-rename; the next read gets it
	}
	ph, ok := ps.Harnesses[name]
	return ph, ok
}

// watchEnabled polls state.json until stopped and counts every read that
// found the harness recorded with enabled = false.
func watchEnabled(t *testing.T, path, name string) (stop func() (reads, disabled int)) {
	t.Helper()
	var r, d atomic.Int64
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			if ph, ok := readPersisted(t, path, name); ok {
				r.Add(1)
				if !ph.Enabled {
					d.Add(1)
				}
			}
			time.Sleep(time.Millisecond)
		}
	}()
	return func() (int, int) {
		close(done)
		wg.Wait()
		return int(r.Load()), int(d.Load())
	}
}

// waitPersisted waits for the state file to record name in state want.
func waitPersisted(t *testing.T, path, name string, want core.State) persistedHarness {
	t.Helper()
	var ph persistedHarness
	waitFor(t, 3*time.Second, "state.json records "+string(want), func() bool {
		var ok bool
		ph, ok = readPersisted(t, path, name)
		return ok && ph.State == want
	})
	return ph
}

// SPEC-0012 Scenario "Close": running + enabled → stopping → stopped,
// enabled still true ON DISK, restart count unchanged, not respawned.
func TestHoldKeepsEnabledInStateJSONAndDoesNotRespawn(t *testing.T) {
	h := gated(shHarness("gated", "while true; do sleep 0.02; done", 5*time.Millisecond))
	m, statePath := newStateManager(t, managerCfg(h), fastPolicy())
	m.Start("gated")
	waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("gated"); return s.State == core.StateRunning })
	waitPersisted(t, statePath, "gated", core.StateRunning)
	before, _ := m.Snapshot("gated")

	stop := watchEnabled(t, statePath, "gated")
	m.Hold("gated", core.HoursShutdownImmediate, time.Time{})
	ph := waitPersisted(t, statePath, "gated", core.StateStopped)
	// Give a (wrong) respawn every chance to happen: many restart delays.
	time.Sleep(100 * time.Millisecond)
	reads, disabled := stop()

	if reads == 0 {
		t.Fatal("never read state.json during the hold; the enabled check proved nothing")
	}
	if disabled != 0 {
		t.Errorf("state.json recorded enabled=false in %d of %d reads during the hold", disabled, reads)
	}
	if !ph.Enabled {
		t.Error("state.json enabled = false after a hold, want true")
	}
	if ph.RestartCount != before.RestartCount {
		t.Errorf("persisted restart_count = %d, want %d (a hold is not a restart)", ph.RestartCount, before.RestartCount)
	}
	snap, _ := m.Snapshot("gated")
	if snap.State != core.StateStopped || !snap.Held || !snap.Enabled {
		t.Errorf("after hold: state=%s held=%v enabled=%v, want stopped/held/enabled", snap.State, snap.Held, snap.Enabled)
	}
	if snap.RestartCount != before.RestartCount {
		t.Errorf("restart count %d → %d across a hold", before.RestartCount, snap.RestartCount)
	}
	if snap.PID != 0 {
		t.Errorf("held harness still has pid %d", snap.PID)
	}
}

// SPEC-0012 Scenario "Open": held → starting → running, enabled untouched.
func TestReleaseStartsHeldHarness(t *testing.T) {
	h := gated(shHarness("gated", "while true; do sleep 0.02; done", 0))
	m, statePath := newStateManager(t, managerCfg(h), fastPolicy())
	m.Start("gated")
	waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("gated"); return s.State == core.StateRunning })
	m.Hold("gated", core.HoursShutdownImmediate, time.Time{})
	m.Release("gated")
	snap, _ := m.Snapshot("gated")
	if snap.State != core.StateRunning || snap.Held || !snap.Enabled {
		t.Fatalf("after release: state=%s held=%v enabled=%v, want running/not held/enabled", snap.State, snap.Held, snap.Enabled)
	}
	if ph := waitPersisted(t, statePath, "gated", core.StateRunning); !ph.Enabled {
		t.Error("state.json enabled = false after release")
	}
}

// SPEC-0012 Scenario "Close during a restart delay": the pending respawn is
// cancelled and the harness is held.
func TestHoldCancelsPendingRespawn(t *testing.T) {
	h := gated(shHarness("gated", "exit 1", 150*time.Millisecond))
	p := fastPolicy()
	p.CrashThreshold = 100 // stay out of degraded; exercise plain restarting
	p.MaxRestarts = 0
	s := newTestSupervisor(t, h, p)
	s.Start()
	waitState(t, s, core.StateRestarting)
	before := s.Snapshot().RestartCount

	s.Hold(core.HoursShutdownImmediate, time.Time{})
	time.Sleep(300 * time.Millisecond) // two restart delays
	snap := s.Snapshot()
	if snap.State != core.StateStopped || !snap.Held || !snap.Enabled {
		t.Fatalf("state=%s held=%v enabled=%v, want stopped/held/enabled", snap.State, snap.Held, snap.Enabled)
	}
	if snap.RestartCount != before {
		t.Errorf("restart count %d → %d: the cancelled respawn ran", before, snap.RestartCount)
	}
	if snap.NextRetryIn != 0 {
		t.Errorf("NextRetryIn = %v after hold, want 0", snap.NextRetryIn)
	}
}

// SPEC-0012 Scenario "Close a degraded harness": stopped at once, backoff
// reset, held.
func TestHoldDegradedResetsBackoff(t *testing.T) {
	h := gated(shHarness("gated", "exit 1", 0))
	p := fastPolicy()
	p.BackoffBase = 400 * time.Millisecond
	p.BackoffCap = 400 * time.Millisecond
	p.MaxRestarts = 0
	s := newTestSupervisor(t, h, p)
	s.Start()
	waitFor(t, 3*time.Second, "flapping", func() bool { return s.Snapshot().Flapping })
	before := s.Snapshot().RestartCount

	s.Hold(core.HoursShutdownImmediate, time.Time{})
	snap := s.Snapshot()
	if snap.State != core.StateStopped || !snap.Held {
		t.Fatalf("state=%s held=%v, want stopped/held", snap.State, snap.Held)
	}
	if snap.Flapping || snap.NextRetryIn != 0 {
		t.Errorf("flapping=%v next_retry_in=%v after hold, want reset", snap.Flapping, snap.NextRetryIn)
	}
	time.Sleep(600 * time.Millisecond) // past the armed backoff
	if got := s.Snapshot(); got.State != core.StateStopped || got.RestartCount != before {
		t.Errorf("after backoff elapsed: state=%s restarts=%d (was %d), want stopped and unchanged", got.State, got.RestartCount, before)
	}
}

// SPEC-0012 Scenarios "Failed harness at open" and "Operator-stopped harness
// at open": neither is held, and a release never starts them.
func TestHoldAndReleaseLeaveFailedAndDisabledAlone(t *testing.T) {
	p := fastPolicy()
	p.MaxRestarts = 1
	failed := newTestSupervisor(t, gated(shHarness("f", "exit 1", 0)), p)
	failed.Start()
	waitState(t, failed, core.StateFailed)
	failed.Hold(core.HoursShutdownImmediate, time.Time{})
	failed.Release()
	if snap := failed.Snapshot(); snap.State != core.StateFailed || snap.Held {
		t.Errorf("failed harness: state=%s held=%v, want failed/not held", snap.State, snap.Held)
	}

	stopped := newTestSupervisor(t, gated(shHarness("s", "while true; do sleep 0.02; done", 0)), fastPolicy())
	stopped.Start()
	waitState(t, stopped, core.StateRunning)
	stopped.Hold(core.HoursShutdownImmediate, time.Time{})
	stopped.Stop() // operator stop while held: enabled=false, no longer held
	if snap := stopped.Snapshot(); snap.Held || snap.Enabled {
		t.Fatalf("after stop: held=%v enabled=%v, want neither", snap.Held, snap.Enabled)
	}
	stopped.Release()
	stopped.Hold(core.HoursShutdownImmediate, time.Time{})
	if snap := stopped.Snapshot(); snap.State != core.StateStopped || snap.Held || snap.Enabled {
		t.Errorf("operator-stopped harness: state=%s held=%v enabled=%v, want stopped, untouched", snap.State, snap.Held, snap.Enabled)
	}
}

// SPEC-0012 Scenario "Boot out of hours": Autostart does not start an enabled
// gated harness; it begins held (the gate pass releases it if in hours).
// Ungated harnesses still start.
func TestAutostartHoldsGatedHarness(t *testing.T) {
	cfg := managerCfg(
		gated(shHarness("gated", "while true; do sleep 0.02; done", 0)),
		shHarness("plain", "while true; do sleep 0.02; done", 0),
	)
	m, statePath := newStateManager(t, cfg, fastPolicy())
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	m.Autostart()
	waitFor(t, 3*time.Second, "plain running", func() bool { s, _ := m.Snapshot("plain"); return s.State == core.StateRunning })
	snap, _ := m.Snapshot("gated")
	if snap.State != core.StateStopped || !snap.Held || !snap.Enabled || !snap.LastStarted.IsZero() {
		t.Fatalf("gated after autostart: state=%s held=%v enabled=%v started=%v, want never-started, held, enabled",
			snap.State, snap.Held, snap.Enabled, snap.LastStarted)
	}
	if up, held, closing, ok := m.GateStatus("gated"); !ok || up || !held || closing {
		t.Errorf("GateStatus = up %v held %v closing %v ok %v, want down, held, not closing, known", up, held, closing, ok)
	}
	m.Release("gated")
	waitFor(t, 3*time.Second, "released harness running", func() bool { s, _ := m.Snapshot("gated"); return s.State == core.StateRunning })
	if ph := waitPersisted(t, statePath, "gated", core.StateRunning); !ph.Enabled {
		t.Error("state.json enabled = false after autostart + release")
	}
}

// SPEC-0012 Scenario "A new harness introduced out of hours": ADR-0014
// records intent true (in state.json) and it begins held rather than started.
func TestReloadIntroducesGatedHarnessHeld(t *testing.T) {
	cfg := managerCfg(shHarness("plain", "while true; do sleep 0.02; done", 0))
	m, statePath := newStateManager(t, cfg, fastPolicy())
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	m.Autostart()

	m.Reload(managerCfg(
		shHarness("plain", "while true; do sleep 0.02; done", 0),
		gated(shHarness("newgated", "while true; do sleep 0.02; done", 0)),
	))
	snap, _ := m.Snapshot("newgated")
	if snap.State != core.StateStopped || !snap.Held || !snap.Enabled {
		t.Fatalf("new gated harness: state=%s held=%v enabled=%v, want stopped/held/enabled", snap.State, snap.Held, snap.Enabled)
	}
	if ph := waitPersisted(t, statePath, "newgated", core.StateStopped); !ph.Enabled {
		t.Error("state.json does not record the new harness's intent as true")
	}
}

// SPEC-0012 Scenarios "A profile switch targets a held harness" and "A
// profile switch at the close".
func TestUseProfileRespectsTheGate(t *testing.T) {
	loop := "while true; do sleep 0.02; done"
	cfg := managerCfg(gated(shHarness("held", loop, 0)), gated(shHarness("up", loop, 0)))
	cfg.Profiles["work"] = core.Profile{Name: "work", Harnesses: []string{"held", "up"}}
	cfg.ProfileOrder = append(cfg.ProfileOrder, "work")
	m, statePath := newStateManager(t, cfg, fastPolicy())

	// "held": operator-stopped, then made a member while out of hours.
	m.Start("held")
	m.Hold("held", core.HoursShutdownImmediate, time.Time{})
	m.Stop("held")
	// "up": running in hours.
	m.Start("up")
	waitFor(t, 3*time.Second, "up running", func() bool { s, _ := m.Snapshot("up"); return s.State == core.StateRunning })
	pid := func() int { s, _ := m.Snapshot("up"); return s.PID }()

	if !m.UseProfile("work") {
		t.Fatal("UseProfile(work) = false")
	}
	if snap, _ := m.Snapshot("held"); snap.State != core.StateStopped || !snap.Held || !snap.Enabled {
		t.Errorf("held member: state=%s held=%v enabled=%v, want stopped/held/enabled", snap.State, snap.Held, snap.Enabled)
	}
	if ph := waitPersisted(t, statePath, "held", core.StateStopped); !ph.Enabled {
		t.Error("state.json does not record the held member's intent as true")
	}
	if snap, _ := m.Snapshot("up"); snap.State != core.StateRunning || snap.PID != pid {
		t.Errorf("running member bounced: state=%s pid %d → %d", snap.State, pid, snap.PID)
	}
}

// The hold-vs-crash window (design.md § Risks "The hold exit races a real
// crash"): a process that exits on its own at the moment a hold is issued must
// end held and stopped, never respawned. Run under -race by `make race`.
func TestHoldRacesNaturalExit(t *testing.T) {
	p := fastPolicy()
	p.MaxRestarts = 0
	p.CrashThreshold = 1000
	for i := 0; i < 20; i++ {
		// The process lives ~10ms; the hold lands somewhere around its exit.
		h := gated(shHarness("race", "sleep 0.01; exit 1", time.Millisecond))
		s := newTestSupervisor(t, h, p)
		s.Start()
		time.Sleep(time.Duration(5+i%10) * time.Millisecond)
		s.Hold(core.HoursShutdownImmediate, time.Time{})
		held := s.Snapshot()
		time.Sleep(40 * time.Millisecond) // many restart delays
		snap := s.Snapshot()
		if snap.State != core.StateStopped || !snap.Held || !snap.Enabled {
			t.Fatalf("iteration %d: state=%s held=%v enabled=%v, want stopped/held/enabled", i, snap.State, snap.Held, snap.Enabled)
		}
		if snap.RestartCount != held.RestartCount || snap.LastStarted != held.LastStarted {
			t.Fatalf("iteration %d: respawned after the hold (restarts %d → %d)", i, held.RestartCount, snap.RestartCount)
		}
		s.Shutdown()
	}
}

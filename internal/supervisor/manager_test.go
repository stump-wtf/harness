package supervisor

// Governing tests: SPEC-0003 REQ "Autostart", "Lifecycle Events", "Config
// Change Application"; ADR-0006 (hot reload, last-good on parse error); ADR-0007
// (persisted intent + restart counts restored on daemon restart; log tee).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// managerCfg builds a one/two-harness config with an autostart profile.
func managerCfg(harnesses ...core.Harness) *core.Config {
	cfg := &core.Config{
		Harnesses: map[string]core.Harness{},
		Profiles:  map[string]core.Profile{},
	}
	var names []string
	for _, h := range harnesses {
		cfg.Harnesses[h.Name] = h
		cfg.HarnessOrder = append(cfg.HarnessOrder, h.Name)
		names = append(names, h.Name)
	}
	cfg.Profiles["default"] = core.Profile{Name: "default", Harnesses: names, Autostart: true}
	cfg.ProfileOrder = []string{"default"}
	return cfg
}

func newTestManager(t *testing.T, cfg *core.Config) *Manager {
	t.Helper()
	dir := t.TempDir()
	m := NewManager(cfg, ManagerOptions{
		Policy:    fastPolicy(),
		StatePath: filepath.Join(dir, "state.json"),
		LogDir:    filepath.Join(dir, "logs"),
	})
	t.Cleanup(m.Close)
	return m
}

// ---- SPEC-0003 REQ "Autostart" -------------------------------------------

func TestManagerAutostartBringsUpEnabledHarnesses(t *testing.T) {
	cfg := managerCfg(shHarness("auto", "while true; do sleep 0.02; done", 0))
	m := newTestManager(t, cfg)
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	m.Autostart()
	waitFor(t, 3*time.Second, "autostarted harness reaches running", func() bool {
		snap, _ := m.Snapshot("auto")
		return snap.State == core.StateRunning
	})
}

// ---- ADR-0007: daemon restart restores intent + restart counts -----------

func TestManagerPersistsAndRestoresIntent(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	logDir := filepath.Join(dir, "logs")

	cfg := managerCfg(shHarness("persist", "exit 0", 5*time.Millisecond))
	// First daemon: never give up so restarts accumulate.
	p := Policy{CrashWindow: 5 * time.Millisecond, CrashThreshold: 1000, MaxRestarts: 0, StopGrace: 100 * time.Millisecond}
	m1 := NewManager(cfg, ManagerOptions{Policy: p, StatePath: statePath, LogDir: logDir})
	if err := m1.Restore(); err != nil {
		t.Fatal(err)
	}
	m1.Autostart()
	waitFor(t, 3*time.Second, "restart count accrues", func() bool {
		snap, _ := m1.Snapshot("persist")
		return snap.RestartCount >= 3
	})
	priorCount := func() int { s, _ := m1.Snapshot("persist"); return s.RestartCount }()
	m1.Stop("persist") // enabled=false so the second daemon won't autostart it
	if err := m1.Save(); err != nil {
		t.Fatal(err)
	}
	m1.Close()

	// Second daemon on the same state.json.
	m2 := NewManager(cfg, ManagerOptions{Policy: p, StatePath: statePath, LogDir: logDir})
	t.Cleanup(m2.Close)
	if err := m2.Restore(); err != nil {
		t.Fatal(err)
	}
	snap, ok := m2.Snapshot("persist")
	if !ok {
		t.Fatal("harness missing after restore")
	}
	if snap.Enabled {
		t.Fatal("intent not restored: expected enabled=false (was stopped before shutdown)")
	}
	if snap.RestartCount < priorCount {
		t.Fatalf("restart count not restored: got %d, want >= %d", snap.RestartCount, priorCount)
	}
}

// ---- SPEC-0003 REQ "Lifecycle Events" ------------------------------------

func TestManagerEmitsLifecycleEvents(t *testing.T) {
	cfg := managerCfg(shHarness("ev", "while true; do sleep 0.02; done", 0))
	m := newTestManager(t, cfg)
	events, cancel := m.Events()
	defer cancel()

	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	m.Start("ev")

	// Collect events until we've seen a state change to running (no polling).
	sawStarting, sawRunning := false, false
	deadline := time.After(3 * time.Second)
	for !(sawStarting && sawRunning) {
		select {
		case ev := <-events:
			if ev.Kind == EventStateChanged && ev.Name == "ev" {
				if ev.To == core.StateStarting {
					sawStarting = true
				}
				if ev.To == core.StateRunning {
					sawRunning = true
				}
			}
		case <-deadline:
			t.Fatalf("did not observe state-change events (starting=%v running=%v)", sawStarting, sawRunning)
		}
	}
}

func TestManagerEmitsExitedAndFlapping(t *testing.T) {
	cfg := managerCfg(shHarness("crash", "exit 1", 0))
	m := newTestManager(t, cfg)
	events, cancel := m.Events()
	defer cancel()
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	m.Start("crash")

	sawExited, sawFlapping := false, false
	deadline := time.After(3 * time.Second)
	for !(sawExited && sawFlapping) {
		select {
		case ev := <-events:
			switch ev.Kind {
			case EventExited:
				sawExited = true
			case EventFlapping:
				sawFlapping = true
				if ev.NextRetryIn <= 0 {
					t.Error("flapping event missing next_retry_in")
				}
			}
		case <-deadline:
			t.Fatalf("missing events: exited=%v flapping=%v", sawExited, sawFlapping)
		}
	}
}

// ---- ADR-0006: config-change flagging via Reload -------------------------

func TestManagerReloadFlagsRunningHarness(t *testing.T) {
	cfg := managerCfg(shHarness("re", "while true; do sleep 0.02; done", 0))
	m := newTestManager(t, cfg)
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	m.Start("re")
	waitFor(t, 3*time.Second, "running", func() bool {
		s, _ := m.Snapshot("re")
		return s.State == core.StateRunning
	})
	origPID := func() int { s, _ := m.Snapshot("re"); return s.PID }()

	// Reload with a changed definition for the running harness.
	newCfg := managerCfg(shHarness("re", "sleep 60", 0))
	m.Reload(newCfg)

	waitFor(t, time.Second, "ConfigChanged flagged", func() bool {
		s, _ := m.Snapshot("re")
		return s.ConfigChanged
	})
	if s, _ := m.Snapshot("re"); s.PID != origPID {
		t.Fatal("reload bounced a running process (must apply on next restart)")
	}
}

func TestManagerReloadAddsAndRemovesHarnesses(t *testing.T) {
	cfg := managerCfg(shHarness("keep", "while true; do sleep 0.02; done", 0),
		shHarness("drop", "while true; do sleep 0.02; done", 0))
	m := newTestManager(t, cfg)
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	if len(m.Snapshots()) != 2 {
		t.Fatalf("expected 2 harnesses, got %d", len(m.Snapshots()))
	}
	// Drop "drop", add "new".
	newCfg := managerCfg(shHarness("keep", "while true; do sleep 0.02; done", 0),
		shHarness("new", "while true; do sleep 0.02; done", 0))
	m.Reload(newCfg)
	if _, ok := m.Snapshot("drop"); ok {
		t.Fatal("removed harness still present after reload")
	}
	if _, ok := m.Snapshot("new"); !ok {
		t.Fatal("added harness missing after reload")
	}
}

// ---- ADR-0006: hot reload keeps last-good config on parse error ----------

func TestReloadFromFileKeepsLastGoodOnParseError(t *testing.T) {
	cfg := managerCfg(shHarness("good", "while true; do sleep 0.02; done", 0))
	m := newTestManager(t, cfg)
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), "bad.toml")
	if err := os.WriteFile(bad, []byte("[harness.oops\ncmd = \"x\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.ReloadFromFile(bad); err == nil {
		t.Fatal("expected a parse error from malformed TOML")
	}
	// Last-good config retained: the original harness is still known.
	if _, ok := m.Snapshot("good"); !ok {
		t.Fatal("last-good config not retained after a bad reload")
	}
}

// ---- ADR-0007: raw PTY output teed to the per-harness log ----------------

func TestLogTeeCapturesOutput(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	cfg := managerCfg(shHarness("noisy", "echo HELLO_HARNESS; sleep 5", 0))
	m := NewManager(cfg, ManagerOptions{
		Policy:    Policy{CrashWindow: time.Second, CrashThreshold: 3, MaxRestarts: 5, StopGrace: 200 * time.Millisecond},
		StatePath: filepath.Join(dir, "state.json"),
		LogDir:    logDir,
	})
	t.Cleanup(m.Close)
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	m.Start("noisy")
	logPath := filepath.Join(logDir, "noisy.log")
	waitFor(t, 3*time.Second, "log file captures PTY output", func() bool {
		data, err := os.ReadFile(logPath)
		return err == nil && strings.Contains(string(data), "HELLO_HARNESS")
	})
}

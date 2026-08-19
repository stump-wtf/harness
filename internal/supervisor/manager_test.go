package supervisor

// Governing tests: SPEC-0003 REQ "Autostart", "Lifecycle Events", "Config
// Change Application"; ADR-0006 (hot reload, last-good on parse error); ADR-0007
// (persisted intent + restart counts restored on daemon restart; log tee).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
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

// The mirror of the test above, for the case it never covered: a harness that is
// still RUNNING when the daemon goes down. Shutdown must not be read as intent.
//
// cmdShutdown used to set enabled=false on every live harness, and Close() ends
// with a Save(), so a clean daemon restart — a systemd restart for a new binary,
// a reboot, anything — persisted enabled=false and the next Autostart() started
// nothing. Only harnesses actually in use were affected; an already-stopped one
// kept its intent. The daemon cannot know why it is being stopped and must not
// guess: intent changes only through Start/Stop/UseProfile.
func TestManagerShutdownPreservesRunningIntent(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	logDir := filepath.Join(dir, "logs")

	cfg := managerCfg(shHarness("survivor", "while true; do sleep 0.02; done", 0))

	m1 := NewManager(cfg, ManagerOptions{Policy: fastPolicy(), StatePath: statePath, LogDir: logDir})
	if err := m1.Restore(); err != nil {
		t.Fatal(err)
	}
	m1.Autostart()
	waitFor(t, 3*time.Second, "harness reaches running", func() bool {
		snap, _ := m1.Snapshot("survivor")
		return snap.State == core.StateRunning
	})
	// Close() is the real shutdown path: Shutdown() per supervisor, then the
	// final durable Save(). No Stop() — nobody asked for this to stop.
	m1.Close()

	// The daemon comes back on the same state.json.
	m2 := NewManager(cfg, ManagerOptions{Policy: fastPolicy(), StatePath: statePath, LogDir: logDir})
	t.Cleanup(m2.Close)
	if err := m2.Restore(); err != nil {
		t.Fatal(err)
	}
	snap, ok := m2.Snapshot("survivor")
	if !ok {
		t.Fatal("harness missing after restore")
	}
	if !snap.Enabled {
		t.Fatal("a daemon restart cleared intent: a running harness must restore enabled=true")
	}
	// The consequence that actually bit: it has to come back up.
	m2.Autostart()
	waitFor(t, 3*time.Second, "harness autostarts after the daemon restart", func() bool {
		s, _ := m2.Snapshot("survivor")
		return s.State == core.StateRunning
	})
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

// TestReloadHookInvokedOnEveryReloadPath verifies the reload hook fires after
// a successful Reload — the single choke point behind SIGHUP, the config
// watcher, and the daemon's reload control op (issue #66: the scheduler
// re-applies entries here, so a path that skipped it would run stale
// schedules) — and does NOT fire when a reload fails parse.
func TestReloadHookInvokedOnEveryReloadPath(t *testing.T) {
	cfg := managerCfg(shHarness("keep", "while true; do sleep 0.02; done", 0))
	m := newTestManager(t, cfg)
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	var calls int
	m.SetReloadHook(func() { calls++ })

	m.Reload(managerCfg(shHarness("keep", "while true; do sleep 0.02; done", 0)))
	if calls != 1 {
		t.Fatalf("hook calls after Reload = %d, want 1", calls)
	}

	good := filepath.Join(t.TempDir(), "good.toml")
	if err := os.WriteFile(good, []byte("[harness.keep]\nharness = \"generic\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.ReloadFromFile(good); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("hook calls after ReloadFromFile = %d, want 2", calls)
	}

	bad := filepath.Join(t.TempDir(), "bad.toml")
	if err := os.WriteFile(bad, []byte("[harness.oops\nharness = \"generic\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.ReloadFromFile(bad); err == nil {
		t.Fatal("expected a parse error from malformed TOML")
	}
	if calls != 2 {
		t.Fatalf("hook must not fire on a failed reload; calls = %d, want 2", calls)
	}
}

// ---- Issue #150: reload autostarts newly-introduced enabled harnesses -----

// TestReloadAutostartsNewEnabledHarness verifies Option A: a harness newly
// introduced by a reload that has config autostart membership (enabled=true
// or autostart profile member) gets runtime intent set and is started.
func TestReloadAutostartsNewEnabledHarness(t *testing.T) {
	cfg := managerCfg(shHarness("existing", "while true; do sleep 0.02; done", 0))
	m := newTestManager(t, cfg)
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	m.Autostart()

	// Add a new harness with enabled=true via reload.
	newH := shHarness("newenabled", "while true; do sleep 0.02; done", 0)
	newH.Enabled = true
	newCfg := managerCfg(
		shHarness("existing", "while true; do sleep 0.02; done", 0),
		newH,
	)
	m.Reload(newCfg)

	snap, ok := m.Snapshot("newenabled")
	if !ok {
		t.Fatal("newly-added harness missing after reload")
	}
	if !snap.Enabled {
		t.Error("newly-added enabled harness should have runtime intent=true after reload")
	}
	// Give it a moment to actually start.
	time.Sleep(100 * time.Millisecond)
	snap, _ = m.Snapshot("newenabled")
	if snap.State != core.StateRunning {
		t.Errorf("newly-added enabled harness state=%v, want running", snap.State)
	}
}

// TestReloadDoesNotOverrideExplicitStop verifies that reloading does not
// restart a harness the user explicitly stopped (265e42a / 2dfa8fc invariant).
func TestReloadDoesNotOverrideExplicitStop(t *testing.T) {
	h := shHarness("stoppable", "while true; do sleep 0.02; done", 0)
	h.Enabled = true
	cfg := managerCfg(h)
	m := newTestManager(t, cfg)
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	m.Autostart()
	time.Sleep(100 * time.Millisecond)

	// Explicitly stop it.
	m.Stop("stoppable")
	time.Sleep(100 * time.Millisecond)

	// Reload with the same config — must NOT restart it.
	m.Reload(cfg)
	time.Sleep(100 * time.Millisecond)

	snap, ok := m.Snapshot("stoppable")
	if !ok {
		t.Fatal("harness missing after reload")
	}
	if snap.Enabled {
		t.Error("explicitly stopped harness should still have intent=false after reload")
	}
	if snap.State == core.StateRunning {
		t.Error("explicitly stopped harness should not be running after reload")
	}
}

// TestReloadNewDisabledHarnessStaysStopped verifies that a newly-introduced
// harness without autostart membership stays stopped after reload.
func TestReloadNewDisabledHarnessStaysStopped(t *testing.T) {
	cfg := managerCfg(shHarness("existing", "while true; do sleep 0.02; done", 0))
	m := newTestManager(t, cfg)
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	m.Autostart()

	// Add a new harness with enabled=false (not in autostart set since
	// managerCfg puts all harnesses in the default autostart profile, so
	// we need a config without it in the profile).
	newCfg := &core.Config{
		Harnesses: map[string]core.Harness{
			"existing": shHarness("existing", "while true; do sleep 0.02; done", 0),
			"newdisabled": {
				Name:    "newdisabled",
				Adapter: "generic",
				Args:    []string{"-c", "while true; do sleep 0.02; done"},
				Backend: core.BackendNative,
				Enabled: false,
			},
		},
		HarnessOrder: []string{"existing", "newdisabled"},
		Profiles: map[string]core.Profile{
			"default": {Name: "default", Harnesses: []string{"existing"}, Autostart: true},
		},
		ProfileOrder: []string{"default"},
	}
	m.Reload(newCfg)
	time.Sleep(100 * time.Millisecond)

	snap, ok := m.Snapshot("newdisabled")
	if !ok {
		t.Fatal("newly-added disabled harness missing after reload")
	}
	if snap.Enabled {
		t.Error("disabled harness should not have runtime intent=true")
	}
	if snap.State == core.StateRunning {
		t.Error("disabled harness should not be running")
	}
}

// ---- ADR-0006: hot reload keeps last-good config on parse error ----------

// TestReloadReIntroducedHarnessAutostarts pins the edge case from PR #157
// review: a harness that was present at boot, explicitly stopped (persisted
// Enabled=false), removed from config, then re-added with enabled=true via
// reload, is treated as newly-introduced and autostarts. Config re-introduction
// deliberately wins over stale persisted intent (ADR-0014).
func TestReloadReIntroducedHarnessAutostarts(t *testing.T) {
	h := shHarness("reintro", "while true; do sleep 0.02; done", 0)
	h.Enabled = true
	cfg := managerCfg(h)
	m := newTestManager(t, cfg)
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	m.Autostart()
	time.Sleep(100 * time.Millisecond)

	// Explicitly stop it — persisted intent becomes Enabled=false.
	m.Stop("reintro")
	time.Sleep(100 * time.Millisecond)

	// Remove from config and reload — harness is dropped.
	emptyCfg := managerCfg()
	m.Reload(emptyCfg)
	if _, ok := m.Snapshot("reintro"); ok {
		t.Fatal("harness should be gone after removal reload")
	}

	// Re-add with enabled=true — should autostart despite stale persisted state.
	m.Reload(cfg)
	time.Sleep(100 * time.Millisecond)

	snap, ok := m.Snapshot("reintro")
	if !ok {
		t.Fatal("re-introduced harness missing after reload")
	}
	if !snap.Enabled {
		t.Error("re-introduced enabled harness should have runtime intent=true (ADR-0014: config re-introduction wins over stale persisted intent)")
	}
	if snap.State != core.StateRunning {
		t.Errorf("re-introduced enabled harness state=%v, want running", snap.State)
	}
}

func TestReloadFromFileKeepsLastGoodOnParseError(t *testing.T) {
	cfg := managerCfg(shHarness("good", "while true; do sleep 0.02; done", 0))
	m := newTestManager(t, cfg)
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(t.TempDir(), "bad.toml")
	if err := os.WriteFile(bad, []byte("[harness.oops\nharness = \"generic\"\n"), 0o600); err != nil {
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

// ---- Issue #99: phantom profile detection and autostart fallback ----------

// TestManagerRestoreWithPhantomProfile verifies that a persisted active profile
// absent from the current config is detected (ProfileResolved() == false), and
// that the daemon falls back to the autostart profile so harnesses actually
// start. This is the exact scenario from the bug report: a chezmoi rename
// leaves state.json pointing at a profile name that no longer exists, and the
// daemon silently starts nothing while reporting perfect health.
func TestManagerRestoreWithPhantomProfile(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	logDir := filepath.Join(dir, "logs")

	cfg := managerCfg(shHarness("alpha", "while true; do sleep 0.02; done", 0))

	// Simulate a state.json from a prior daemon where "everything" was the
	// active profile — a profile that no longer exists in cfg.
	phantomState := persistedState{
		Version:       stateSchemaVersion,
		ActiveProfile: "everything", // not in cfg
		Harnesses:     map[string]persistedHarness{},
		// Deliberately no per-harness entries: the harnesses were managed by
		// the phantom profile and their names may have changed. The daemon
		// must fall back to the autostart set, not to nothing.
	}
	data, err := json.Marshal(phantomState)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(cfg, ManagerOptions{
		Policy:    fastPolicy(),
		StatePath: statePath,
		LogDir:    logDir,
	})
	t.Cleanup(m.Close)

	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}

	// The daemon must detect the phantom profile.
	if m.ProfileResolved() {
		t.Fatal("expected ProfileResolved() == false for a persisted profile not in config")
	}

	// The daemon must NOT silently start nothing — the autostart fallback
	// should have seeded intent for the autostart profile members.
	m.Autostart()
	waitFor(t, 3*time.Second, "autostart fallback brings up harnesses", func() bool {
		snap, _ := m.Snapshot("alpha")
		return snap.State == core.StateRunning
	})
}

// TestManagerProfileResolvedAfterUseProfile verifies that choosing a valid
// profile clears the unresolved flag (#99 recovery path).
func TestManagerProfileResolvedAfterUseProfile(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	logDir := filepath.Join(dir, "logs")

	cfg := managerCfg(shHarness("beta", "while true; do sleep 0.02; done", 0))

	phantomState := persistedState{
		Version:       stateSchemaVersion,
		ActiveProfile: "ghost",
		Harnesses:     map[string]persistedHarness{},
	}
	data, _ := json.Marshal(phantomState)
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(cfg, ManagerOptions{
		Policy:    fastPolicy(),
		StatePath: statePath,
		LogDir:    logDir,
	})
	t.Cleanup(m.Close)

	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	if m.ProfileResolved() {
		t.Fatal("expected unresolved before use-profile")
	}

	// Choosing a real profile resolves it.
	if !m.UseProfile("default") {
		t.Fatal("use-profile default failed")
	}
	if !m.ProfileResolved() {
		t.Fatal("expected ProfileResolved() == true after use-profile")
	}
}

// TestManagerReloadResolvesProfile verifies that a config reload which
// reintroduces the missing profile name clears the unresolved flag (#99
// interaction with hot reload).
func TestManagerReloadResolvesProfile(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	logDir := filepath.Join(dir, "logs")

	cfg := managerCfg(shHarness("gamma", "while true; do sleep 0.02; done", 0))

	phantomState := persistedState{
		Version:       stateSchemaVersion,
		ActiveProfile: "returned",
		Harnesses:     map[string]persistedHarness{},
	}
	data, _ := json.Marshal(phantomState)
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(cfg, ManagerOptions{
		Policy:    fastPolicy(),
		StatePath: statePath,
		LogDir:    logDir,
	})
	t.Cleanup(m.Close)

	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	if m.ProfileResolved() {
		t.Fatal("expected unresolved before reload")
	}

	// Reload a config that includes the "returned" profile.
	newCfg := managerCfg(shHarness("gamma", "while true; do sleep 0.02; done", 0))
	newCfg.Profiles["returned"] = core.Profile{
		Name:      "returned",
		Harnesses: []string{"gamma"},
	}
	newCfg.ProfileOrder = append(newCfg.ProfileOrder, "returned")

	m.Reload(newCfg)
	if !m.ProfileResolved() {
		t.Fatal("expected ProfileResolved() == true after reload reintroduces profile")
	}
}

// TestManagerRestorePhantomProfileOverridesPersistedDisabled is the case the
// original #99 fix missed. Its sibling test seeds an EMPTY Harnesses map, so
// every harness takes the pre-existing `autostart[name]` branch and the test
// passes with the phantom-profile logic removed entirely.
//
// The realistic shape is the opposite: state.json carries per-harness entries,
// and the members of the now-vanished profile are recorded as DISABLED —
// because that profile is what disabled them. If persisted intent wins there,
// the daemon still comes up having started nothing, which is exactly the bug.
func TestManagerRestorePhantomProfileOverridesPersistedDisabled(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	cfg := managerCfg(shHarness("alpha", "while true; do sleep 0.02; done", 0))

	data, err := json.Marshal(persistedState{
		Version:       stateSchemaVersion,
		ActiveProfile: "everything", // phantom: absent from cfg
		Harnesses: map[string]persistedHarness{
			// Disabled by the profile that no longer exists.
			"alpha": {Enabled: false, RestartCount: 3, LastExitCode: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(cfg, ManagerOptions{
		Policy:    fastPolicy(),
		StatePath: statePath,
		LogDir:    filepath.Join(dir, "logs"),
	})
	t.Cleanup(m.Close)

	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	if m.ProfileResolved() {
		t.Fatal("expected ProfileResolved() == false for a phantom profile")
	}

	// Restart history must survive the override — the fallback changes intent,
	// not the harness's recorded past.
	if snap, _ := m.Snapshot("alpha"); snap.RestartCount != 3 {
		t.Errorf("RestartCount = %d, want 3 preserved across the fallback", snap.RestartCount)
	}

	m.Autostart()
	waitFor(t, 3*time.Second, "phantom-profile fallback starts an autostart member recorded as disabled", func() bool {
		snap, _ := m.Snapshot("alpha")
		return snap.State == core.StateRunning
	})
}

// TestManagerRestoreResolvedProfileKeepsPersistedIntent is the guard on the
// other side: when the profile DOES resolve, a harness the operator stopped
// must stay stopped. The fallback is scoped to the unresolved case only.
func TestManagerRestoreResolvedProfileKeepsPersistedIntent(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	cfg := managerCfg(shHarness("alpha", "while true; do sleep 0.02; done", 0))

	data, err := json.Marshal(persistedState{
		Version:       stateSchemaVersion,
		ActiveProfile: "default", // exists in managerCfg, and is autostart
		Harnesses: map[string]persistedHarness{
			"alpha": {Enabled: false}, // operator stopped it
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(cfg, ManagerOptions{
		Policy:    fastPolicy(),
		StatePath: statePath,
		LogDir:    filepath.Join(dir, "logs"),
	})
	t.Cleanup(m.Close)

	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	if !m.ProfileResolved() {
		t.Fatal("expected ProfileResolved() == true")
	}
	if snap, _ := m.Snapshot("alpha"); snap.Enabled {
		t.Error("a deliberately stopped harness must stay stopped when the profile resolves")
	}
	// …and that silence is exactly what needs reporting: config says
	// autostart = true, the harness never comes up, and nothing else says why.
	if got := m.DormantAutostart(); !slices.Equal(got, []string{"alpha"}) {
		t.Errorf("DormantAutostart() = %v, want [alpha]", got)
	}
}

// A member the operator never stopped is absent from state.json, gets restored
// enabled, and must NOT be reported as dormant.
func TestManagerDormantAutostartEmptyWhenNothingSuppressed(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	cfg := managerCfg(shHarness("alpha", "while true; do sleep 0.02; done", 0))

	data, err := json.Marshal(persistedState{
		Version:       stateSchemaVersion,
		ActiveProfile: "default",
		Harnesses:     map[string]persistedHarness{"alpha": {Enabled: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(cfg, ManagerOptions{
		Policy:    fastPolicy(),
		StatePath: statePath,
		LogDir:    filepath.Join(dir, "logs"),
	})
	t.Cleanup(m.Close)

	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	if got := m.DormantAutostart(); len(got) != 0 {
		t.Errorf("DormantAutostart() = %v, want empty", got)
	}
}

// The #99 fallback overrides persisted intent to enabled, so a member caught by
// it is started and must not also be reported dormant — that would tell the
// operator to start something already running.
func TestManagerDormantAutostartExcludesPhantomProfileFallback(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	cfg := managerCfg(shHarness("alpha", "while true; do sleep 0.02; done", 0))

	data, err := json.Marshal(persistedState{
		Version:       stateSchemaVersion,
		ActiveProfile: "gone", // renamed away upstream → unresolved (#99)
		Harnesses:     map[string]persistedHarness{"alpha": {Enabled: false}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(cfg, ManagerOptions{
		Policy:    fastPolicy(),
		StatePath: statePath,
		LogDir:    filepath.Join(dir, "logs"),
	})
	t.Cleanup(m.Close)

	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	if snap, _ := m.Snapshot("alpha"); !snap.Enabled {
		t.Fatal("the #99 fallback should have forced alpha enabled")
	}
	if got := m.DormantAutostart(); len(got) != 0 {
		t.Errorf("DormantAutostart() = %v, want empty (fallback already started it)", got)
	}
}

// Starting the harness is the documented fix, so the report must stop naming it.
func TestManagerStartClearsDormantAutostart(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")

	cfg := managerCfg(shHarness("alpha", "while true; do sleep 0.02; done", 0))

	data, err := json.Marshal(persistedState{
		Version:       stateSchemaVersion,
		ActiveProfile: "default",
		Harnesses:     map[string]persistedHarness{"alpha": {Enabled: false}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	m := NewManager(cfg, ManagerOptions{
		Policy:    fastPolicy(),
		StatePath: statePath,
		LogDir:    filepath.Join(dir, "logs"),
	})
	t.Cleanup(m.Close)

	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	if got := m.DormantAutostart(); len(got) != 1 {
		t.Fatalf("precondition: DormantAutostart() = %v, want [alpha]", got)
	}
	m.Start("alpha")
	if got := m.DormantAutostart(); len(got) != 0 {
		t.Errorf("DormantAutostart() = %v, want empty after start", got)
	}
}

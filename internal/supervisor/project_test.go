package supervisor

// Governing tests: SPEC-0004 REQ "Project Naming And Namespacing" (namespaced
// registration with provenance, two-project coexistence, global-name collision
// rejected with no partial state) and REQ "Project Control Operations"
// (reconcile-idempotent ProjectUp, ProjectDown stop-and-forget, unknown-project
// sentinel, invalid definitions leave nothing behind); ADR-0009.

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// loopScript is a harness body that stays up until stopped.
const loopScript = "while true; do sleep 0.02; done"

// projectDefs builds project-local looping harness definitions, enabled so
// ProjectUp starts them (SPEC-0004 REQ "Bring Up").
func projectDefs(names ...string) []core.Harness {
	out := make([]core.Harness, 0, len(names))
	for _, n := range names {
		h := shHarness(n, loopScript, 0)
		h.Enabled = true
		out = append(out, h)
	}
	return out
}

func snapshotNames(m *Manager) []string {
	snaps := m.Snapshots()
	out := make([]string, 0, len(snaps))
	for _, s := range snaps {
		out = append(out, s.Name)
	}
	return out
}

// ---- SPEC-0004 REQ "Project Naming And Namespacing" -----------------------

// TestProjectUpRegistersNamespacedWithProvenance: harnesses register
// daemon-wide as <project>/<harness> with project provenance; globals stay
// bare with empty provenance.
func TestProjectUpRegistersNamespacedWithProvenance(t *testing.T) {
	m := newTestManager(t, managerCfg(shHarness("global", loopScript, 0)))
	res, err := m.ProjectUp("reduit", projectDefs("agent", "reviewer"))
	if err != nil {
		t.Fatalf("ProjectUp: %v", err)
	}
	// The result carries the registered set, computed under the manager lock,
	// and flags the first up as a change.
	if len(res.Names) != 2 || res.Names[0] != "reduit/agent" || res.Names[1] != "reduit/reviewer" {
		t.Errorf("res.Names = %v, want [reduit/agent reduit/reviewer]", res.Names)
	}
	if !res.Changed {
		t.Error("first up not flagged Changed")
	}

	for _, full := range []string{"reduit/agent", "reduit/reviewer"} {
		if _, ok := m.Snapshot(full); !ok {
			t.Fatalf("harness %q not registered", full)
		}
		if got := m.ProjectOf(full); got != "reduit" {
			t.Errorf("ProjectOf(%q) = %q, want %q", full, got, "reduit")
		}
	}
	if got := m.ProjectOf("global"); got != "" {
		t.Errorf("ProjectOf(global) = %q, want empty (global provenance)", got)
	}
	names, ok := m.ProjectHarnesses("reduit")
	if !ok || len(names) != 2 || names[0] != "reduit/agent" || names[1] != "reduit/reviewer" {
		t.Errorf("ProjectHarnesses = %v ok=%v, want [reduit/agent reduit/reviewer] true", names, ok)
	}
	// Definitions are resolvable under the full name (drives describe/list),
	// with provenance from the same lock hold.
	if h, project, ok := m.HarnessRecord("reduit/agent"); !ok || h.Args == nil || project != "reduit" {
		t.Errorf("HarnessRecord(reduit/agent) = %+v project=%q ok=%v, want sh definition owned by reduit",
			h, project, ok)
	}
	// ProjectUp starts the new harnesses (SPEC-0004 REQ "Bring Up").
	waitFor(t, 3*time.Second, "project harnesses running", func() bool {
		a, _ := m.Snapshot("reduit/agent")
		b, _ := m.Snapshot("reduit/reviewer")
		return a.State == core.StateRunning && b.State == core.StateRunning
	})
}

// TestProjectCoexistenceSameLocalName: scenario "Two projects, same local
// harness name" — reduit/agent and spotter/agent run concurrently.
func TestProjectCoexistenceSameLocalName(t *testing.T) {
	m := newTestManager(t, managerCfg(shHarness("global", loopScript, 0)))
	if _, err := m.ProjectUp("reduit", projectDefs("agent")); err != nil {
		t.Fatalf("ProjectUp reduit: %v", err)
	}
	if _, err := m.ProjectUp("spotter", projectDefs("agent")); err != nil {
		t.Fatalf("ProjectUp spotter: %v", err)
	}
	waitFor(t, 3*time.Second, "both projects' agents running", func() bool {
		a, _ := m.Snapshot("reduit/agent")
		b, _ := m.Snapshot("spotter/agent")
		return a.State == core.StateRunning && b.State == core.StateRunning
	})
	if m.ProjectOf("reduit/agent") != "reduit" || m.ProjectOf("spotter/agent") != "spotter" {
		t.Error("provenance mixed up between coexisting projects")
	}
}

// TestProjectUpCollisionWithGlobalName: scenario "Name collides with a global
// harness" — the up fails with the sentinel and registers NOTHING.
func TestProjectUpCollisionWithGlobalName(t *testing.T) {
	m := newTestManager(t, managerCfg(shHarness("reduit", loopScript, 0)))
	before := snapshotNames(m)

	_, err := m.ProjectUp("reduit", projectDefs("agent", "reviewer"))
	if !errors.Is(err, config.ErrProjectNameCollision) {
		t.Fatalf("ProjectUp err = %v, want ErrProjectNameCollision", err)
	}
	// No partial state: nothing registered, no project record.
	after := snapshotNames(m)
	if len(after) != len(before) {
		t.Errorf("snapshots changed on failed up: before %v, after %v", before, after)
	}
	if _, ok := m.Snapshot("reduit/agent"); ok {
		t.Error("reduit/agent registered despite collision")
	}
	if _, ok := m.ProjectHarnesses("reduit"); ok {
		t.Error("project record left behind despite collision")
	}
}

// TestProjectUpReconcileSurvivesLaterGlobalCollision: the bare-global-shadow
// check applies only to a NEW registration — a global harness named like the
// project that arrives later via reload must not wedge reconcile of the
// already-registered project (SPEC-0004 REQ "Bring Up": up SHALL be
// idempotent/reconciling).
func TestProjectUpReconcileSurvivesLaterGlobalCollision(t *testing.T) {
	m := newTestManager(t, managerCfg(shHarness("global", loopScript, 0)))
	if _, err := m.ProjectUp("reduit", projectDefs("agent")); err != nil {
		t.Fatalf("ProjectUp: %v", err)
	}
	// A later global reload introduces [harness.reduit].
	m.Reload(managerCfg(shHarness("global", loopScript, 0), shHarness("reduit", loopScript, 0)))

	// Reconcile of the live project still works.
	res, err := m.ProjectUp("reduit", projectDefs("agent", "watcher"))
	if err != nil {
		t.Fatalf("re-up after global name collision: %v", err)
	}
	if len(res.Names) != 2 {
		t.Errorf("res.Names = %v, want 2 harnesses", res.Names)
	}
	// A FRESH project shadowing a global name still collides.
	if _, err := m.ProjectUp("global", projectDefs("x")); !errors.Is(err, config.ErrProjectNameCollision) {
		t.Errorf("fresh colliding project err = %v, want ErrProjectNameCollision", err)
	}
}

// TestProjectUpInvalidDefsNoPartialState: table-driven invalid payloads all
// fail with ErrInvalidProjectDef and register nothing.
func TestProjectUpInvalidDefsNoPartialState(t *testing.T) {
	cases := []struct {
		name    string
		project string
		defs    []core.Harness
	}{
		{"empty project name", "", projectDefs("agent")},
		{"slash in project name", "redu/it", projectDefs("agent")},
		// "." and ".." are reserved: registered names derive log paths, so
		// ".." would escape the log dir and "." would collide with a bare
		// global harness's log file.
		{"dot project name", ".", projectDefs("agent")},
		{"dotdot project name", "..", projectDefs("agent")},
		{"no harnesses", "reduit", nil},
		{"empty harness name", "reduit", []core.Harness{shHarness("", loopScript, 0)}},
		{"slash in harness name", "reduit", []core.Harness{shHarness("a/b", loopScript, 0)}},
		{"dot harness name", "reduit", []core.Harness{shHarness(".", loopScript, 0)}},
		{"dotdot harness name", "reduit", []core.Harness{shHarness("..", loopScript, 0)}},
		{"duplicate harness", "reduit", projectDefs("agent", "agent")},
		{"unknown harness kind", "reduit", []core.Harness{{Name: "agent", Adapter: "cursor", Backend: core.BackendNative}}},
		// Prompt and args belong to different harness shapes — the same
		// invariants the config parsers enforce (ADR-0011).
		{"prompt with args", "reduit", []core.Harness{{
			Name: "agent", Prompt: "hi", Args: []string{"x"}, Backend: core.BackendNative,
		}}},
		{"prompt with args", "reduit", []core.Harness{{
			Name: "agent", Prompt: "hi", Args: []string{"x"}, Backend: core.BackendNative,
		}}},
		{"invalid backend", "reduit", []core.Harness{{Name: "agent", Adapter: "generic", Backend: "bogus"}}},
		{"negative restart delay", "reduit", []core.Harness{{
			Name: "agent", Adapter: "generic", Backend: core.BackendNative, RestartDelay: -time.Second,
		}}},
		// The invalid entry is third: earlier valid entries must not stick.
		{"invalid mid-list", "reduit", []core.Harness{
			shHarness("a", loopScript, 0),
			shHarness("b", loopScript, 0),
			{Name: "c", Adapter: "cursor", Backend: core.BackendNative}, // unknown harness kind
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t, managerCfg(shHarness("global", loopScript, 0)))
			_, err := m.ProjectUp(tc.project, tc.defs)
			if !errors.Is(err, ErrInvalidProjectDef) {
				t.Fatalf("ProjectUp err = %v, want ErrInvalidProjectDef", err)
			}
			if got := snapshotNames(m); len(got) != 1 || got[0] != "global" {
				t.Errorf("partial state left behind: snapshots = %v", got)
			}
			if _, ok := m.ProjectHarnesses(tc.project); ok {
				t.Error("project record left behind despite invalid defs")
			}
		})
	}
}

// TestProjectUpAcceptsPromptHarness: a prompt-only definition (empty Cmd) is a
// valid project harness — its argv is synthesized at spawn (ADR-0011), so the
// registration path must not hard-require cmd. Registered disabled so the test
// never actually launches an agent CLI.
func TestProjectUpAcceptsPromptHarness(t *testing.T) {
	m := newTestManager(t, managerCfg(shHarness("global", loopScript, 0)))
	def := core.Harness{
		Name:    "oneshot",
		Prompt:  "do the thing",
		Backend: core.BackendNative,
		Restart: core.RestartNo,
	}
	res, err := m.ProjectUp("reduit", []core.Harness{def})
	if err != nil {
		t.Fatalf("ProjectUp: %v", err)
	}
	if len(res.Names) != 1 || res.Names[0] != "reduit/oneshot" {
		t.Fatalf("Names = %v, want [reduit/oneshot]", res.Names)
	}
	h, ok := m.HarnessDef("reduit/oneshot")
	if !ok {
		t.Fatal("prompt harness not registered")
	}
	if h.Prompt != "do the thing" || len(h.Args) != 0 {
		t.Errorf("registered def Prompt/Args = %q/%v, want prompt-only", h.Prompt, h.Args)
	}
}

// ---- SPEC-0004 REQ "Project Control Operations": reconcile ----------------

// TestProjectUpReconciles: re-up adds new harnesses, stops + deregisters
// removed ones, and stages changed definitions without bouncing the running
// process (SPEC-0003 REQ "Config Change Application").
func TestProjectUpReconciles(t *testing.T) {
	m := newTestManager(t, managerCfg(shHarness("global", loopScript, 0)))
	if _, err := m.ProjectUp("reduit", projectDefs("agent", "reviewer")); err != nil {
		t.Fatalf("first ProjectUp: %v", err)
	}
	waitFor(t, 3*time.Second, "agent running", func() bool {
		s, _ := m.Snapshot("reduit/agent")
		return s.State == core.StateRunning
	})
	agentPID := func() int { s, _ := m.Snapshot("reduit/agent"); return s.PID }()

	// Re-up: agent changes (new args), reviewer is gone, watcher is new.
	changed := shHarness("agent", "while true; do sleep 0.05; done", 0)
	changed.Enabled = true
	watcher := shHarness("watcher", loopScript, 0)
	watcher.Enabled = true
	res, err := m.ProjectUp("reduit", []core.Harness{changed, watcher})
	if err != nil {
		t.Fatalf("re-up: %v", err)
	}
	if !res.Changed {
		t.Error("reconcile that added/removed/changed harnesses not flagged Changed")
	}

	// Removed: reviewer stopped + deregistered.
	if _, ok := m.Snapshot("reduit/reviewer"); ok {
		t.Error("reduit/reviewer still registered after reconcile removed it")
	}
	if m.ProjectOf("reduit/reviewer") != "" {
		t.Error("stale provenance for removed harness")
	}
	// Added: watcher registered + started.
	waitFor(t, 3*time.Second, "watcher running", func() bool {
		s, _ := m.Snapshot("reduit/watcher")
		return s.State == core.StateRunning
	})
	// Changed: agent NOT bounced — same PID, flagged to apply on next restart.
	snap, _ := m.Snapshot("reduit/agent")
	if snap.PID != agentPID {
		t.Errorf("agent bounced by re-up: pid %d → %d", agentPID, snap.PID)
	}
	if !snap.ConfigChanged {
		t.Error("changed definition not flagged ConfigChanged")
	}
	// The record follows the new file: agent + watcher, in order.
	names, _ := m.ProjectHarnesses("reduit")
	if len(names) != 2 || names[0] != "reduit/agent" || names[1] != "reduit/watcher" {
		t.Errorf("ProjectHarnesses = %v, want [reduit/agent reduit/watcher]", names)
	}
}

// TestProjectUpIdempotentUnchanged: an identical re-up is a clean no-op — no
// error, no bounce, no ConfigChanged flag, and Changed=false so the daemon
// skips its config_reloaded broadcast.
func TestProjectUpIdempotentUnchanged(t *testing.T) {
	m := newTestManager(t, managerCfg(shHarness("global", loopScript, 0)))
	if _, err := m.ProjectUp("reduit", projectDefs("agent")); err != nil {
		t.Fatalf("first ProjectUp: %v", err)
	}
	waitFor(t, 3*time.Second, "agent running", func() bool {
		s, _ := m.Snapshot("reduit/agent")
		return s.State == core.StateRunning
	})
	pid := func() int { s, _ := m.Snapshot("reduit/agent"); return s.PID }()

	res, err := m.ProjectUp("reduit", projectDefs("agent"))
	if err != nil {
		t.Fatalf("identical re-up: %v", err)
	}
	if res.Changed {
		t.Error("verbatim no-op re-up flagged Changed (would broadcast for nothing)")
	}
	snap, ok := m.Snapshot("reduit/agent")
	if !ok {
		t.Fatal("agent gone after identical re-up")
	}
	if snap.PID != pid {
		t.Errorf("identical re-up bounced the process: pid %d → %d", pid, snap.PID)
	}
	if snap.ConfigChanged {
		t.Error("identical re-up flagged ConfigChanged")
	}
}

// TestProjectUpDisabledHarnessRegistersWithoutStart: SPEC-0004 REQ "Project
// File Schema" — `enabled = false` has the identical meaning it has in the
// global config: the harness registers (visible to list/ps) but is not
// started by project_up.
func TestProjectUpDisabledHarnessRegistersWithoutStart(t *testing.T) {
	m := newTestManager(t, managerCfg(shHarness("global", loopScript, 0)))
	on := shHarness("agent", loopScript, 0)
	on.Enabled = true
	off := shHarness("helper", loopScript, 0) // Enabled false: register only
	if _, err := m.ProjectUp("reduit", []core.Harness{on, off}); err != nil {
		t.Fatalf("ProjectUp: %v", err)
	}
	waitFor(t, 3*time.Second, "enabled harness running", func() bool {
		s, _ := m.Snapshot("reduit/agent")
		return s.State == core.StateRunning
	})
	snap, ok := m.Snapshot("reduit/helper")
	if !ok {
		t.Fatal("disabled harness not registered")
	}
	if snap.State != core.StateStopped || snap.Enabled {
		t.Errorf("disabled harness state=%s enabled=%v, want stopped/disabled", snap.State, snap.Enabled)
	}
	if h, ok := m.HarnessDef("reduit/helper"); !ok || h.Enabled {
		t.Errorf("HarnessDef(reduit/helper) = %+v ok=%v, want disabled definition", h, ok)
	}
}

// ---- SPEC-0004 REQ "Tear Down" ---------------------------------------------

// TestProjectDownStopsAndForgets: down stops every project harness, returns
// their names, and the daemon retains no record afterward.
func TestProjectDownStopsAndForgets(t *testing.T) {
	m := newTestManager(t, managerCfg(shHarness("global", loopScript, 0)))
	m.Start("global")
	if _, err := m.ProjectUp("reduit", projectDefs("agent", "reviewer")); err != nil {
		t.Fatalf("ProjectUp: %v", err)
	}
	waitFor(t, 3*time.Second, "project running", func() bool {
		s, _ := m.Snapshot("reduit/agent")
		return s.State == core.StateRunning
	})

	removed, err := m.ProjectDown("reduit")
	if err != nil {
		t.Fatalf("ProjectDown: %v", err)
	}
	if len(removed) != 2 || !slices.Contains(removed, "reduit/agent") || !slices.Contains(removed, "reduit/reviewer") {
		t.Errorf("removed = %v, want both reduit harnesses", removed)
	}
	if _, ok := m.Snapshot("reduit/agent"); ok {
		t.Error("reduit/agent still registered after down")
	}
	if _, ok := m.ProjectHarnesses("reduit"); ok {
		t.Error("project record survives down")
	}
	if m.ProjectOf("reduit/agent") != "" {
		t.Error("stale provenance after down")
	}
	// Globals are untouched.
	if s, ok := m.Snapshot("global"); !ok || s.State != core.StateRunning {
		t.Error("global harness disturbed by project down")
	}
	// A second down now fails: the daemon has no record (SPEC-0004).
	if _, err := m.ProjectDown("reduit"); !errors.Is(err, config.ErrUnknownProject) {
		t.Errorf("second down err = %v, want ErrUnknownProject", err)
	}
}

// TestProjectDownReleasesLogsAndExtraOut: SPEC-0004 REQ "Tear Down" — after
// down (and after a re-up drops a harness) the daemon retains no record: the
// harness's log tree is removed and its ExtraOut resource (the attach Mux) is
// dropped via the DropExtraOut hook.
func TestProjectDownReleasesLogsAndExtraOut(t *testing.T) {
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	var mu sync.Mutex
	var dropped []string
	m := NewManager(managerCfg(shHarness("global", loopScript, 0)), ManagerOptions{
		Policy:    fastPolicy(),
		StatePath: filepath.Join(dir, "state.json"),
		LogDir:    logDir,
		DropExtraOut: func(name string) {
			mu.Lock()
			dropped = append(dropped, name)
			mu.Unlock()
		},
	})
	t.Cleanup(m.Close)

	if _, err := m.ProjectUp("reduit", projectDefs("agent", "reviewer")); err != nil {
		t.Fatalf("ProjectUp: %v", err)
	}
	waitFor(t, 3*time.Second, "project logs exist", func() bool {
		_, errA := os.Stat(filepath.Join(logDir, "reduit", "agent.log"))
		_, errB := os.Stat(filepath.Join(logDir, "reduit", "reviewer.log"))
		return errA == nil && errB == nil
	})

	// A re-up that drops reviewer releases its resources but keeps agent's.
	if _, err := m.ProjectUp("reduit", projectDefs("agent")); err != nil {
		t.Fatalf("re-up: %v", err)
	}
	if _, err := os.Stat(filepath.Join(logDir, "reduit", "reviewer.log")); !os.IsNotExist(err) {
		t.Errorf("removed harness's log survives re-up: %v", err)
	}
	if _, err := os.Stat(filepath.Join(logDir, "reduit", "agent.log")); err != nil {
		t.Errorf("continuing harness's log removed by re-up: %v", err)
	}
	mu.Lock()
	droppedNow := append([]string(nil), dropped...)
	mu.Unlock()
	if !slices.Contains(droppedNow, "reduit/reviewer") || slices.Contains(droppedNow, "reduit/agent") {
		t.Errorf("dropped after re-up = %v, want reviewer only", droppedNow)
	}

	// Down removes the whole project log subtree and drops the rest.
	if _, err := m.ProjectDown("reduit"); err != nil {
		t.Fatalf("ProjectDown: %v", err)
	}
	if _, err := os.Stat(filepath.Join(logDir, "reduit")); !os.IsNotExist(err) {
		t.Errorf("project log directory survives down: %v", err)
	}
	mu.Lock()
	droppedNow = append(dropped[:0:0], dropped...)
	mu.Unlock()
	if !slices.Contains(droppedNow, "reduit/agent") {
		t.Errorf("dropped after down = %v, want reduit/agent included", droppedNow)
	}
}

// TestProjectDownUnknownProject: scenario "project_down on unknown project" —
// sentinel error, no state change.
func TestProjectDownUnknownProject(t *testing.T) {
	m := newTestManager(t, managerCfg(shHarness("global", loopScript, 0)))
	before := snapshotNames(m)
	_, err := m.ProjectDown("nope")
	if !errors.Is(err, config.ErrUnknownProject) {
		t.Fatalf("ProjectDown err = %v, want ErrUnknownProject", err)
	}
	if after := snapshotNames(m); len(after) != len(before) {
		t.Errorf("state changed on unknown-project down: %v → %v", before, after)
	}
}

// ---- ADR-0009: global reload never touches project harnesses ---------------

// TestGlobalReloadPreservesProjectHarnesses: a global config reload (which
// drops a removed global harness) leaves registered project harnesses running
// with their provenance intact.
func TestGlobalReloadPreservesProjectHarnesses(t *testing.T) {
	m := newTestManager(t, managerCfg(
		shHarness("keep", loopScript, 0),
		shHarness("drop", loopScript, 0),
	))
	if _, err := m.ProjectUp("reduit", projectDefs("agent")); err != nil {
		t.Fatalf("ProjectUp: %v", err)
	}
	waitFor(t, 3*time.Second, "agent running", func() bool {
		s, _ := m.Snapshot("reduit/agent")
		return s.State == core.StateRunning
	})

	m.Reload(managerCfg(shHarness("keep", loopScript, 0))) // "drop" removed globally

	if _, ok := m.Snapshot("drop"); ok {
		t.Error("removed global harness survived reload")
	}
	snap, ok := m.Snapshot("reduit/agent")
	if !ok {
		t.Fatal("project harness deregistered by global reload")
	}
	if snap.State != core.StateRunning {
		t.Errorf("project harness state = %s after reload, want running", snap.State)
	}
	if m.ProjectOf("reduit/agent") != "reduit" {
		t.Error("provenance lost across global reload")
	}
	if !slices.Contains(snapshotNames(m), "reduit/agent") {
		t.Error("project harness missing from render order after reload")
	}
}

// TestReloadIgnoresGlobalNameShadowingProjectHarness: defense-in-depth for the
// reverse collision — a (hand-built) global config carrying a harness whose
// name equals a registered project harness's fully-qualified name must not
// clobber the project supervisor's definition nor duplicate it in the render
// order (ADR-0009; SPEC-0004 REQ "Project Naming And Namespacing"). The
// config parser rejects "/" in names, so only an injected Config can carry one.
func TestReloadIgnoresGlobalNameShadowingProjectHarness(t *testing.T) {
	m := newTestManager(t, managerCfg(shHarness("keep", loopScript, 0)))
	if _, err := m.ProjectUp("reduit", projectDefs("agent")); err != nil {
		t.Fatalf("ProjectUp: %v", err)
	}
	waitFor(t, 3*time.Second, "agent running", func() bool {
		s, _ := m.Snapshot("reduit/agent")
		return s.State == core.StateRunning
	})

	shadow := shHarness("reduit/agent", "sleep 60", 0) // different definition
	m.Reload(managerCfg(shHarness("keep", loopScript, 0), shadow))

	names := snapshotNames(m)
	count := 0
	for _, n := range names {
		if n == "reduit/agent" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("reduit/agent appears %d times in render order %v, want exactly once", count, names)
	}
	// The project's definition wins: no staged change from the shadow global.
	snap, ok := m.Snapshot("reduit/agent")
	if !ok {
		t.Fatal("project harness lost after shadowing reload")
	}
	if snap.ConfigChanged {
		t.Error("global shadow definition staged onto the project supervisor")
	}
	if h, project, ok := m.HarnessRecord("reduit/agent"); !ok || project != "reduit" || len(h.Args) < 2 || h.Args[1] != loopScript {
		t.Errorf("HarnessRecord resolved shadow global (h=%+v project=%q ok=%v), want project definition", h, project, ok)
	}
}

// ---- ADR-0009: project harnesses never reach state.json --------------------

// TestSaveExcludesProjectHarnessesUnderChurn: Save runs concurrently with
// project up/down churn and profile switches; state.json must never contain a
// project harness entry, and the whole dance must stay race-clean under
// `go test -race`.
func TestSaveExcludesProjectHarnessesUnderChurn(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	m := NewManager(managerCfg(shHarness("global", loopScript, 0)), ManagerOptions{
		Policy:    fastPolicy(),
		StatePath: statePath,
		LogDir:    filepath.Join(dir, "logs"),
	})
	t.Cleanup(m.Close)

	// Disabled defs register/deregister without spawning processes, keeping
	// the churn loop fast.
	defs := []core.Harness{shHarness("agent", loopScript, 0)}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = m.Save()
			m.UseProfile("default")
		}
	}()
	for i := 0; i < 25; i++ {
		if _, err := m.ProjectUp("reduit", defs); err != nil {
			t.Errorf("ProjectUp #%d: %v", i, err)
			break
		}
		if _, err := m.ProjectDown("reduit"); err != nil {
			t.Errorf("ProjectDown #%d: %v", i, err)
			break
		}
	}
	close(stop)
	wg.Wait()

	// A final up + save: the registered project harness must be excluded.
	if _, err := m.ProjectUp("reduit", defs); err != nil {
		t.Fatalf("final ProjectUp: %v", err)
	}
	if err := m.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state.json: %v", err)
	}
	if strings.Contains(string(data), "reduit/") {
		t.Errorf("project harness persisted to state.json (ADR-0009):\n%s", data)
	}
	if !strings.Contains(string(data), "global") {
		t.Errorf("global harness missing from state.json:\n%s", data)
	}
}

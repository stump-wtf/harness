package supervisor

// Governing tests: SPEC-0004 REQ "Project Naming And Namespacing" (namespaced
// registration with provenance, two-project coexistence, global-name collision
// rejected with no partial state) and REQ "Project Control Operations"
// (reconcile-idempotent ProjectUp, ProjectDown stop-and-forget, unknown-project
// sentinel, invalid definitions leave nothing behind); ADR-0009.

import (
	"errors"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// loopScript is a harness body that stays up until stopped.
const loopScript = "while true; do sleep 0.02; done"

// projectDefs builds project-local looping harness definitions.
func projectDefs(names ...string) []core.Harness {
	out := make([]core.Harness, 0, len(names))
	for _, n := range names {
		out = append(out, shHarness(n, loopScript, 0))
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

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// ---- SPEC-0004 REQ "Project Naming And Namespacing" -----------------------

// TestProjectUpRegistersNamespacedWithProvenance: harnesses register
// daemon-wide as <project>/<harness> with project provenance; globals stay
// bare with empty provenance.
func TestProjectUpRegistersNamespacedWithProvenance(t *testing.T) {
	m := newTestManager(t, managerCfg(shHarness("global", loopScript, 0)))
	if err := m.ProjectUp("reduit", projectDefs("agent", "reviewer")); err != nil {
		t.Fatalf("ProjectUp: %v", err)
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
	// Definitions are resolvable under the full name (drives describe/list).
	if h, ok := m.HarnessDef("reduit/agent"); !ok || h.Cmd != "sh" {
		t.Errorf("HarnessDef(reduit/agent) = %+v ok=%v, want sh definition", h, ok)
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
	if err := m.ProjectUp("reduit", projectDefs("agent")); err != nil {
		t.Fatalf("ProjectUp reduit: %v", err)
	}
	if err := m.ProjectUp("spotter", projectDefs("agent")); err != nil {
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

	err := m.ProjectUp("reduit", projectDefs("agent", "reviewer"))
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
		{"no harnesses", "reduit", nil},
		{"empty harness name", "reduit", []core.Harness{shHarness("", loopScript, 0)}},
		{"slash in harness name", "reduit", []core.Harness{shHarness("a/b", loopScript, 0)}},
		{"duplicate harness", "reduit", projectDefs("agent", "agent")},
		{"missing cmd", "reduit", []core.Harness{{Name: "agent", Backend: core.BackendNative}}},
		{"invalid backend", "reduit", []core.Harness{{Name: "agent", Cmd: "sh", Backend: "bogus"}}},
		{"negative restart delay", "reduit", []core.Harness{{
			Name: "agent", Cmd: "sh", Backend: core.BackendNative, RestartDelay: -time.Second,
		}}},
		// The invalid entry is third: earlier valid entries must not stick.
		{"invalid mid-list", "reduit", []core.Harness{
			shHarness("a", loopScript, 0),
			shHarness("b", loopScript, 0),
			{Name: "c", Backend: core.BackendNative}, // missing cmd
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newTestManager(t, managerCfg(shHarness("global", loopScript, 0)))
			err := m.ProjectUp(tc.project, tc.defs)
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

// ---- SPEC-0004 REQ "Project Control Operations": reconcile ----------------

// TestProjectUpReconciles: re-up adds new harnesses, stops + deregisters
// removed ones, and stages changed definitions without bouncing the running
// process (SPEC-0003 REQ "Config Change Application").
func TestProjectUpReconciles(t *testing.T) {
	m := newTestManager(t, managerCfg(shHarness("global", loopScript, 0)))
	if err := m.ProjectUp("reduit", projectDefs("agent", "reviewer")); err != nil {
		t.Fatalf("first ProjectUp: %v", err)
	}
	waitFor(t, 3*time.Second, "agent running", func() bool {
		s, _ := m.Snapshot("reduit/agent")
		return s.State == core.StateRunning
	})
	agentPID := func() int { s, _ := m.Snapshot("reduit/agent"); return s.PID }()

	// Re-up: agent changes (new args), reviewer is gone, watcher is new.
	changed := shHarness("agent", "while true; do sleep 0.05; done", 0)
	if err := m.ProjectUp("reduit", []core.Harness{changed, shHarness("watcher", loopScript, 0)}); err != nil {
		t.Fatalf("re-up: %v", err)
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
// error, no bounce, no ConfigChanged flag.
func TestProjectUpIdempotentUnchanged(t *testing.T) {
	m := newTestManager(t, managerCfg(shHarness("global", loopScript, 0)))
	if err := m.ProjectUp("reduit", projectDefs("agent")); err != nil {
		t.Fatalf("first ProjectUp: %v", err)
	}
	waitFor(t, 3*time.Second, "agent running", func() bool {
		s, _ := m.Snapshot("reduit/agent")
		return s.State == core.StateRunning
	})
	pid := func() int { s, _ := m.Snapshot("reduit/agent"); return s.PID }()

	if err := m.ProjectUp("reduit", projectDefs("agent")); err != nil {
		t.Fatalf("identical re-up: %v", err)
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

// ---- SPEC-0004 REQ "Tear Down" ---------------------------------------------

// TestProjectDownStopsAndForgets: down stops every project harness, returns
// their names, and the daemon retains no record afterward.
func TestProjectDownStopsAndForgets(t *testing.T) {
	m := newTestManager(t, managerCfg(shHarness("global", loopScript, 0)))
	m.Start("global")
	if err := m.ProjectUp("reduit", projectDefs("agent", "reviewer")); err != nil {
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
	if len(removed) != 2 || !contains(removed, "reduit/agent") || !contains(removed, "reduit/reviewer") {
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
	if err := m.ProjectUp("reduit", projectDefs("agent")); err != nil {
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
	if !contains(snapshotNames(m), "reduit/agent") {
		t.Error("project harness missing from render order after reload")
	}
}

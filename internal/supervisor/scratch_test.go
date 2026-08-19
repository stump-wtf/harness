package supervisor

// Governing tests: SPEC-0011 REQ "Scratchpad Creation" (register + start under
// a minted name), REQ "Name Minting" (slug shape, suffix, collision-free
// concurrent runs), REQ "Ephemerality (No Persistence)" (state.json never
// contains a scratchpad), REQ "Teardown" (RemoveHarness accepts scratch
// provenance). ADR-0017.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// scratchDef builds a scratchpad definition with the slug passed separately,
// mirroring the daemon's call shape.
func scratchDef(adapter string, args ...string) core.Harness {
	return core.Harness{Adapter: adapter, Args: args, Enabled: true, Restart: core.RestartNo}
}

func TestScratchRunMintsNameAndStarts(t *testing.T) {
	m := newTestManager(t, managerCfg(shHarness("global", loopScript, 0)))
	name, err := m.ScratchRun(scratchDef("generic", "-c", loopScript), "claude-opus-5")
	if err != nil {
		t.Fatalf("ScratchRun: %v", err)
	}
	if !strings.HasPrefix(name, "claude-opus-5-") || len(name) != len("claude-opus-5-")+4 {
		t.Errorf("minted name %q, want claude-opus-5-<4 chars>", name)
	}
	if got := m.ProjectOf(name); got != ProvenanceScratch {
		t.Errorf("ProjectOf = %q, want %q", got, ProvenanceScratch)
	}
	waitFor(t, 3*time.Second, "scratchpad running", func() bool {
		s, _ := m.Snapshot(name)
		return s.State == core.StateRunning
	})
}

func TestScratchRunSlugify(t *testing.T) {
	m := newTestManager(t, managerCfg())
	if _, err := m.ScratchRun(scratchDef("generic", "-c", loopScript), "My Cool -- Slug!!"); err != nil {
		t.Fatalf("ScratchRun: %v", err)
	}
	found := false
	for _, n := range snapshotNames(m) {
		if strings.HasPrefix(n, "my-cool-slug-") {
			found = true
		}
	}
	if !found {
		t.Errorf("sanitized slug not found in %v", snapshotNames(m))
	}
}

func TestScratchRunConcurrentNeverCollides(t *testing.T) {
	m := newTestManager(t, managerCfg())
	const n = 16
	names := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			name, err := m.ScratchRun(scratchDef("generic", "-c", loopScript), "same")
			if err != nil {
				t.Errorf("ScratchRun: %v", err)
				return
			}
			names <- name
		}()
	}
	wg.Wait()
	close(names)
	seen := map[string]bool{}
	for name := range names {
		if seen[name] {
			t.Fatalf("duplicate minted name %q", name)
		}
		seen[name] = true
	}
}

func TestScratchpadNeverPersisted(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	m := NewManager(managerCfg(shHarness("global", loopScript, 0)), ManagerOptions{
		Policy:    fastPolicy(),
		StatePath: statePath,
		LogDir:    filepath.Join(dir, "logs"),
	})
	if _, err := m.ScratchRun(scratchDef("generic", "-c", loopScript), "claude-opus-5"); err != nil {
		t.Fatalf("ScratchRun: %v", err)
	}
	if err := m.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	data, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatalf("read state.json: %v", err)
	}
	if strings.Contains(string(data), "claude-opus-5") {
		t.Errorf("scratchpad leaked into state.json (ADR-0017):\n%s", data)
	}
	if !strings.Contains(string(data), "global") {
		t.Errorf("global harness missing from state.json:\n%s", data)
	}
	// And a fresh manager restores no scratchpad.
	m2 := NewManager(managerCfg(shHarness("global", loopScript, 0)), ManagerOptions{
		Policy:    fastPolicy(),
		StatePath: statePath,
		LogDir:    filepath.Join(dir, "logs"),
	})
	t.Cleanup(m2.Close)
	if err := m2.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for _, n := range snapshotNames(m2) {
		if strings.HasPrefix(n, "claude-opus-5") {
			t.Errorf("scratchpad restored across managers: %q", n)
		}
	}
}

func TestScratchpadRemoveAndRestartPolicy(t *testing.T) {
	m := newTestManager(t, managerCfg())
	name, err := m.ScratchRun(scratchDef("generic", "-c", loopScript), "pad")
	if err != nil {
		t.Fatalf("ScratchRun: %v", err)
	}
	// Session semantics: the default policy forced on a definition that
	// arrived without one.
	if h, _, ok := m.HarnessRecord(name); !ok || h.Restart != core.RestartNo {
		t.Errorf("scratchpad restart policy = %q, want no", h.Restart)
	}
	if err := m.RemoveHarness(name); err != nil {
		t.Fatalf("RemoveHarness(scratchpad): %v", err)
	}
	if _, ok := m.Snapshot(name); ok {
		t.Error("scratchpad still registered after rm")
	}
}

func TestScratchRunInvalidDefinition(t *testing.T) {
	m := newTestManager(t, managerCfg())
	// Unknown kind → structured refusal, nothing registered.
	if _, err := m.ScratchRun(core.Harness{Adapter: "nope", Args: []string{"x"}}, "bad"); err == nil {
		t.Fatal("unknown kind accepted")
	}
	// Empty slug → refused.
	if _, err := m.ScratchRun(scratchDef("generic", "-c", loopScript), "  "); err == nil {
		t.Fatal("empty slug accepted")
	}
	if got := snapshotNames(m); len(got) != 0 {
		t.Errorf("invalid scratch runs registered something: %v", got)
	}
}

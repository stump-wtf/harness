package supervisor

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/stump-wtf/harness/internal/core"
)

// SPEC-0018 REQ-12: the env_file list round-trips through state.json, and a
// state file written by an older daemon (single-path form) still restores.
// Only paths are persisted, never values (ADR-0008).
func TestPersistedProjectHarnessEnvFileList(t *testing.T) {
	h := core.Harness{
		Name:     "reviewer",
		Adapter:  "claude-code",
		EnvFiles: []string{"claude.env", "reviewer.env"},
	}

	p := toPersistedProjectHarness(h)
	if p.EnvFile != "" {
		t.Errorf("new write also set the legacy single-path field: %q", p.EnvFile)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := roundtripToCore(t, raw); !slices.Equal(got.EnvFiles, h.EnvFiles) {
		t.Errorf("EnvFiles = %q, want %q", got.EnvFiles, h.EnvFiles)
	}

	// An older state.json carries only the single-path form.
	legacy := []byte(`{"name":"solo","harness":"claude-code","env_file":"secrets.env"}`)
	got := roundtripToCore(t, legacy)
	want := []string{"secrets.env"}
	if !slices.Equal(got.EnvFiles, want) {
		t.Errorf("legacy EnvFiles = %q, want %q", got.EnvFiles, want)
	}

	// A harness with no env_file restores to nil, not a one-blank list.
	bare := roundtripToCore(t, []byte(`{"name":"bare","harness":"claude-code"}`))
	if bare.EnvFiles != nil {
		t.Errorf("bare EnvFiles = %q, want nil", bare.EnvFiles)
	}
}

func roundtripToCore(t *testing.T, raw []byte) core.Harness {
	t.Helper()
	var p persistedProjectHarness
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	return p.toCore()
}

// REQ-12: a list state.json survives an actual save/load cycle to disk, and
// no env value — only paths — reaches the file (ADR-0008).
func TestSaveStateEnvFileListKeepsPathsOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	ps := persistedState{}
	ps.Projects = map[string]persistedProject{
		"proj": {
			Harnesses: []persistedProjectHarness{
				toPersistedProjectHarness(core.Harness{
					Name:     "proj/reviewer",
					Adapter:  "claude-code",
					EnvFiles: []string{"/tmp/secrets/claude.env", "/tmp/secrets/reviewer.env"},
				}),
			},
		},
	}
	saveState(path, ps)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("SHARED_TOKEN")) {
		t.Error("an env value reached state.json (ADR-0008)")
	}
	loaded, err := loadState(path)
	if err != nil {
		t.Fatal(err)
	}
	ph := loaded.Projects["proj"].Harnesses[0]
	want := []string{"/tmp/secrets/claude.env", "/tmp/secrets/reviewer.env"}
	if !slices.Equal(ph.EnvFiles, want) {
		t.Errorf("EnvFiles after save/load = %q, want %q", ph.EnvFiles, want)
	}
}

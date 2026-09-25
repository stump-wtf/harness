package supervisor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/core"
)

// Governing: ADR-0008; SPEC-0018 REQ-12. A list of env files merges in
// order, and a later file wins a key collision — that is the whole point of
// the shared claude.env under a per-persona file.

func TestEnvFileListLaterFileWins(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "claude.env")
	persona := filepath.Join(dir, "reviewer.env")
	// A missing shared file is tolerated, exactly as the single-file form is.
	if err := os.WriteFile(shared, []byte("CLAUDE_CODE_OAUTH_TOKEN=shared\nONLY_IN_SHARED=1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(persona, []byte("CLAUDE_CODE_OAUTH_TOKEN=persona\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := core.Harness{Name: "x", EnvFiles: []string{shared, filepath.Join(dir, "does-not-exist.env"), persona}}
	got, err := buildEnv(h, RunEnv{})
	if err != nil {
		t.Fatal(err)
	}
	last := map[string]string{}
	for _, kv := range got {
		if k, v, ok := strings.Cut(kv, "="); ok {
			last[k] = v
		}
	}
	if last["CLAUDE_CODE_OAUTH_TOKEN"] != "persona" {
		t.Errorf("later file must win: got %q", last["CLAUDE_CODE_OAUTH_TOKEN"])
	}
	if last["ONLY_IN_SHARED"] != "1" {
		t.Errorf("earlier file must still contribute: got %q", last["ONLY_IN_SHARED"])
	}
}

func TestDiscoveryEnvReadsMergedList(t *testing.T) {
	dir := t.TempDir()
	shared := filepath.Join(dir, "claude.env")
	persona := filepath.Join(dir, "reviewer.env")
	if err := os.WriteFile(shared, []byte("CRUSH_GLOBAL_DATA=/shared\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(persona, []byte("CRUSH_GLOBAL_DATA=/persona\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := core.Harness{Name: "x", EnvFiles: []string{shared, persona}}
	got, err := DiscoveryEnv(h, []string{"CRUSH_GLOBAL_DATA"})
	if err != nil {
		t.Fatal(err)
	}
	if got["CRUSH_GLOBAL_DATA"] != "/persona" {
		t.Errorf("DiscoveryEnv = %v, want the later file's value", got)
	}
}

func TestEnvFileListPersistsAndRestores(t *testing.T) {
	h := core.Harness{Name: "x", EnvFiles: []string{"/a/claude.env", "/a/reviewer.env"}}
	back := toPersistedProjectHarness(h).toCore()
	if len(back.EnvFiles) != 2 || back.EnvFiles[1] != "/a/reviewer.env" {
		t.Fatalf("list did not round-trip: %v", back.EnvFiles)
	}
	// A single file ALSO writes the legacy string field, so an older daemon
	// (or a downgrade) loses nothing.
	single := core.Harness{Name: "x", EnvFiles: []string{"/a/claude.env"}}
	raw, err := json.Marshal(toPersistedProjectHarness(single))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"env_file":"/a/claude.env"`) {
		t.Errorf("legacy field missing for single file: %s", raw)
	}
	// A state written by an older daemon carries only the string field.
	var p persistedProjectHarness
	if err := json.Unmarshal([]byte(`{"name":"x","env_file":"/old/path.env"}`), &p); err != nil {
		t.Fatal(err)
	}
	got := p.toCore()
	if len(got.EnvFiles) != 1 || got.EnvFiles[0] != "/old/path.env" {
		t.Errorf("legacy string not restored: %v", got.EnvFiles)
	}
}

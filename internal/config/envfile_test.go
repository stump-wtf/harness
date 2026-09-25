package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Governing: ADR-0008; SPEC-0018 REQ-12. env_file accepts a string or a
// non-empty list, merged in order with a later file winning a collision.

func TestEnvFileListParsesInOrder(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	shared := write("claude.env", "CLAUDE_CODE_OAUTH_TOKEN=shared\n")
	persona := write("reviewer.env", "CLAUDE_CODE_OAUTH_TOKEN=persona\nSWITCHBOARD_TOKEN=sbk\n")
	cfgPath := filepath.Join(dir, "harness.toml")
	src := "[harness.reviewer]\nharness = \"crush\"\nworkdir = \"/tmp\"\nenv_file = [\"" + shared + "\", \"" + persona + "\"]\n"
	if err := os.WriteFile(cfgPath, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.Harnesses["reviewer"]
	if len(h.EnvFiles) != 2 || h.EnvFiles[0] != shared || h.EnvFiles[1] != persona {
		t.Fatalf("EnvFiles = %v, want [%q %q]", h.EnvFiles, shared, persona)
	}
}

func TestEnvFileEmptyListIsLocatedError(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "harness.toml")
	// Two leading comments so the error's line number points at the key.
	src := "# a harness\n# with an emptied env_file\n[harness.reviewer]\nharness = \"crush\"\nworkdir = \"/tmp\"\nenv_file = []\n"
	if err := os.WriteFile(cfgPath, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(cfgPath)
	if err == nil {
		t.Fatal("env_file = [] must be a load error")
	}
	if !strings.Contains(err.Error(), "reviewer") || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("error must name the harness and the key: %v", err)
	}
	var loc *Error
	if !errors.As(err, &loc) || loc.Line != 3 {
		t.Fatalf("error must be located at the harness table: %v", err)
	}
}

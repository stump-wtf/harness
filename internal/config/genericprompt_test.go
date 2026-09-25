package config

// Generic Prompt Rejection Tests
//
// `generic` runs sh and has no prompt synthesis. It used to borrow Crush's,
// so `generic` + `prompt` loaded cleanly and then ran `crush run <prompt>`.
// Each config front door now refuses the combination with a located error
// naming the harness.
//
// Governing: ADR-0023, SPEC-0017 REQ "Generic Kind Rejects Prompts".

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// assertGenericPromptError checks err is the located refusal for harness name
// declared at file:line, naming the key that carried the prompt.
func assertGenericPromptError(t *testing.T, err error, file string, line int, name, key string) {
	t.Helper()
	if err == nil {
		t.Fatal("a generic harness with a prompt loaded; want a validation error")
	}
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("error is %T, want *config.Error: %v", err, err)
	}
	if ce.File != file || ce.Line != line {
		t.Errorf("error located at %s:%d, want %s:%d (err: %v)", ce.File, ce.Line, file, line, err)
	}
	for _, want := range []string{
		`harness "` + name + `"`,
		`"generic" runs sh and has no prompt synthesis`,
		`takes no "` + key + `"`,
		`harness = "crush"|"claude-code"|"codex"`,
	} {
		if !strings.Contains(ce.Msg, want) {
			t.Errorf("message %q does not contain %q", ce.Msg, want)
		}
	}
}

// writeTriagePrompt writes an instruction file that exists and is valid, so the
// only thing wrong with a harness naming it is its kind.
func writeTriagePrompt(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "triage.md")
	if err := os.WriteFile(p, []byte("triage the queue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestGenericPromptRejectedInGlobalConfig covers the scenarios "Generic with
// a prompt fails the load" and "Generic with a prompt file fails the load" for
// the global harness.toml, through the real Load path.
func TestGenericPromptRejectedInGlobalConfig(t *testing.T) {
	for _, tc := range []struct{ key, stanza string }{
		{"prompt", `prompt = "triage the queue"`},
		{"prompt_file", `prompt_file = "triage.md"`},
	} {
		t.Run(tc.key, func(t *testing.T) {
			dir := t.TempDir()
			writeTriagePrompt(t, dir)
			path := writeCfg(t, dir, "[harness.ok]\nharness = \"generic\"\nargs = [\"-c\", \"true\"]\n\n"+
				"[harness.triage]\nharness = \"generic\"\n"+tc.stanza+"\n")
			_, err := Load(path)
			assertGenericPromptError(t, err, path, 5, "triage", tc.key)
		})
	}
}

// TestGenericPromptRejectedInDropIn: a harness_d drop-in is the same config
// view, so the refusal is located in the drop-in file, not the main one.
func TestGenericPromptRejectedInDropIn(t *testing.T) {
	for _, tc := range []struct{ key, stanza string }{
		{"prompt", `prompt = "triage the queue"`},
		{"prompt_file", `prompt_file = "triage.md"`},
	} {
		t.Run(tc.key, func(t *testing.T) {
			dir := t.TempDir()
			dropIn := filepath.Join(dir, "harness.d")
			if err := os.MkdirAll(dropIn, 0o755); err != nil {
				t.Fatal(err)
			}
			writeTriagePrompt(t, dropIn)
			dropInPath := filepath.Join(dropIn, "triage.toml")
			if err := os.WriteFile(dropInPath, []byte("[harness.triage]\nharness = \"generic\"\n"+tc.stanza+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := writeCfg(t, dir, "[server]\nharness_d = \"harness.d\"\n")
			_, err := Load(path)
			assertGenericPromptError(t, err, dropInPath, 1, "triage", tc.key)
		})
	}
}

// TestGenericPromptRejectedInProjectFile: a project harness.toml shares
// registerHarness with the global parser, and refuses the same way.
func TestGenericPromptRejectedInProjectFile(t *testing.T) {
	for _, tc := range []struct{ key, stanza string }{
		{"prompt", `prompt = "triage the queue"`},
		{"prompt_file", `prompt_file = "triage.md"`},
	} {
		t.Run(tc.key, func(t *testing.T) {
			dir := t.TempDir()
			writeTriagePrompt(t, dir)
			path := filepath.Join(dir, "harness.toml")
			if err := os.WriteFile(path, []byte("[harness.triage]\nharness = \"generic\"\n"+tc.stanza+"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadProject(path)
			assertGenericPromptError(t, err, path, 1, "triage", tc.key)
		})
	}
}

// TestGenericPromptFileRefusedBeforeItIsRead: the kind check comes before
// prompt_file is resolved, so a generic harness naming a file that does not
// exist is refused for its kind, not reported as a missing file.
func TestGenericPromptFileRefusedBeforeItIsRead(t *testing.T) {
	dir := t.TempDir()
	path := writeCfg(t, dir, "[harness.triage]\nharness = \"generic\"\nprompt_file = \"missing.md\"\n")
	_, err := Load(path)
	assertGenericPromptError(t, err, path, 1, "triage", "prompt_file")
}

// TestResidentGenericUnchanged is the scenario "Resident generic is
// unchanged": no prompt, args through sh, loads as before.
func TestResidentGenericUnchanged(t *testing.T) {
	cfg, err := Parse([]byte("[harness.clock]\nharness = \"generic\"\nargs = [\"-c\", \"while true; do date; sleep 5; done\"]\n"), "t.toml")
	if err != nil {
		t.Fatalf("resident generic no longer loads: %v", err)
	}
	h := cfg.Harnesses["clock"]
	if h.Adapter != "generic" || len(h.Args) != 2 || h.Prompt != "" {
		t.Fatalf("clock = %+v, want generic with its two args and no prompt", h)
	}
}

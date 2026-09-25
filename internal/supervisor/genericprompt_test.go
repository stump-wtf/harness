package supervisor

// Generic Prompt Refusal Tests
//
// `generic` runs sh and has no prompt synthesis. Its PromptCommand used to
// delegate to Crush, so a `generic` harness carrying a prompt ran `crush run
// <prompt>` for an operator who never configured Crush. Config validation and
// the wire now reject the combination; these tests pin the spawn backstop for
// any path that bypassed them.
//
// Governing: ADR-0023, SPEC-0017 REQ "Generic Kind Rejects Prompts".

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/adapter"
	"github.com/stump-wtf/harness/internal/core"
)

// fakeCrushOnPath puts an executable named crush first on PATH. Running it
// creates the returned marker file, so the marker existing is evidence that
// crush was exec'd, whatever argv it got.
func fakeCrushOnPath(t *testing.T) (marker string) {
	t.Helper()
	dir := t.TempDir()
	marker = filepath.Join(dir, "crush-ran")
	script := "#!/bin/sh\n: > '" + marker + "'\n"
	if err := os.WriteFile(filepath.Join(dir, "crush"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

// TestSpawnRefusesGenericPrompt is the scenario "No path produces Crush's
// argv": a prompt on a harness whose kind has no prompt synthesis fails the
// start with an error naming the harness, and nothing is exec'd. The empty and
// unknown kinds ride along because Resolve maps both to Generic.
//
// The positive control spawns the same prompt on `crush` first and requires
// the marker to appear, so the marker's absence afterwards means crush did not
// run, not that the probe cannot see it. Restoring Generic's delegation to
// Crush fails this test: execArgv then gets a non-empty argv and spawn starts
// the fake crush.
func TestSpawnRefusesGenericPrompt(t *testing.T) {
	marker := fakeCrushOnPath(t)

	// Positive control: the probe can see crush run.
	proc, err := spawn(core.Harness{Name: "control", Adapter: "crush", Prompt: "x"}, 80, 24, RunEnv{})
	if err != nil {
		t.Fatalf("control spawn of a crush prompt harness: %v", err)
	}
	_ = proc.cmd.Wait()
	_ = proc.pty.Close()
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("control: fake crush never ran, so the probe cannot detect an exec: %v", err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}

	promptFile := filepath.Join(t.TempDir(), "prompt.md")
	if err := os.WriteFile(promptFile, []byte("triage the queue\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, h := range []core.Harness{
		{Name: "gen-inline", Adapter: "generic", Prompt: "triage the queue"},
		{Name: "gen-file", Adapter: "generic", PromptFile: promptFile},
		{Name: "gen-opts", Adapter: "generic", Prompt: "x", Model: "m", AutoAccept: true, MaxTurns: 2, Quiet: true},
		{Name: "no-kind", Prompt: "x"},
		{Name: "bad-kind", Adapter: "nope", Prompt: "x"},
	} {
		t.Run(h.Name, func(t *testing.T) {
			proc, err := spawn(h, 80, 24, RunEnv{})
			if proc != nil {
				_ = proc.cmd.Wait()
				_ = proc.pty.Close()
				t.Fatalf("spawn started %q for a %q harness with a prompt; want a refusal", proc.cmd.Path, h.Adapter)
			}
			if !errors.Is(err, ErrGenericPrompt) {
				t.Fatalf("spawn error = %v, want ErrGenericPrompt", err)
			}
			if !strings.Contains(err.Error(), `"`+h.Name+`"`) {
				t.Errorf("error %q does not name the harness %q", err, h.Name)
			}
			if _, statErr := os.Stat(marker); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("crush was exec'd for a %q harness (marker stat: %v)", h.Adapter, statErr)
			}
		})
	}
}

// TestExecArgvResidentGenericUnchanged is the scenario "Resident generic is
// unchanged": without a prompt, generic still runs sh with the configured args.
func TestExecArgvResidentGenericUnchanged(t *testing.T) {
	h := core.Harness{
		Name:    "clock",
		Adapter: "generic",
		Args:    []string{"-c", "while true; do date; sleep 5; done"},
	}
	name, args := mustExecArgv(t, h, "/home/x")
	if name != "sh" {
		t.Errorf("cmd = %q, want sh", name)
	}
	if !slices.Equal(args, h.Args) {
		t.Errorf("args = %q, want %q", args, h.Args)
	}
}

// mustExecArgv is execArgv for a harness the test expects to resolve: an
// error fails the test instead of leaving an empty argv to assert against.
func mustExecArgv(t *testing.T, h core.Harness, workdir string) (string, []string) {
	t.Helper()
	return mustExecArgvReg(t, h, workdir, adapter.NewRegistryWithDefaults())
}

// mustExecArgvReg is mustExecArgv against a caller-supplied registry.
func mustExecArgvReg(t *testing.T, h core.Harness, workdir string, reg *adapter.Registry) (string, []string) {
	t.Helper()
	name, args, err := execArgvWithRegistry(h, workdir, reg)
	if err != nil {
		t.Fatalf("execArgv(%q): %v", h.Name, err)
	}
	return name, args
}

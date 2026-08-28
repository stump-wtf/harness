package supervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/harness/internal/adapter"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// TestResolvePromptReadsAtSpawn: a prompt_file harness spawns with the file's
// CONTENTS as its instruction, and the resolution is local — the returned copy
// carries the text while PromptFile is cleared, so nothing writes the document
// back into config truth.
// Governing: ADR-0018; SPEC-0006 REQ "Prompt Source".
func TestResolvePromptReadsAtSpawn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sweep.prompt.md")
	if err := os.WriteFile(path, []byte("  sweep the fleet  \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := core.Harness{Name: "sweep", Adapter: "claude-code", PromptFile: path}

	got, err := resolvePrompt(h)
	if err != nil {
		t.Fatalf("resolvePrompt: %v", err)
	}
	if got.Prompt != "sweep the fleet" {
		t.Errorf("Prompt = %q, want the file's trimmed contents", got.Prompt)
	}
	if got.PromptFile != "" {
		t.Errorf("PromptFile = %q, want it cleared once resolved", got.PromptFile)
	}
	// The caller's harness must be untouched: spawn works on a copy, so the
	// registry keeps the path and later writers keep round-tripping it.
	if h.Prompt != "" || h.PromptFile != path {
		t.Errorf("resolvePrompt mutated its argument: %+v", h)
	}

	name, args := execArgvWithRegistry(got, "/home/x", adapter.NewRegistryWithDefaults())
	if name != "claude" {
		t.Errorf("exec = %q, want claude", name)
	}
	if len(args) == 0 || args[len(args)-1] != "sweep the fleet" {
		t.Errorf("argv = %v, want the file's contents as the final element", args)
	}
}

// TestResolvePromptLeavesInlineAndCmdHarnessesAlone: the resolver is a no-op
// for the two shapes that carry no prompt_file, so it cannot perturb an inline
// prompt or a long-running cmd harness.
func TestResolvePromptLeavesInlineAndCmdHarnessesAlone(t *testing.T) {
	for _, h := range []core.Harness{
		{Name: "inline", Adapter: "crush", Prompt: "sweep"},
		{Name: "repl", Adapter: "generic", Args: []string{"-c", "sleep 1"}},
	} {
		got, err := resolvePrompt(h)
		if err != nil {
			t.Fatalf("%s: resolvePrompt: %v", h.Name, err)
		}
		if got.Prompt != h.Prompt || got.PromptFile != "" {
			t.Errorf("%s: resolvePrompt changed a harness with no prompt_file: %+v", h.Name, got)
		}
	}
}

// TestResolvePromptFailsWhenFileVanishes is the reason spawn re-checks what the
// config parser already validated: the file can be deleted between load and
// firing, and launching an agent with an empty instruction is the silent no-op
// this feature exists to remove. The error must name the harness and the path.
func TestResolvePromptFailsWhenFileVanishes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sweep.prompt.md")
	if err := os.WriteFile(path, []byte("sweep"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := core.Harness{Name: "sweep", Adapter: "claude-code", PromptFile: path}
	if _, err := resolvePrompt(h); err != nil {
		t.Fatalf("resolvePrompt on a present file: %v", err)
	}

	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	_, err := resolvePrompt(h)
	if err == nil {
		t.Fatal("resolvePrompt succeeded on a deleted file, want an error")
	}
	if !strings.Contains(err.Error(), "sweep") || !strings.Contains(err.Error(), path) {
		t.Errorf("error %q does not name both the harness and the path", err)
	}
}

// TestResolvePromptRejectsEmptyFile: an empty instruction file is an error, not
// an empty prompt — otherwise the agent launches with nothing to do and the run
// looks successful.
func TestResolvePromptRejectsEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sweep.prompt.md")
	if err := os.WriteFile(path, []byte("\n \t\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := resolvePrompt(core.Harness{Name: "sweep", PromptFile: path})
	if err == nil {
		t.Fatal("resolvePrompt succeeded on a whitespace-only file, want an error")
	}
	if !strings.Contains(err.Error(), "is empty") {
		t.Errorf("error %q does not say the file is empty", err)
	}
}

// TestResolvePromptSeesEditsWithoutReload pins the behavior that motivates
// reading per spawn rather than per config load: editing the referenced file
// changes the next run, with no reload in between.
// Governing: SPEC-0006 REQ "Prompt Source" (edited prompt file scenario).
func TestResolvePromptSeesEditsWithoutReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sweep.prompt.md")
	if err := os.WriteFile(path, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	// One harness value, as the registry would hold it across both runs.
	h := core.Harness{Name: "sweep", Adapter: "claude-code", PromptFile: path}

	first, err := resolvePrompt(h)
	if err != nil {
		t.Fatalf("resolvePrompt: %v", err)
	}
	if err := os.WriteFile(path, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := resolvePrompt(h)
	if err != nil {
		t.Fatalf("resolvePrompt after edit: %v", err)
	}
	if first.Prompt != "first" || second.Prompt != "second" {
		t.Errorf("prompts = %q then %q, want the file's contents at each spawn",
			first.Prompt, second.Prompt)
	}
}

// TestResolvePromptExpandsHome: a stored ~ path (a wire def or an older state
// file can carry one) is expanded by the same rule workdir and env_file use,
// rather than being opened literally and reported missing.
func TestResolvePromptExpandsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	f, err := os.CreateTemp(home, ".harness-prompt-test-*.md")
	if err != nil {
		t.Skipf("cannot write to home: %v", err)
	}
	defer os.Remove(f.Name())
	if _, err := f.WriteString("sweep"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	got, err := resolvePrompt(core.Harness{
		Name:       "sweep",
		PromptFile: filepath.Join("~", filepath.Base(f.Name())),
	})
	if err != nil {
		t.Fatalf("resolvePrompt on a ~ path: %v", err)
	}
	if got.Prompt != "sweep" {
		t.Errorf("Prompt = %q, want the file's contents", got.Prompt)
	}
}

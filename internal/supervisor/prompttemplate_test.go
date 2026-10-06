package supervisor

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/adapter"
	"github.com/stump-wtf/harness/internal/core"
)

// TestResolvePromptTemplateExpands: a prompt_template harness spawns with the
// RENDERED text as its instruction, and the resolution is local — the copy
// carries the rendered text while PromptTemplate is cleared, so config truth
// stays the template as written (REQ-15).
// Governing: SPEC-0017 REQ-5, REQ-7, REQ-11.
func TestResolvePromptTemplateExpands(t *testing.T) {
	h := core.Harness{
		Name:           "sweep",
		Adapter:        "claude-code",
		PromptTemplate: "sweep {{harness.name}} for run {{run.id}} via {{run.trigger?}}",
	}
	run := RunEnv{RunID: 7, Trigger: "schedule", StartedAt: time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)}

	got, err := resolvePrompt(h, "/home/x", run)
	if err != nil {
		t.Fatalf("resolvePrompt: %v", err)
	}
	if got.Prompt != "sweep sweep for run 7 via schedule" {
		t.Errorf("Prompt = %q, want the rendered template", got.Prompt)
	}
	if got.PromptTemplate != "" {
		t.Errorf("PromptTemplate = %q, want it cleared once rendered", got.PromptTemplate)
	}
	if h.PromptTemplate != "sweep {{harness.name}} for run {{run.id}} via {{run.trigger?}}" {
		t.Errorf("resolvePrompt mutated its argument: %+v", h)
	}

	name, args := mustExecArgvReg(t, got, "/home/x", adapter.NewRegistryWithDefaults())
	if name != "claude" {
		t.Errorf("exec = %q, want claude", name)
	}
	if len(args) == 0 || args[len(args)-1] != "sweep sweep for run 7 via schedule" {
		t.Errorf("argv = %v, want the rendered text as the final element", args)
	}
}

// TestResolvePromptTemplateFileReadsAtSpawn: prompt_template_file is read AND
// re-parsed per spawn (REQ-5), and a file edited into a grammar error after
// load fails the start with an error naming the file — an agent launched with
// a broken instruction would be a silent no-op.
func TestResolvePromptTemplateFileEditedIntoError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sweep.tmpl.md")
	if err := os.WriteFile(path, []byte("sweep {{harness.name}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := core.Harness{Name: "sweep", Adapter: "claude-code", PromptTemplateFile: path}

	first, err := resolvePrompt(h, "/home/x", RunEnv{})
	if err != nil {
		t.Fatalf("first resolvePrompt: %v", err)
	}
	if first.Prompt != "sweep sweep" {
		t.Errorf("first Prompt = %q, want the rendered file", first.Prompt)
	}

	if err := os.WriteFile(path, []byte("sweep {{run.idd}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = resolvePrompt(h, "/home/x", RunEnv{})
	if err == nil {
		t.Fatal("resolvePrompt succeeded on a template edited into a grammar error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the file: %v", err)
	}
}

// TestResolvePromptTemplateFileVanished: the per-spawn read is a hard error
// when the file is gone, exactly as prompt_file is (REQ-5 follows prompt_file's
// rules).
func TestResolvePromptTemplateFileVanished(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gone.tmpl.md")
	h := core.Harness{Name: "sweep", Adapter: "claude-code", PromptTemplateFile: path}
	if _, err := resolvePrompt(h, "/home/x", RunEnv{}); err == nil {
		t.Fatal("resolvePrompt succeeded on a vanished template file")
	}
}

// TestVerbatimPromptKeepsItsBraces: prompt and prompt_file are never expanded,
// whatever they contain (REQ-5; REQ-17's amendment to SPEC-0006 "Prompt
// Source"). The braces reach the adapter's PromptCommand as written.
func TestVerbatimPromptKeepsItsBraces(t *testing.T) {
	run := RunEnv{RunID: 7, Trigger: "schedule"}
	h := core.Harness{Name: "sweep", Adapter: "claude-code", Prompt: "print {{run.id}} literally"}

	got, err := resolvePrompt(h, "/home/x", run)
	if err != nil {
		t.Fatalf("resolvePrompt: %v", err)
	}
	if got.Prompt != "print {{run.id}} literally" {
		t.Errorf("verbatim prompt was expanded: %q", got.Prompt)
	}
	_, args := mustExecArgvReg(t, got, "/home/x", adapter.NewRegistryWithDefaults())
	if len(args) == 0 || args[len(args)-1] != "print {{run.id}} literally" {
		t.Errorf("argv = %v, want the verbatim prompt as the final element", args)
	}
}

// TestPromptFileArgvUnaffectedByEvent: a triggered harness using prompt_file
// gets a byte-identical argv with and without an event (SPEC-0014; REQ-17's
// scenario "An existing verbatim harness is unaffected by events"). Nothing
// from an event reaches a verbatim prompt harness's argv.
func TestPromptFileArgvUnaffectedByEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sweep.prompt.md")
	if err := os.WriteFile(path, []byte("sweep the fleet\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := core.Harness{Name: "sweep", Adapter: "claude-code", PromptFile: path}
	started := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	plain := RunEnv{RunID: 9, Trigger: "webhook", StartedAt: started}
	evented := RunEnv{RunID: 9, Trigger: "webhook", Source: "webhook/x", EventFile: "/tmp/event.json", StartedAt: started}

	nameA, argsA, err := argvFor(h, plain)
	if err != nil {
		t.Fatalf("plain execArgv: %v", err)
	}
	nameB, argsB, err := argvFor(h, evented)
	if err != nil {
		t.Fatalf("evented execArgv: %v", err)
	}
	if nameA != nameB || !reflect.DeepEqual(argsA, argsB) {
		t.Errorf("argv differs with an event:\nplain %q %v\nevent %q %v", nameA, argsA, nameB, argsB)
	}
}

func argvFor(h core.Harness, run RunEnv) (string, []string, error) {
	resolved, err := resolvePrompt(h, "/home/x", run)
	if err != nil {
		return "", nil, err
	}
	plan, err := execArgv(resolved, "/home/x", run)
	if err != nil {
		return "", nil, err
	}
	return plan.name, plan.args, nil
}

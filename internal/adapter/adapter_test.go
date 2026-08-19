package adapter

import (
	"errors"
	"testing"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

func TestRegistryGet(t *testing.T) {
	r := NewRegistryWithDefaults()

	tests := []struct {
		name    string
		wantErr bool
	}{
		{"claude-code", false},
		{"crush", false},
		{"codex", false},
		{"generic", false},
		{"nonexistent", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a, err := r.Get(tt.name)
			if tt.wantErr {
				if !errors.Is(err, ErrUnknownAdapter) {
					t.Fatalf("got err %v, want ErrUnknownAdapter", err)
				}
				if a != nil {
					t.Fatal("expected nil adapter for unknown name")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if a.Name() != tt.name {
				t.Fatalf("got name %q, want %q", a.Name(), tt.name)
			}
		})
	}
}

func TestRegistryNames(t *testing.T) {
	r := NewRegistryWithDefaults()
	names := r.Names()
	want := []string{"claude-code", "crush", "codex", "generic"}
	if len(names) != len(want) {
		t.Fatalf("got %d names, want %d", len(names), len(want))
	}
	for i, n := range names {
		if n != want[i] {
			t.Fatalf("names[%d] = %q, want %q", i, n, want[i])
		}
	}
}

func TestResolveExplicitAgent(t *testing.T) {
	r := NewRegistryWithDefaults()

	// Explicit agent = "claude-code" on a harness with an unrecognized cmd.
	h := core.Harness{
		Adapter: "claude-code",
	}
	a := r.Resolve(h)
	if a == nil {
		t.Fatal("expected non-nil adapter")
	}
	if a.Name() != "claude-code" {
		t.Fatalf("got %q, want claude-code", a.Name())
	}
}

func TestResolveUnsetOrUnknownIsGeneric(t *testing.T) {
	r := NewRegistryWithDefaults()

	// The `harness` enum key is required and validated at both front doors, so
	// an empty adapter should never reach Resolve. If one does, it must not be
	// guessed into an agent: Resolve is the last stop before something is
	// executed, and Generic runs what it is given without harvesting anything.
	h := core.Harness{Prompt: "do the thing"}
	a := r.Resolve(h)
	if a.Name() != "generic" {
		t.Fatalf("Resolve(empty) = %q, want generic", a.Name())
	}
	// An unknown value maps defensively to Generic (config validation
	// rejects it before this point; Resolve never returns nil).
	if got := r.Resolve(core.Harness{Adapter: "nope"}); got.Name() != "generic" {
		t.Fatalf("Resolve(nope) = %q, want generic", got.Name())
	}
}
func TestResolvePromptHarnessWithExplicitAgent(t *testing.T) {
	r := NewRegistryWithDefaults()

	h := core.Harness{
		Prompt:  "do the thing",
		Adapter: "claude-code",
	}
	a := r.Resolve(h)
	if a == nil {
		t.Fatal("expected non-nil adapter")
	}
	if a.Name() != "claude-code" {
		t.Fatalf("got %q, want claude-code", a.Name())
	}
}

func TestResolveUnknownFallsBackToGeneric(t *testing.T) {
	r := NewRegistryWithDefaults()

	// Config validation rejects unknown values up front; Resolve defends the
	// wire by mapping them to Generic instead of returning nil.
	a := r.Resolve(core.Harness{Adapter: "nope"})
	if a == nil || a.Name() != "generic" {
		t.Fatalf("Resolve(nope) = %v, want generic", a)
	}
}

func TestClaudeCodeTrajectoryDir(t *testing.T) {
	a := &ClaudeCode{}
	dir := a.TrajectoryDir("/some/workdir")
	if dir == "" {
		t.Fatal("expected non-empty trajectory dir for claude-code")
	}
	// Should end with .claude/projects
	if !endsWith(dir, ".claude/projects") {
		t.Fatalf("trajectory dir %q does not end with .claude/projects", dir)
	}
}

func TestCrushTrajectoryDir(t *testing.T) {
	a := &Crush{}
	dir := a.TrajectoryDir("/some/workdir")
	if dir == "" {
		t.Fatal("expected non-empty trajectory dir for crush")
	}
}

func TestCodexTrajectoryDir(t *testing.T) {
	a := &Codex{}
	dir := a.TrajectoryDir("/some/workdir")
	if dir == "" {
		t.Fatal("expected non-empty trajectory dir for codex")
	}
}

func TestGenericTrajectoryDir(t *testing.T) {
	a := &Generic{}
	dir := a.TrajectoryDir("/some/workdir")
	if dir != "" {
		t.Fatalf("expected empty trajectory dir for generic, got %q", dir)
	}
}

func TestGenericTailAdapterIsNil(t *testing.T) {
	a := &Generic{}
	if a.TailAdapter() != nil {
		t.Fatal("expected nil tail adapter for generic")
	}
}

func TestClaudeCodeTailAdapterNonNil(t *testing.T) {
	a := &ClaudeCode{}
	if a.TailAdapter() == nil {
		t.Fatal("expected non-nil tail adapter for claude-code")
	}
}

// endsWith is a tiny helper to avoid importing path/filepath just for a suffix
// check in a test assertion.
func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// --- PromptCommand tests (issue #74) ---

func TestCrushPromptCommand(t *testing.T) {
	a := &Crush{}
	cmd, args := a.PromptCommand("do the thing", core.AgentOpts{Quiet: true})
	if cmd != "crush" {
		t.Fatalf("cmd = %q, want crush", cmd)
	}
	want := []string{"run", "--quiet", "do the thing"}
	if !slicesEqual(args, want) {
		t.Fatalf("args = %q, want %q", args, want)
	}
}

func TestCrushPromptCommandAllOpts(t *testing.T) {
	a := &Crush{}
	cmd, args := a.PromptCommand("check deps", core.AgentOpts{
		Quiet:      true,
		Model:      "claude-opus-5",
		AutoAccept: true,
		MaxTurns:   10,
	})
	if cmd != "crush" {
		t.Fatalf("cmd = %q, want crush", cmd)
	}
	want := []string{"--yolo", "run", "--quiet", "--model", "claude-opus-5", "check deps"}
	if !slicesEqual(args, want) {
		t.Fatalf("args = %q, want %q", args, want)
	}
}

func TestClaudeCodePromptCommand(t *testing.T) {
	a := &ClaudeCode{}
	cmd, args := a.PromptCommand("review PR", core.AgentOpts{Quiet: true})
	if cmd != "claude" {
		t.Fatalf("cmd = %q, want claude", cmd)
	}
	want := []string{"-p", "--output-format", "stream-json", "review PR"}
	// Quiet maps to -p (non-interactive), not --quiet; AutoAccept is false
	// so --dangerously-skip-permissions is absent.
	if !slicesEqual(args, want) {
		t.Fatalf("args = %q, want %q", args, want)
	}
}

func TestClaudeCodePromptCommandWithModel(t *testing.T) {
	a := &ClaudeCode{}
	cmd, args := a.PromptCommand("fix bug", core.AgentOpts{
		Quiet: true,
		Model: "claude-opus-5",
	})
	if cmd != "claude" {
		t.Fatalf("cmd = %q, want claude", cmd)
	}
	// --model comes before the prompt
	found := false
	for i, arg := range args {
		if arg == "--model" && i+1 < len(args) && args[i+1] == "claude-opus-5" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("args %q missing --model claude-opus-5", args)
	}
	// Prompt must be last
	if args[len(args)-1] != "fix bug" {
		t.Fatalf("last arg = %q, want prompt as final element", args[len(args)-1])
	}
}

func TestCodexPromptCommand(t *testing.T) {
	a := &Codex{}
	cmd, args := a.PromptCommand("write tests", core.AgentOpts{Quiet: true})
	if cmd != "codex" {
		t.Fatalf("cmd = %q, want codex", cmd)
	}
	// Codex uses `codex exec` for non-interactive runs
	if len(args) < 2 || args[0] != "exec" {
		t.Fatalf("args = %q, want exec subcommand", args)
	}
	if args[len(args)-1] != "write tests" {
		t.Fatalf("last arg = %q, want prompt as final element", args[len(args)-1])
	}
}

func TestGenericPromptCommandFallsBackToCrush(t *testing.T) {
	a := &Generic{}
	cmd, args := a.PromptCommand("do something", core.AgentOpts{Quiet: true})
	// Generic has no native prompt mode; falls back to crush so prompt
	// harnesses without an explicit agent key still work.
	if cmd != "crush" {
		t.Fatalf("cmd = %q, want crush fallback for generic", cmd)
	}
	if len(args) == 0 {
		t.Fatal("expected non-empty args")
	}
	if args[len(args)-1] != "do something" {
		t.Fatalf("last arg = %q, want prompt as final element", args[len(args)-1])
	}
}

// slicesEqual is a test helper mirroring slices.Equal without importing it
// in the test file's import block (it may already be imported).
func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

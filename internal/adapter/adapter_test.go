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
		Cmd:   "my-wrapper",
		Agent: "claude-code",
	}
	a := r.Resolve(h)
	if a == nil {
		t.Fatal("expected non-nil adapter")
	}
	if a.Name() != "claude-code" {
		t.Fatalf("got %q, want claude-code", a.Name())
	}
}

func TestResolveExplicitAgentOverridesInference(t *testing.T) {
	r := NewRegistryWithDefaults()

	// SPEC-0006 REQ "Adapter Selection" scenario "Explicit agent overrides
	// inference": cmd matches an inference rule, but an explicit agent key
	// should win.
	h := core.Harness{
		Cmd:   "claude",
		Agent: "crush",
	}
	a := r.Resolve(h)
	if a == nil {
		t.Fatal("expected non-nil adapter")
	}
	if a.Name() != "crush" {
		t.Fatalf("got %q, want crush (explicit should win over inference)", a.Name())
	}
}

func TestResolveInferredFromCmd(t *testing.T) {
	r := NewRegistryWithDefaults()

	tests := []struct {
		cmd      string
		wantName string
	}{
		{"claude", "claude-code"},
		{"crush", "crush"},
		{"codex", "codex"},
		{"/usr/local/bin/claude", "claude-code"}, // basename extraction
		{"./my-wrapper", "generic"},              // unrecognized
		{"", "generic"},                          // empty cmd
	}

	for _, tt := range tests {
		t.Run(tt.cmd, func(t *testing.T) {
			h := core.Harness{Cmd: tt.cmd}
			a := r.Resolve(h)
			if a == nil {
				t.Fatal("expected non-nil adapter")
			}
			if a.Name() != tt.wantName {
				t.Fatalf("Resolve(%q) = %q, want %q", tt.cmd, a.Name(), tt.wantName)
			}
		})
	}
}

func TestResolvePromptHarnessFallsToGeneric(t *testing.T) {
	r := NewRegistryWithDefaults()

	// A prompt harness has no Cmd — inference finds nothing, falls to generic.
	// The daemon's interim synthesis uses crush, but trajectory discovery
	// should not assume a prompt harness is crush unless `agent` is set.
	h := core.Harness{Prompt: "do the thing"}
	a := r.Resolve(h)
	if a == nil {
		t.Fatal("expected non-nil adapter")
	}
	if a.Name() != "generic" {
		t.Fatalf("got %q, want generic for prompt harness without agent key", a.Name())
	}
}

func TestResolvePromptHarnessWithExplicitAgent(t *testing.T) {
	r := NewRegistryWithDefaults()

	h := core.Harness{
		Prompt: "do the thing",
		Agent:  "claude-code",
	}
	a := r.Resolve(h)
	if a == nil {
		t.Fatal("expected non-nil adapter")
	}
	if a.Name() != "claude-code" {
		t.Fatalf("got %q, want claude-code", a.Name())
	}
}

func TestResolveUnknownAgentReturnsNil(t *testing.T) {
	r := NewRegistryWithDefaults()

	h := core.Harness{Agent: "nonexistent"}
	a := r.Resolve(h)
	if a != nil {
		t.Fatalf("expected nil for unknown agent, got %v", a)
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

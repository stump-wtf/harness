// Package adapter maps a harness's agent CLI to its tool-specific knowledge —
// where transcripts live, and (in future stories) where skills come from and
// where they must be projected.
//
// Governing: ADR-0011 (agent adapters), SPEC-0006 REQ "Adapter Selection",
// SPEC-0006 REQ "Trajectory Discovery".
//
// The daemon holds a Registry it dispatches on without understanding any
// entry, mirroring the existing backend precedent (ADR-0003). Selection is
// the harness's `harness` enum key (core.Harness.Adapter), defaulting to
// Crush. An unrecognized tool resolves to Generic, which reports no
// trajectory — its record is the scrollback ring (ADR-0007).
package adapter

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/stump-wtf/agent-trace/tail"
	"github.com/stump-wtf/harness/internal/core"
)

// ErrUnknownAdapter is returned when a harness names an adapter that does not
// exist in the registry. SPEC-0006 REQ "Adapter Selection" requires this to be
// a config-validation error identifying the harness and the unknown adapter.
var ErrUnknownAdapter = errors.New("unknown adapter")

// Adapter answers tool-specific questions about a harness. The full ADR-0011
// interface covers skills (from/to) and trajectory; issue #76 implements only
// the trajectory surface — skill methods arrive in later stories.
type Adapter interface {
	// Name is the adapter's registry key: "claude-code", "crush", "codex",
	// "generic", "command".
	Name() string

	// TrajectoryDir returns the directory where this tool stores session
	// transcripts for the given harness workdir, or "" when the tool has no
	// native trajectory format. When empty, the daemon falls back to the
	// SPEC-0002 scrollback record (ADR-0007).
	//
	// Most adapters return a fixed path (e.g. ~/.claude/projects/); the workdir
	// parameter is available for tools that scope sessions per-project.
	TrajectoryDir(workdir string) string

	// TailAdapter returns the agent-trace tail.Adapter used to enumerate and
	// parse sessions, or nil when the adapter has no native trajectory (Generic).
	TailAdapter() tail.Adapter

	// Executable returns the CLI an adapter's long-running (non-prompt)
	// harness runs; Args from the config are appended after it.
	Executable() string

	// PromptCommand returns the executable and argv for running a prompt
	// one-shot with this adapter's CLI. Each adapter maps the generic
	// AgentOpts onto its own flags (e.g. crush uses --yolo, claude uses
	// --dangerously-skip-permissions). The prompt is always the final argv
	// element. An adapter with no prompt mode (Generic) returns an empty
	// cmd, which spawn treats as a refusal, never as something to exec.
	// Governing: issue #74 (adapter-aware prompt synthesis), SPEC-0017 REQ
	// "Generic Kind Rejects Prompts".
	PromptCommand(prompt string, opts core.AgentOpts) (cmd string, args []string)
}

// Registry maps adapter names to Adapter implementations. The daemon holds one
// and dispatches on it without understanding any entry — the same pattern as
// the backend registry (ADR-0003).
type Registry struct {
	entries map[string]Adapter
}

// NewRegistry returns a Registry populated with the built-in adapters:
// claude-code, crush, codex, generic, and command.
func NewRegistry() *Registry {
	r := &Registry{
		entries: make(map[string]Adapter),
	}
	r.register(&ClaudeCode{})
	r.register(&Crush{})
	r.register(&Codex{})
	r.register(&Generic{})
	r.register(&Command{})
	return r
}

func (r *Registry) register(a Adapter) {
	r.entries[a.Name()] = a
}

// Get returns the named adapter, or ErrUnknownAdapter.
func (r *Registry) Get(name string) (Adapter, error) {
	a, ok := r.entries[name]
	if !ok {
		return nil, ErrUnknownAdapter
	}
	return a, nil
}

// Names returns every registered adapter name in insertion order.
func (r *Registry) Names() []string {
	return []string{"claude-code", "crush", "codex", "generic", "command"}
}

// Resolve selects the adapter for a harness from its `harness` enum key. Per
// SPEC-0006 REQ "Adapter Selection", the key is required and validated at
// both front doors (config parse and the project-up wire), so neither an
// empty nor an unknown value should reach here.
//
// Both are nonetheless mapped to Generic rather than to an agent. Resolve is
// the last stop before something gets executed, and the safe answer to "which
// agent did they mean?" is not to guess one — Generic runs what it is given
// and harvests no trajectory.
func (r *Registry) Resolve(h core.Harness) Adapter {
	if a, ok := r.entries[h.Adapter]; ok {
		return a
	}
	return r.entries["generic"]
}

// --- Built-in adapters ---

// ClaudeCode is the adapter for Claude Code (claude CLI).
type ClaudeCode struct{}

func (a *ClaudeCode) Name() string { return "claude-code" }

func (a *ClaudeCode) Executable() string { return "claude" }

func (a *ClaudeCode) TrajectoryDir(_ string) string {
	// Claude Code stores JSONL transcripts under ~/.claude/projects/, organized
	// by project directory hash. The agent-trace ClaudeCodeAdapter discovers
	// them.
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

func (a *ClaudeCode) TailAdapter() tail.Adapter { return &tail.ClaudeCodeAdapter{} }

func (a *ClaudeCode) PromptCommand(prompt string, opts core.AgentOpts) (string, []string) {
	args := []string{"-p"}
	if opts.AutoAccept {
		args = append(args, "--dangerously-skip-permissions")
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(opts.MaxTurns))
	}
	// --verbose is REQUIRED alongside --output-format stream-json under -p:
	// claude rejects the pair outright with "When using --print,
	// --output-format=stream-json requires --verbose" and exits 1 before it
	// reads the prompt. Without it every scheduled claude-code harness dies at
	// launch, which for a cron one-shot reads as a silent no-op — the schedule
	// fires, the process is gone, and nothing ran.
	args = append(args, "--verbose", "--output-format", "stream-json")
	return "claude", append(args, prompt)
}

// Crush is the adapter for Crush.
type Crush struct{}

func (a *Crush) Name() string { return "crush" }

func (a *Crush) Executable() string { return "crush" }

func (a *Crush) TrajectoryDir(_ string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "crush")
}

func (a *Crush) TailAdapter() tail.Adapter { return &tail.CrushAdapter{} }

func (a *Crush) PromptCommand(prompt string, opts core.AgentOpts) (string, []string) {
	// --yolo is a GLOBAL crush flag — it must precede the `run` subcommand;
	// after it, crush exits "unknown flag" and the harness crash-loops.
	// crush has no --max-turns at any position, so the budget stays inert
	// (issue #59 remains open against crush growing the flag).
	args := []string{}
	if opts.AutoAccept {
		args = append(args, "--yolo")
	}
	args = append(args, "run")
	if opts.Quiet {
		args = append(args, "--quiet")
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	return "crush", append(args, prompt)
}

// Codex is the adapter for Codex.
type Codex struct{}

func (a *Codex) Name() string { return "codex" }

func (a *Codex) Executable() string { return "codex" }

func (a *Codex) TrajectoryDir(_ string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".codex", "sessions")
}

func (a *Codex) TailAdapter() tail.Adapter { return &tail.CodexAdapter{} }

func (a *Codex) PromptCommand(prompt string, opts core.AgentOpts) (string, []string) {
	args := []string{"exec"}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.AutoAccept {
		args = append(args, "--full-auto")
	}
	return "codex", append(args, prompt)
}

// Generic is the adapter for unrecognized tools. It reports no trajectory —
// the daemon falls back to the SPEC-0002 scrollback ring (ADR-0007). Per
// SPEC-0006 REQ "Adapter Selection", this is a real adapter, not an error.
type Generic struct{}

func (a *Generic) Name() string { return "generic" }

// Executable is sh: a Generic harness is an arbitrary command, expressed as
// args (e.g. harness = "generic", args = ["-c", "while true; do …; done"]).
func (a *Generic) Executable() string { return "sh" }

func (a *Generic) TrajectoryDir(_ string) string { return "" }

func (a *Generic) TailAdapter() tail.Adapter { return nil }

// PromptCommand returns no argv: Generic runs sh and has no prompt synthesis.
// It used to delegate to Crush, so an operator whose CLI was not in the list
// wrote `generic` + `prompt` and got `crush run <prompt>` (or a crash loop
// naming a binary they never configured). Config validation now rejects the
// combination at every front door; the empty executable is what lets spawn
// refuse one that got past them, rather than guessing an agent.
// Governing: ADR-0023, SPEC-0017 REQ "Generic Kind Rejects Prompts".
func (a *Generic) PromptCommand(string, core.AgentOpts) (string, []string) {
	return "", nil
}

// ArgvOwner is implemented by an adapter whose harness declares its own whole
// argv instead of args appended to an adapter executable. Spawn asks for it
// before anything else, so neither Executable nor PromptCommand is consulted
// for such a harness. Governing: ADR-0023, SPEC-0017 REQ-2 "Command Harness
// Kind".
type ArgvOwner interface {
	// Argv returns the executable and arguments to exec for h, whose
	// relative paths resolve against workdir. It never returns a shell
	// invocation or a joined command string.
	Argv(h core.Harness, workdir string) (cmd string, args []string)
}

// Command is the adapter for `harness = "command"`: the harness's Argv is the
// process, exec'd directly. It is the shell-free replacement for `generic` +
// args = ["-c", "…"]: an argument containing spaces, `$(…)` or `;` reaches the
// child as one byte-identical argument, because nothing ever parses it. Like
// Generic it reports no native trajectory (a `transcripts` binding is
// SPEC-0017 REQ-4, not yet here).
// Governing: ADR-0023, SPEC-0017 REQ-2 "Command Harness Kind".
type Command struct{}

func (a *Command) Name() string { return "command" }

// Executable is empty: a command harness has no executable of its own, and
// Argv (ArgvOwner) is what spawn runs.
func (a *Command) Executable() string { return "" }

func (a *Command) TrajectoryDir(_ string) string { return "" }

func (a *Command) TailAdapter() tail.Adapter { return nil }

// PromptCommand returns no argv. A command harness's operator owns its argv,
// so there is nothing to synthesize; delivering a prompt to it (SPEC-0017
// REQ-12) will be a render step over Argv, not a second argv. Spawn refuses a
// prompt it cannot deliver instead of dropping it.
func (a *Command) PromptCommand(string, core.AgentOpts) (string, []string) {
	return "", nil
}

// Argv returns argv[0] and a copy of argv[1:]. A relative argv[0] containing
// a path separator (`./bin/report`, `scripts/x`) resolves against workdir, so
// it names the same file whichever directory the daemon was started from; a
// bare name (`echo`) is left for spawn's PATH lookup, and an absolute path is
// used as is. The arguments are copied, not aliased, so nothing downstream
// can reach back into the registered definition.
func (a *Command) Argv(h core.Harness, workdir string) (string, []string) {
	if len(h.Argv) == 0 {
		return "", nil
	}
	name := h.Argv[0]
	if !filepath.IsAbs(name) && strings.ContainsRune(name, filepath.Separator) && workdir != "" {
		name = filepath.Join(workdir, name)
	}
	return name, append([]string(nil), h.Argv[1:]...)
}

// NewRegistryWithDefaults returns a Registry with the built-in adapters.
// This is what the daemon constructs at startup; with cmd→adapter inference
// retired by the `harness` enum key (default crush), it is equivalent to
// NewRegistry and kept for call-site stability.
func NewRegistryWithDefaults() *Registry {
	return NewRegistry()
}

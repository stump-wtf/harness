// Package adapter maps a harness's agent CLI to its tool-specific knowledge —
// where transcripts live, and (in future stories) where skills come from and
// where they must be projected.
//
// Governing: ADR-0011 (agent adapters), SPEC-0006 REQ "Adapter Selection",
// SPEC-0006 REQ "Trajectory Discovery".
//
// The daemon holds a Registry it dispatches on without understanding any
// entry, mirroring the existing backend precedent (ADR-0003). Selection is by
// an optional `agent` key on the harness, inferred from `cmd` when unset.
// An unrecognized tool resolves to Generic, which reports no trajectory —
// its record is the scrollback ring (ADR-0007).
package adapter

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"github.com/stump-wtf/agent-trace/tail"
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
	// "generic".
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

	// PromptCommand returns the executable and argv for running a prompt
	// one-shot with this adapter's CLI. Each adapter maps the generic
	// AgentOpts onto its own flags (e.g. crush uses --yolo, claude uses
	// --dangerously-skip-permissions). The prompt is always the final argv
	// element. Governing: issue #74 (adapter-aware prompt synthesis).
	PromptCommand(prompt string, opts core.AgentOpts) (cmd string, args []string)
}

// Registry maps adapter names to Adapter implementations. The daemon holds one
// and dispatches on it without understanding any entry — the same pattern as
// the backend registry (ADR-0003).
type Registry struct {
	entries map[string]Adapter
	// inference maps a cmd basename to an adapter name, for zero-config
	// selection when the `agent` key is absent (SPEC-0006 REQ "Adapter
	// Selection": "inferred from cmd").
	inference map[string]string
}

// NewRegistry returns a Registry populated with the built-in adapters:
// claude-code, crush, codex, and generic.
func NewRegistry() *Registry {
	r := &Registry{
		entries:   make(map[string]Adapter),
		inference: make(map[string]string),
	}
	r.register(&ClaudeCode{})
	r.register(&Crush{})
	r.register(&Codex{})
	r.register(&Generic{})
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
	return []string{"claude-code", "crush", "codex", "generic"}
}

// RegisterInference maps a cmd basename to an adapter name, so that
// Resolve("claude") finds the claude-code adapter without an explicit
// `agent` key. Call this for each built-in inference rule.
func (r *Registry) RegisterInference(cmdBasename, adapterName string) {
	r.inference[cmdBasename] = adapterName
}

// Resolve selects the adapter for a harness: the `agent` key when set,
// otherwise inferred from `cmd`, otherwise Generic. Per SPEC-0006 REQ
// "Adapter Selection", an explicit `agent` key naming an unknown adapter is
// an error (validation should catch this before Resolve is called, but
// Resolve also returns it defensively).
func (r *Registry) Resolve(h core.Harness) Adapter {
	if h.Agent != "" {
		if a, ok := r.entries[h.Agent]; ok {
			return a
		}
		return nil
	}
	// Infer from cmd basename. A prompt harness has no Cmd (the supervisor
	// synthesizes it at spawn time), so inference falls through to generic —
	// the prompt harness's trajectory is the scrollback ring unless an
	// explicit `agent` key is set.
	fields := strings.Fields(h.Cmd)
	if len(fields) == 0 {
		return r.entries["generic"]
	}
	cmd := filepath.Base(fields[0])
	if name, ok := r.inference[cmd]; ok {
		return r.entries[name]
	}
	return r.entries["generic"]
}

// --- Built-in adapters ---

// ClaudeCode is the adapter for Claude Code (claude CLI).
type ClaudeCode struct{}

func (a *ClaudeCode) Name() string { return "claude-code" }

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
	args = append(args, "--output-format", "stream-json")
	return "claude", append(args, prompt)
}

// Crush is the adapter for Crush.
type Crush struct{}

func (a *Crush) Name() string { return "crush" }

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

func (a *Generic) TrajectoryDir(_ string) string { return "" }

func (a *Generic) TailAdapter() tail.Adapter { return nil }

func (a *Generic) PromptCommand(prompt string, opts core.AgentOpts) (string, []string) {
	return (&Crush{}).PromptCommand(prompt, opts)
}

// DefaultInference returns the default cmd→adapter inference table used by
// NewRegistryWithDefaults. This is the zero-config path described in SPEC-0006
// REQ "Adapter Selection" scenario "Adapter inferred from cmd": a harness with
// cmd = "claude" and no agent key resolves to the claude-code adapter.
func DefaultInference() map[string]string {
	return map[string]string{
		"claude": "claude-code",
		"crush":  "crush",
		"codex":  "codex",
	}
}

// NewRegistryWithDefaults returns a Registry with both the built-in adapters
// and the default cmd→adapter inference rules wired. This is what the daemon
// constructs at startup.
func NewRegistryWithDefaults() *Registry {
	r := NewRegistry()
	for cmd, name := range DefaultInference() {
		r.RegisterInference(cmd, name)
	}
	return r
}

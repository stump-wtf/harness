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

// NoSkills is the default skill surface: no default roots and no target, so
// a harness on such an adapter starts normally with no projection
// (SPEC-0006 REQ "Spawn-Time Projection"). Adapters whose tool discovers
// skills elsewhere embed it until ADR-0039's [adapter.<name>.skills] tables
// give them a real answer.
type NoSkills struct{}

func (NoSkills) SkillRoots(string) []string { return nil }
func (NoSkills) SkillTarget() string        { return "" }

// Adapter answers tool-specific questions about a harness. The full ADR-0011
// interface covers skills (from/to) and trajectory; issue #76 implements only
// the trajectory surface — skill methods arrive in later stories.
type Adapter interface {
	// Name is the adapter's registry key: "claude-code", "crush", "codex",
	// "pi", "omp", "opencode", "generic", "command".
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

	// SkillRoots returns the adapter's default skill roots — the lowest
	// precedence tier of the SPEC-0006 merge ("Ordered Merge and
	// Shadowing") — with workdir-relative entries resolved. Empty when the
	// adapter declares no defaults. ADR-0039 keeps these in the Go registry
	// until [adapter.<name>.skills] tables exist (issue #75's recorded
	// decision).
	SkillRoots(workdir string) []string

	// SkillTarget returns the directory the merged skill set is materialized
	// into before exec, or "" when the adapter projects nothing: a harness
	// on such an adapter starts normally with no projection (SPEC-0006 REQ
	// "Spawn-Time Projection").
	SkillTarget() string
}

// Registry maps adapter names to Adapter implementations. The daemon holds one
// and dispatches on it without understanding any entry — the same pattern as
// the backend registry (ADR-0003).
type Registry struct {
	entries map[string]Adapter
}

// NewRegistry returns a Registry populated with the built-in adapters:
// claude-code, crush, codex, pi, omp, opencode, generic, and command.
func NewRegistry() *Registry {
	r := &Registry{
		entries: make(map[string]Adapter),
	}
	r.register(&ClaudeCode{})
	r.register(&Crush{})
	r.register(&Codex{})
	r.register(Pi)
	r.register(OMP)
	r.register(&OpenCode{})
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
	return []string{"claude-code", "crush", "codex", core.AdapterPi, core.AdapterOMP, core.AdapterOpenCode, "generic", "command"}
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

// TrajectoryAdapter selects the adapter whose trajectory surface (TailAdapter,
// TrajectoryDir) applies to h: the one named by h.TrajectoryKind(). For every
// kind but a bound command harness that is Resolve's answer; a command harness
// with `transcripts` gets the bound adapter's, so its hand-built agent argv is
// discovered like that adapter's own harnesses. Spawn never uses this — the
// process is still the command harness's own argv.
// Governing: ADR-0023, SPEC-0017 REQ-4 "Transcript Binding".
func (r *Registry) TrajectoryAdapter(h core.Harness) Adapter {
	if a, ok := r.entries[h.TrajectoryKind()]; ok {
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

// SkillRoots and SkillTarget are ADR-0039's claude-code built-in, held in
// the Go registry until [adapter.<name>.skills] tables exist to carry them
// (issue #75's recorded decision): the per-repo default root and the
// repo-root default, with the per-workdir one resolved. ~/.claude/skills is
// both a root and the target in the ADR; SPEC-0006's target rule excludes it
// from the merge, so SkillRoots drops it — whatever lives there is the
// tool's own concern, and projection lands beside it.
func (a *ClaudeCode) SkillRoots(workdir string) []string {
	if workdir == "" {
		return nil
	}
	return []string{filepath.Join(workdir, ".claude", "skills")}
}

func (a *ClaudeCode) SkillTarget() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "skills")
}

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
	// SPEC-0018 REQ-11 persona keys, after the scalar flags and before
	// --verbose, so every variadic flag (--mcp-config, --allowedTools) is
	// followed by a flag. Validation keeps these claude-code-one-shot-only,
	// so the other adapters never see them set.
	if opts.SystemPromptFile != "" {
		// --append-system-prompt-file APPENDS to Claude Code's own system
		// prompt; it never replaces it.
		args = append(args, "--append-system-prompt-file", opts.SystemPromptFile)
	}
	if opts.MCPConfig != "" {
		// --strict-mcp-config means ONLY this file's servers run: the one-shot
		// does not inherit every MCP server the user has configured.
		args = append(args, "--mcp-config", opts.MCPConfig, "--strict-mcp-config")
	}
	if len(opts.AllowedTools) > 0 {
		args = append(args, "--allowedTools")
		// Each tool its own argv element: "Bash(git log:*)" stays one token,
		// so a shell never sees a comma list.
		args = append(args, opts.AllowedTools...)
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
type Crush struct {
	NoSkills
}

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
type Codex struct {
	NoSkills
}

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

// PiAgentDirEnv relocates the agent directory, sessions included, for both Pi
// and OMP: OMP kept Pi's variable name rather than minting its own (its
// pi-utils dirs.ts reads PI_CODING_AGENT_DIR for the default profile). Run
// correlation resolves it from the harness's own environment
// (runtrace.Sources), so an env_file that moves the agent directory is
// followed.
const PiAgentDirEnv = "PI_CODING_AGENT_DIR"

// PiFamily is the adapter for Pi and for its fork OMP (oh-my-pi): one
// implementation registered twice, because the two CLIs share their
// print-mode flags, their agent-dir variable and their session directory
// layout, and differ only in the executable, the default agent directory and
// the session-file details the reader needs (OMP opens every session with a
// 256-byte {"type":"title"} slot line; agent-trace >= v0.6.0 reads both).
//
// Flags and directories were verified against the source of Pi v0.87.1
// (github.com/badlogic/pi-mono packages/coding-agent at 8676a0dc: cli/args.ts,
// config.ts) and OMP v18.3.0 (github.com/can1357/oh-my-pi at 62bc57be:
// packages/coding-agent/src/cli/args.ts, packages/utils/src/dirs.ts) on
// 2026-09-24. Neither binary was run. The argv golden in adapter_test.go pins
// the flags; a CLI that drifts from them runs as `harness = "command"` with
// its own argv and `transcripts` until this is updated.
//
// Governing: ADR-0023, SPEC-0017 REQ-13 "Pi And OMP Adapters", design.md
// "pi and omp: one implementation, two registry entries".
type PiFamily struct {
	NoSkills
	name string
	exe  string
	// agentDir is the default agent directory relative to HOME; sessions
	// live in its "sessions" subdirectory.
	agentDir string
	// observed reports whether agent-trace's Pi session reader parses this
	// CLI's session files. Both do at agent-trace v0.6.0: the reader accepts
	// OMP's title slot, and tail.PiAdapter with OMP set labels the sessions
	// it reads tail.HarnessOMP, so an "omp" scope claims exactly its own
	// (runtrace.CouldWrite, ClaimantAt).
	observed bool
}

// Pi and OMP are the two registered members of the family.
var (
	Pi  = &PiFamily{name: core.AdapterPi, exe: "pi", agentDir: filepath.Join(".pi", "agent"), observed: true}
	OMP = &PiFamily{name: core.AdapterOMP, exe: "omp", agentDir: filepath.Join(".omp", "agent"), observed: true}
)

func (a *PiFamily) Name() string { return a.name }

func (a *PiFamily) Executable() string { return a.exe }

// SessionRoot is where this CLI writes session files, as a process whose
// environment is env and whose home directory is home sees it:
// $PI_CODING_AGENT_DIR/sessions when the variable is set, else
// <home>/<agentDir>/sessions. A leading "~/" in the variable is expanded
// against home, as both CLIs expand it.
//
// Not modelled (OMP only): a named profile (OMP_PROFILE / PI_PROFILE, which
// moves the agent directory under ~/.omp/profiles/<name>), PI_CONFIG_DIR, the
// XDG_DATA_HOME migration on Linux, and PI_CODING_AGENT_SESSION_DIR or
// --session-dir.
func (a *PiFamily) SessionRoot(env map[string]string, home string) string {
	if d := env[PiAgentDirEnv]; d != "" {
		if d == "~" {
			d = home
		} else if rest, ok := strings.CutPrefix(d, "~/"); ok {
			d = filepath.Join(home, rest)
		}
		return filepath.Join(d, "sessions")
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, a.agentDir, "sessions")
}

// Observed reports whether agent-trace reads this CLI's sessions, and so
// whether its harnesses are observed and appear in the SPEC-0013 series.
func (a *PiFamily) Observed() bool { return a.observed }

// TrajectoryDir is the session root under the daemon's own environment; run
// correlation resolves it from the harness's environment instead
// (SessionRoot). Empty for a member agent-trace cannot read.
func (a *PiFamily) TrajectoryDir(_ string) string {
	if !a.observed {
		return ""
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return a.SessionRoot(map[string]string{PiAgentDirEnv: os.Getenv(PiAgentDirEnv)}, home)
}

// TailAdapter is agent-trace's Pi reader at the session root. The OMP flag
// makes it label the sessions it reads tail.HarnessOMP, so an omp scope
// claims exactly its own.
func (a *PiFamily) TailAdapter() tail.Adapter {
	dir := a.TrajectoryDir("")
	if dir == "" {
		return nil
	}
	return &tail.PiAdapter{Dir: dir, OMP: a.name == core.AdapterOMP}
}

// PromptCommand is `<exe> --print [--model M] <prompt>`. --print is the
// non-interactive mode and already prints only the final response, so quiet
// adds nothing. --model takes the CLI's "provider/id" pattern verbatim.
// auto_accept emits nothing because neither CLI prompts for tool permission,
// and max_turns emits nothing because neither has a turn budget (as for
// Crush). The prompt is the final element.
func (a *PiFamily) PromptCommand(prompt string, opts core.AgentOpts) (string, []string) {
	args := []string{"--print"}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	return a.exe, append(args, prompt)
}

// OpenCode is the adapter for OpenCode (SST's opencode, https://opencode.ai).
//
// The argv is NOT verified against a running binary: no opencode is installed
// on the lane that wrote this. It follows the adapter template and the issue:
// the resident process is a bare `opencode` (its TUI) with `args` appended,
// and the one-shot is `opencode run`, the CLI's non-interactive mode, with
// `model` passed as `--model` in the CLI's "provider/model" form.
// auto_accept, max_turns and quiet are accepted and emit nothing: OpenCode
// declares tool permissions in its own config rather than through a
// skip-permissions flag, and has neither a turn budget nor a quiet switch to
// map (issue #919 names these as what a human must re-check with the real
// binary). A CLI that drifts from this runs as `harness = "command"` with its
// own argv and `transcripts = "opencode"` until this is updated, exactly as
// for the Pi family.
//
// The store needs no relocation rule: the CLI records every session in one
// SQLite database, `~/.opencode/opencode.db` — agent-trace's
// OpenCodeAdapter default — and reads no variable that moves it, so discovery
// follows only the HOME a harness's env_file gives it.
//
// Governing: ADR-0011, ADR-0033 (trace first: agent-trace's OpenCodeAdapter
// is on main), issue #919.
type OpenCode struct {
	NoSkills
}

func (a *OpenCode) Name() string { return core.AdapterOpenCode }

func (a *OpenCode) Executable() string { return "opencode" }

// SessionPath is the CLI's session database as a process whose home directory
// is home sees it: <home>/.opencode/opencode.db. Run correlation resolves it
// from the harness's own HOME, so an env_file that moves the home directory
// is followed.
func (a *OpenCode) SessionPath(home string) string {
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".opencode", "opencode.db")
}

// TrajectoryDir is the directory holding the database under the daemon's own
// home; run correlation resolves the database itself from the harness's
// environment (SessionPath).
func (a *OpenCode) TrajectoryDir(_ string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".opencode")
}

// TailAdapter is agent-trace's OpenCode reader at the database. The database
// is one SQLite store for every session the CLI has recorded, so sessions are
// keyed "<database>/<session-id>" like Crush's; attribution narrows them by
// the session's recorded directory.
func (a *OpenCode) TailAdapter() tail.Adapter {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}
	return &tail.OpenCodeAdapter{DBPath: a.SessionPath(home)}
}

// PromptCommand is `opencode run [--model M] <prompt>`. See the type doc for
// what was verified and what a human must check against the real binary.
func (a *OpenCode) PromptCommand(prompt string, opts core.AgentOpts) (string, []string) {
	args := []string{"run"}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	return "opencode", append(args, prompt)
}

// Generic is the adapter for unrecognized tools. It reports no trajectory —
// the daemon falls back to the SPEC-0002 scrollback ring (ADR-0007). Per
// SPEC-0006 REQ "Adapter Selection", this is a real adapter, not an error.
type Generic struct {
	NoSkills
}

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
// Generic it reports no native trajectory of its own: a harness that binds one
// with `transcripts` gets that adapter's through Registry.TrajectoryAdapter.
// Governing: ADR-0023, SPEC-0017 REQ-2 "Command Harness Kind", REQ-4
// "Transcript Binding".
type Command struct {
	NoSkills
}

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

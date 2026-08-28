package core

// Governing: ADR-0006 (configuration & profiles — the harness/profile schema),
// ADR-0003 (backend: native vs tmux), ADR-0002 (the daemon's registry holds
// these parsed records). These are the core domain types every other package
// (config, supervisor, protocol, tui) imports.

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"time"
)

// Backend selects how a harness's process is hosted: natively under the
// daemon's own PTY (default), or via a tmux session (ADR-0003 escape hatch).
type Backend string

const (
	// BackendNative runs the process under the daemon's own PTY (ADR-0003).
	BackendNative Backend = "native"
	// BackendTmux runs the process inside a tmux session (compat escape hatch).
	BackendTmux Backend = "tmux"
)

// Valid reports whether b is a known backend.
func (b Backend) Valid() bool {
	return b == BackendNative || b == BackendTmux
}

// RestartPolicy controls whether a harness is automatically restarted after it
// exits, mirroring Docker Compose's `restart` directive. The empty string means
// "default" (always restart while enabled), preserving backward compatibility.
type RestartPolicy string

const (
	// RestartAlways restarts the harness unconditionally while enabled.
	RestartAlways RestartPolicy = "always"
	// RestartNo never restarts the harness automatically.
	RestartNo RestartPolicy = "no"
	// RestartUnlessStopped restarts the harness unless it was explicitly stopped.
	// In this daemon both RestartAlways and RestartUnlessStopped behave like
	// Docker's unless-stopped: an explicit Stop persists enabled=false
	// (ADR-0007), so a manually stopped harness stays down across daemon
	// restarts under either value. Docker's stronger `always` (comes back on
	// daemon restart even after a manual stop) is intentionally not modeled.
	RestartUnlessStopped RestartPolicy = "unless-stopped"
	// RestartOnFailure restarts the harness only when it exits with a non-zero
	// code.
	RestartOnFailure RestartPolicy = "on-failure"
)

// Valid reports whether p is a known restart policy. The empty string (the
// zero value / omitted key) is valid and means the default, RestartAlways.
func (p RestartPolicy) Valid() bool {
	switch p {
	case "", RestartAlways, RestartNo, RestartUnlessStopped, RestartOnFailure:
		return true
	}
	return false
}

// ShouldRestart reports whether an exit with the given code should be followed
// by an automatic respawn under policy p. A spawn failure or signal death
// (code == -1) counts as a failure for RestartOnFailure. The zero value and any
// unknown policy fall through to the always-restart default.
func (p RestartPolicy) ShouldRestart(code int) bool {
	switch p {
	case RestartNo:
		return false
	case RestartOnFailure:
		return code != 0
	default: // "", RestartAlways, RestartUnlessStopped
		return true
	}
}

// Harness is one supervised process definition: a command + args + working
// directory the daemon spawns and keeps alive. The daemon knows nothing about
// what runs inside — it is just cmd/args/workdir (ADR-0006).
type Harness struct {
	// Name is the table name, unique across the config.
	Name string
	// Args are the command arguments, appended after the adapter's
	// executable; {workdir} placeholders are expanded at spawn time by the
	// supervisor, not here.
	Args []string
	// Prompt is an agent one-shot instruction, the declarative alternative
	// to a long-running harness: when set, the supervisor synthesizes the
	// entire agent argv at spawn time via the adapter's PromptCommand (Args
	// stay EMPTY), so the file remains the source of truth (ADR-0006) and
	// the prompt text never passes through {workdir} arg expansion.
	// Governing: ADR-0011; issue #56 (harness abstraction for agent CLIs).
	Prompt string
	// PromptFile is the path to a file whose contents are the agent one-shot
	// instruction — the alternative to an inline Prompt for a specification too
	// long to live on one TOML line (a basic string carries no raw newline).
	// Mutually exclusive with Prompt; either satisfies the "is a prompt
	// harness" predicate every prompt-dependent key is validated against.
	//
	// This field holds a PATH, never the file's contents. The supervisor reads
	// it at spawn, immediately before exec, and passes what it read to the
	// adapter's PromptCommand. Storing contents here instead would inline the
	// whole document into the TOML on the next config-writer round-trip (the
	// TUI form re-emits Prompt verbatim), push it through every display
	// surface and the wire, and make each prompt edit require a reload — the
	// same class of failure ADR-0011 avoids by refusing to desugar Model into
	// Args. Reading per spawn also means a scheduled run uses the file as it
	// stands at firing time.
	//
	// Config load validates that the path resolves to a readable, non-empty
	// file; spawn re-checks, because the file can be deleted in between and a
	// run with an empty instruction is a silent no-op.
	// Governing: ADR-0018; SPEC-0006 REQ "Prompt Source".
	PromptFile string
	// Model selects which model the agent CLI runs a prompt harness with.
	// Config truth only, and it requires Prompt (validation enforces it —
	// there is no vendor-agnostic place to inject a flag into an arbitrary
	// cmd's argv, so a cmd harness passes --model through its own Args): the
	// supervisor folds the value into the synthesized agent argv at spawn time
	// via AgentCommand, so it never rides Args and, like the prompt text, is
	// exempt from {workdir} arg expansion.
	// Governing: ADR-0011; issue #57 (add `model` field for model selection).
	Model string
	// AutoAccept enables unattended/yolo mode for a prompt harness, bypassing
	// the agent CLI's permission prompts. Config truth only, and it requires
	// Prompt (validation enforces it — there is no vendor-agnostic place to
	// inject a flag into an arbitrary cmd's argv, so a cmd harness passes its
	// tool's flag through its own Args): the supervisor folds the vendor's
	// yolo flag into the synthesized agent argv at spawn time via
	// AgentCommand, so it never rides Args.
	// WARNING: this bypasses ALL of the agent's permission prompts — ADR-0008
	// names an unattended yolo agent reachable over network attach as the top
	// threat — so it belongs only on trusted, headless runs.
	// Governing: ADR-0008; ADR-0011; issue #58 (add `auto_accept` field for
	// unattended mode).
	AutoAccept bool
	// Quiet controls whether a prompt one-shot runs headless (no interactive
	// output — the default, matching the synthesized argv's --quiet). Config
	// truth only, and it requires Prompt when explicitly set (validation
	// enforces it): set `quiet = false` on a prompt harness to have the agent
	// stream its output to whoever attaches. The field never rides Args — a
	// cmd harness passes its tool's own tone flag through Args (issue #60).
	// Governing: ADR-0011; issue #60 (add `quiet` field for headless output
	// suppression).
	Quiet bool
	// MaxTurns caps the number of agent iterations a prompt harness may run
	// before the CLI stops, the budget guard for unattended one-shots. Config
	// truth only, and it requires Prompt (validation enforces it — there is no
	// vendor-agnostic place to inject a flag into an arbitrary cmd's argv, so
	// a cmd harness passes --max-turns through its own Args): the supervisor
	// folds the value into the synthesized agent argv at spawn time via
	// AgentCommand, so it never rides Args. 0 means unset/unlimited (the flag
	// is simply not emitted).
	// Governing: ADR-0011; issue #59 (add `max_turns` field for budget
	// capping).
	MaxTurns int
	// Workdir is the process working directory (may contain a leading ~).
	Workdir string
	// EnvFile is a file of KEY=VALUE pairs sourced before launch (ADR-0008;
	// secrets stay here, out of the config).
	EnvFile string
	// RestartDelay is the base delay between a crash and a respawn.
	RestartDelay time.Duration
	// Restart controls whether the harness is automatically restarted after it
	// exits, mirroring Docker Compose's `restart` directive. Empty means
	// "default" (always restart while enabled), preserving the daemon's
	// historical behavior. Valid values: "no", "always", "unless-stopped",
	// "on-failure". The config parsers normalize an omitted key to "always" —
	// except for prompt harnesses, which default to "no": a one-shot agent run
	// exiting 0 must not respawn (an explicit `restart = ...` still wins).
	Restart RestartPolicy
	// Backend selects the hosting strategy (default native, ADR-0003).
	Backend Backend
	// Description is shown in the TUI list (ADR-0006).
	Description string
	// Enabled is whether the daemon autostarts this harness independent of any
	// profile (ADR-0006). Profiles are the primary autostart mechanism.
	Enabled bool
	// TmuxSocket names the tmux socket; inert unless Backend == tmux (ADR-0006
	// keeps it for backward compatibility).
	TmuxSocket string
	// Schedule is a cron expression that fires this harness on a cadence.
	// At each firing the daemon starts the harness if it is not already
	// running (an overlapping firing is skipped, not stacked). The run exiting
	// is terminal for that firing; the restart policy applies only to abnormal
	// exit if configured (validation rejects "always"/"unless-stopped" here).
	// Requires Prompt (a one-shot agent run is the use case) and is mutually
	// exclusive with Enabled and with profile membership (autostart and
	// schedule are distinct concerns). Global config only — project files
	// reject it. Governing: ADR-0013 (the decision to express this as a key on
	// [harness.*] rather than a [job.*] table); SPEC-0008 REQ "Schedule Key",
	// REQ "Schedule Exclusions"; issue #66; ADR-0006 (schema); ADR-0011 (prompt
	// one-shot); SPEC-0003 (enabled-intent model the exclusion carves against).
	Schedule string
	// Adapter is the harness kind — the config `harness` key, an enum:
	// "crush" (the default when omitted), "claude-code", "codex",
	// "generic". It selects the adapter, which supplies BOTH the
	// tool-specific behaviour (trajectory discovery, prompt flag mapping)
	// and the executable a long-running (non-prompt) harness runs; `args`
	// are appended after it. Naming an unknown value is a
	// config-validation error. "generic" has no executable of its own, so
	// it is only valid for a prompt harness. Governing: ADR-0011, SPEC-0006
	// REQ "Adapter Selection".
	Adapter string
	// HarvestTrajectory controls whether the harness's trajectory is exposed
	// read-only through the facade (list_trajectories / get_trajectory).
	// Defaults to false: a trajectory may contain secrets the harnessed
	// program printed itself (ADR-0008), so exposure is opt-in per harness.
	// A harness that has not opted in is omitted from list results and
	// get_trajectory refuses with a structured error. Governing: ADR-0008,
	// SPEC-0006 REQ "Harvest Opt-In".
	HarvestTrajectory bool
	// MCPAllow is the per-harness capability scope for the MCP facade
	// (SPEC-0005 REQ "Capability Scoping"). Defaults to ["read"]; including
	// "write" permits write-class facade tools (harness_start, harness_stop,
	// harness_restart). Trajectory tools are read-class and available to
	// every harness, but still gated by HarvestTrajectory. Global config only
	// — a project file declaring mcp_allow is rejected, so a cloned
	// repository cannot grant its own harnesses write authority over the
	// fleet. Governing: SPEC-0005 REQ "Capability Scoping".
	MCPAllow []string
}

// ReadPromptFile reads the instruction text a PromptFile names. It is the one
// place that decides what makes a prompt file usable, so the config parser's
// eager check and the supervisor's spawn-time read cannot drift into
// disagreeing — a file the load accepted must be a file the spawn can run.
//
// path must already be resolved (~ expanded, relative made absolute); callers
// own that because the two sides resolve against different bases. An empty or
// whitespace-only file is an error, not an empty prompt: launching an agent
// with no instruction is the silent no-op ADR-0018 exists to remove.
// Governing: ADR-0018; SPEC-0006 REQ "Prompt Source".
func ReadPromptFile(path string) (string, error) {
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "", fmt.Errorf("%q does not exist", path)
	case err != nil:
		return "", fmt.Errorf("%q is not readable: %w", path, err)
	case info.IsDir():
		return "", fmt.Errorf("%q is a directory, not a file", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("%q is not readable: %w", path, err)
	}
	prompt := strings.TrimSpace(string(b))
	if prompt == "" {
		return "", fmt.Errorf("%q is empty", path)
	}
	return prompt, nil
}

// IsAgent reports whether h is an agent one-shot — a harness whose argv is
// synthesized at spawn from a prompt (ADR-0011) rather than run from configured
// args. Either prompt source counts: Prompt carries the instruction inline,
// PromptFile names a file the supervisor reads at spawn (ADR-0018). Use this
// wherever "is this a prompt harness?" is asked, so a prompt_file harness is
// never mistaken for a cmd harness by a bare Prompt != "" check.
// Governing: SPEC-0006 REQ "Prompt Source".
func (h Harness) IsAgent() bool { return h.Prompt != "" || h.PromptFile != "" }

// AgentOpts carries the config-truth knobs a prompt harness folds into its
// synthesized agent argv at spawn time. Every field stays verbatim — none are
// {workdir} placeholders — and a cmd harness ignores them all (config
// validation forbids the combinations; the wire def keeps them inert).
type AgentOpts struct {
	// Quiet runs the one-shot headless (no interactive output); emitted as
	// --quiet when true. The default for a prompt one-shot.
	Quiet bool
	// Model selects the agent model, emitted as --model when non-empty.
	Model string
	// AutoAccept enables unattended/yolo mode, emitted as --yolo.
	AutoAccept bool
	// MaxTurns caps agent iterations, emitted as --max-turns when > 0.
	MaxTurns int
}

// QualifiedName returns the daemon-wide name a project-local harness registers
// under: `<project>/<harness>` (e.g. "reduit/agent"). Bare global harness names
// stay un-prefixed. Governing: ADR-0009 (project-scoped compose), SPEC-0004 REQ
// "Project Naming And Namespacing".
func QualifiedName(project, harness string) string {
	return project + "/" + harness
}

// Profile is a named set of harnesses you "hop into" (ADR-0006). It is a view
// plus an autostart set; membership is by harness name reference.
type Profile struct {
	// Name is the profile table name, unique across the config.
	Name string
	// Description is shown in the TUI (ADR-0006).
	Description string
	// Harnesses lists member harness names, in file order.
	Harnesses []string
	// Autostart marks a profile the daemon brings up on start (ADR-0005/0006).
	Autostart bool
}

// AuthorizedKey is one entry of the remote SSH allowlist: an OpenSSH
// public-key line plus whether that key attaches read-only. Governing: ADR-0008
// (SSH public-key auth; optional per-key read-only scoping), SPEC-0002.
type AuthorizedKey struct {
	// Line is the raw `authorized_keys` line (type base64 [comment]) as written
	// in config or the keys file. It is parsed by the SSH server, not here.
	Line string
	// ReadOnly marks a key that may only open read-only attaches — enforced by
	// the remote session opening the TUI in read-only mode (ADR-0008).
	ReadOnly bool
}

// ServerConfig is the optional Wish SSH remote-access front door (ADR-0004,
// ADR-0008). It is off unless Enabled is set; enabling it is a deliberate
// config step (bind address + an authorized-keys allowlist). Secrets never live
// here — only public keys and paths (ADR-0008).
type ServerConfig struct {
	// Enabled turns the Wish SSH server on. Off by default (ADR-0008: remote is
	// opt-in).
	Enabled bool
	// Listen is the SSH bind address, host:port. Empty means the daemon's
	// loopback default (ADR-0008: bind narrowly by default).
	Listen string
	// AuthorizedKeys is the inline allowlist of SSH public keys permitted to
	// attach. Only listed keys may connect — there are no unauthenticated
	// sessions (ADR-0008).
	AuthorizedKeys []AuthorizedKey
	// AuthorizedKeysFile is an optional path to an OpenSSH `authorized_keys`
	// file whose entries are merged with AuthorizedKeys.
	AuthorizedKeysFile string
	// HostKeyPath overrides the persisted host-key location; empty uses the
	// default under $XDG_STATE_HOME/harness (ADR-0008).
	HostKeyPath string
}

// Config is a fully parsed, validated harness.toml: the harness registry and
// the profiles, each preserving file order for stable rendering.
type Config struct {
	// Harnesses is every harness keyed by name.
	Harnesses map[string]Harness
	// Profiles is every profile keyed by name.
	Profiles map[string]Profile
	// HarnessOrder is harness names in the order they appear in the file.
	HarnessOrder []string
	// ProfileOrder is profile names in the order they appear in the file.
	ProfileOrder []string
	// Server is the optional [server] remote-access configuration (ADR-0004).
	Server ServerConfig
	// Daemon is the optional [daemon] configuration (issue #98).
	Daemon DaemonConfig
}

// DaemonConfig carries optional daemon-level settings ([daemon] table).
type DaemonConfig struct {
	// WatchConfig controls whether the daemon watches harness.toml and
	// per-harness env files for changes and auto-reloads (issue #98). Default
	// is true (watching on); set watch_config = false to opt out.
	WatchConfig *bool
	// OTelEndpoint is the OTLP/HTTP endpoint the daemon ships agent traces to
	// (e.g. "https://cairn.stump.wtf/v1/traces" or a Honeycomb/Tempo
	// endpoint). When set, the daemon builds OTel traces from harvested
	// sessions via agent-trace's otel.BuildTrace and POSTs them as standard
	// OTLP JSON. Any OTLP-compatible endpoint works — Harness does not know
	// or care what is on the other end. Per-harness opt-in via
	// harvest_trajectory still gates which harnesses contribute traces.
	// Governing: ADR-0008 (secrets), SPEC-0006 REQ "Trajectory Discovery".
	OTelEndpoint string
}

// WatchConfigEnabled reports whether config watching is enabled, defaulting
// to true when the key is absent.
func (d DaemonConfig) WatchConfigEnabled() bool {
	if d.WatchConfig == nil {
		return true
	}
	return *d.WatchConfig
}

// OrderedHarnesses returns the harnesses in file order.
func (c *Config) OrderedHarnesses() []Harness {
	out := make([]Harness, 0, len(c.HarnessOrder))
	for _, name := range c.HarnessOrder {
		out = append(out, c.Harnesses[name])
	}
	return out
}

// OrderedProfiles returns the profiles in file order.
func (c *Config) OrderedProfiles() []Profile {
	out := make([]Profile, 0, len(c.ProfileOrder))
	for _, name := range c.ProfileOrder {
		out = append(out, c.Profiles[name])
	}
	return out
}

// AutostartHarnesses returns the set of harness names the daemon should bring
// up on start: any Enabled harness, plus every member of an autostart profile
// (ADR-0005 REQ "Autostart", SPEC-0003 REQ "Autostart").
func (c *Config) AutostartHarnesses() []string {
	seen := make(map[string]bool)
	var out []string
	add := func(name string) {
		if _, ok := c.Harnesses[name]; !ok {
			return
		}
		if seen[name] {
			return
		}
		seen[name] = true
		out = append(out, name)
	}
	for _, name := range c.HarnessOrder {
		if c.Harnesses[name].Enabled {
			add(name)
		}
	}
	for _, pname := range c.ProfileOrder {
		p := c.Profiles[pname]
		if !p.Autostart {
			continue
		}
		for _, hn := range p.Harnesses {
			add(hn)
		}
	}
	return out
}

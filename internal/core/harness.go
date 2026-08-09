package core

// Governing: ADR-0006 (configuration & profiles — the harness/profile schema),
// ADR-0003 (backend: native vs tmux), ADR-0002 (the daemon's registry holds
// these parsed records). These are the core domain types every other package
// (config, supervisor, protocol, tui) imports.

import (
	"strconv"
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
	// Cmd is the executable to run (required unless Prompt is set).
	Cmd string
	// Args are the command arguments; {workdir} placeholders are expanded at
	// spawn time by the supervisor, not here.
	Args []string
	// Prompt is an agent one-shot instruction, the declarative alternative to
	// Cmd: exactly one of the two is set (config validation enforces it, and
	// Args belong to Cmd only). Cmd/Args stay EMPTY for a prompt harness — the
	// supervisor synthesizes the agent argv at spawn time via AgentCommand, so
	// the file remains the source of truth (ADR-0006) and the prompt text
	// never passes through {workdir} arg expansion.
	// Governing: ADR-0011; issue #56 (harness abstraction for agent CLIs).
	Prompt string
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
}

// AgentOpts carries the config-truth knobs a prompt harness folds into its
// synthesized agent argv at spawn time. Every field stays verbatim — none are
// {workdir} placeholders — and a cmd harness ignores them all (config
// validation forbids the combinations; the wire def keeps them inert).
type AgentOpts struct {
	// Model selects the agent model, emitted as --model when non-empty.
	Model string
	// AutoAccept enables unattended/yolo mode, emitted as --yolo.
	AutoAccept bool
	// MaxTurns caps agent iterations, emitted as --max-turns when > 0.
	MaxTurns int
}

// AgentCommand returns the argv a prompt harness spawns: the agent CLI, the
// optional --model selection (issue #57), the optional --yolo unattended flag
// (issue #58), the optional --max-turns budget (issue #59), then the prompt,
// verbatim, as the FINAL argv element — flags precede the prompt so the
// instruction text stays last. Callers pass the opts through untouched: they
// are config truth, not configured args, so none go through the {workdir}
// placeholder expansion the supervisor applies to Args.
// Governing: ADR-0011 — interim single-vendor synthesis; the SPEC-0006 adapter
// registry replaces this call site (and maps auto-accept and the turn budget
// onto each vendor's own flag).
func AgentCommand(prompt string, opts AgentOpts) (cmd string, args []string) {
	args = []string{"run", "--quiet"}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	if opts.AutoAccept {
		args = append(args, "--yolo")
	}
	if opts.MaxTurns > 0 {
		args = append(args, "--max-turns", strconv.Itoa(opts.MaxTurns))
	}
	return "crush", append(args, prompt)
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

package core

// Governing: ADR-0006 (configuration & profiles — the harness/profile schema),
// ADR-0003 (backend: native vs tmux), ADR-0002 (the daemon's registry holds
// these parsed records). These are the core domain types every other package
// (config, supervisor, protocol, tui) imports.

import "time"

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

// Harness is one supervised process definition: a command + args + working
// directory the daemon spawns and keeps alive. The daemon knows nothing about
// what runs inside — it is just cmd/args/workdir (ADR-0006).
type Harness struct {
	// Name is the table name, unique across the config.
	Name string
	// Cmd is the executable to run (required).
	Cmd string
	// Args are the command arguments; {workdir} placeholders are expanded at
	// spawn time by the supervisor, not here.
	Args []string
	// Workdir is the process working directory (may contain a leading ~).
	Workdir string
	// EnvFile is a file of KEY=VALUE pairs sourced before launch (ADR-0008;
	// secrets stay here, out of the config).
	EnvFile string
	// RestartDelay is the base delay between a crash and a respawn.
	RestartDelay time.Duration
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

// Config is a fully parsed, validated harnessd.toml: the harness registry and
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

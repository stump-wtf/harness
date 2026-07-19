// Package config parses harnessd.toml into the core domain types.
//
// Governing: ADR-0006 (TOML stays; [harness.*] tables with bare-[name]
// backward compatibility, [profile.*] tables; file is the source of truth) and
// ADR-0001 (BurntSushi/toml, dropping the python tomllib dependency).
// Validation errors carry a source line for the SPEC-0001 reload banner.
package config

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// rawHarness mirrors a harness TOML table before validation/normalization.
// Enabled is a pointer so we can tell "absent" (default false) from an explicit
// value without ambiguity.
type rawHarness struct {
	Cmd          string   `toml:"cmd"`
	Args         []string `toml:"args"`
	Workdir      string   `toml:"workdir"`
	EnvFile      string   `toml:"env_file"`
	RestartDelay int      `toml:"restart_delay"`
	Backend      string   `toml:"backend"`
	Description  string   `toml:"description"`
	Enabled      *bool    `toml:"enabled"`
	TmuxSocket   string   `toml:"tmux_socket"`
}

// rawProfile mirrors a [profile.*] TOML table before validation.
type rawProfile struct {
	Description string   `toml:"description"`
	Harnesses   []string `toml:"harnesses"`
	Autostart   bool     `toml:"autostart"`
}

// DefaultPath returns the conventional config location,
// $XDG_CONFIG_HOME/harnessd/harnessd.toml (falling back to ~/.config).
func DefaultPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "harnessd.toml"
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "harnessd", "harnessd.toml")
}

// Load reads and parses the config file at path.
func Load(path string) (*core.Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data, path)
}

// Parse parses raw TOML into a validated *core.Config. filename is used only
// for error messages (source location). Every failure is a *Error carrying the
// offending line where one can be determined (ADR-0006, SPEC-0001).
func Parse(data []byte, filename string) (*core.Config, error) {
	var top map[string]toml.Primitive
	md, err := toml.Decode(string(data), &top)
	if err != nil {
		return nil, syntaxError(filename, err)
	}

	// Table headers in file order give us both ordering and per-table line
	// numbers for validation errors, deterministically — BurntSushi's map
	// iteration order is not stable.
	headers := scanTables(data)

	// Decode the [harness.*] and [profile.*] namespaces lazily.
	var harnessNS, profileNS map[string]toml.Primitive
	if p, ok := top["harness"]; ok {
		if err := md.PrimitiveDecode(p, &harnessNS); err != nil {
			return nil, newError(filename, lineOf(headers, "harness"), "[harness]: %v", err)
		}
	}
	if p, ok := top["profile"]; ok {
		if err := md.PrimitiveDecode(p, &profileNS); err != nil {
			return nil, newError(filename, lineOf(headers, "profile"), "[profile]: %v", err)
		}
	}

	cfg := &core.Config{
		Harnesses: map[string]core.Harness{},
		Profiles:  map[string]core.Profile{},
	}

	// Defer profile member validation until every harness is known.
	type pendingProfile struct {
		profile core.Profile
		line    int
	}
	var pending []pendingProfile

	for _, h := range headers {
		switch {
		case len(h.parts) == 1 && h.parts[0] == "harness":
			continue // the namespace parent header itself
		case len(h.parts) == 1 && h.parts[0] == "profile":
			continue

		case len(h.parts) == 1:
			// Bare [name] table — backward-compatible harness (ADR-0006).
			name := h.parts[0]
			var rh rawHarness
			if err := md.PrimitiveDecode(top[name], &rh); err != nil {
				return nil, newError(filename, h.line, "[%s]: %v", name, err)
			}
			if err := addHarness(cfg, filename, name, h.line, rh); err != nil {
				return nil, err
			}

		case len(h.parts) == 2 && h.parts[0] == "harness":
			name := h.parts[1]
			var rh rawHarness
			if err := md.PrimitiveDecode(harnessNS[name], &rh); err != nil {
				return nil, newError(filename, h.line, "[harness.%s]: %v", name, err)
			}
			if err := addHarness(cfg, filename, name, h.line, rh); err != nil {
				return nil, err
			}

		case len(h.parts) == 2 && h.parts[0] == "profile":
			name := h.parts[1]
			var rp rawProfile
			if err := md.PrimitiveDecode(profileNS[name], &rp); err != nil {
				return nil, newError(filename, h.line, "[profile.%s]: %v", name, err)
			}
			if _, exists := cfg.Profiles[name]; exists {
				return nil, newError(filename, h.line, "duplicate profile %q", name)
			}
			p := core.Profile{
				Name:        name,
				Description: rp.Description,
				Harnesses:   rp.Harnesses,
				Autostart:   rp.Autostart,
			}
			cfg.Profiles[name] = p
			cfg.ProfileOrder = append(cfg.ProfileOrder, name)
			pending = append(pending, pendingProfile{profile: p, line: h.line})

		default:
			// Deeper nesting like [harness.foo.bar] or [a.b] is not part of
			// the ADR-0006 schema.
			return nil, newError(filename, h.line, "unrecognized table [%s]", strings.Join(h.parts, "."))
		}
	}

	// Validate profile membership now that all harnesses are registered.
	for _, pp := range pending {
		for _, member := range pp.profile.Harnesses {
			if _, ok := cfg.Harnesses[member]; !ok {
				return nil, newError(filename, pp.line,
					"profile %q references unknown harness %q", pp.profile.Name, member)
			}
		}
	}

	return cfg, nil
}

// addHarness validates a raw harness table and registers it on cfg.
func addHarness(cfg *core.Config, filename, name string, line int, rh rawHarness) error {
	if _, exists := cfg.Harnesses[name]; exists {
		return newError(filename, line, "duplicate harness %q", name)
	}
	if strings.TrimSpace(rh.Cmd) == "" {
		return newError(filename, line, "harness %q: missing required key \"cmd\"", name)
	}

	backend := core.Backend(rh.Backend)
	if rh.Backend == "" {
		backend = core.BackendNative
	} else if !backend.Valid() {
		return newError(filename, line,
			"harness %q: invalid backend %q (want \"native\" or \"tmux\")", name, rh.Backend)
	}

	if rh.RestartDelay < 0 {
		return newError(filename, line,
			"harness %q: restart_delay must not be negative (got %d)", name, rh.RestartDelay)
	}

	enabled := false
	if rh.Enabled != nil {
		enabled = *rh.Enabled
	}

	h := core.Harness{
		Name:         name,
		Cmd:          rh.Cmd,
		Args:         rh.Args,
		Workdir:      rh.Workdir,
		EnvFile:      rh.EnvFile,
		RestartDelay: time.Duration(rh.RestartDelay) * time.Second,
		Backend:      backend,
		Description:  rh.Description,
		Enabled:      enabled,
		TmuxSocket:   rh.TmuxSocket,
	}
	cfg.Harnesses[name] = h
	cfg.HarnessOrder = append(cfg.HarnessOrder, name)
	return nil
}

// syntaxError converts a BurntSushi decode error into a location-carrying
// *Error. BurntSushi's ParseError carries a Position with the 1-based line.
func syntaxError(filename string, err error) error {
	var pe toml.ParseError
	if errors.As(err, &pe) {
		msg := pe.Message
		if msg == "" {
			msg = err.Error()
		}
		return newError(filename, pe.Position.Line, "%s", msg)
	}
	return newError(filename, 0, "%s", err.Error())
}

// tableHeader is a parsed TOML table header and the line it sits on.
type tableHeader struct {
	parts []string // dotted key parts, e.g. ["harness", "claude-src"]
	line  int      // 1-based
}

// headerRe matches a standard table header line ("[a.b]"), tolerating leading
// whitespace and a trailing comment. Array-of-tables ("[[…]]") is excluded —
// the schema has no array tables.
var headerRe = regexp.MustCompile(`^\s*\[\s*([^\[\]]+?)\s*\]\s*(?:#.*)?$`)

// scanTables extracts every table header in file order with its line number.
// Ordering and line attribution come from the source text (deterministic),
// while values come from the TOML decoder (authoritative).
func scanTables(data []byte) []tableHeader {
	var out []tableHeader
	for i, line := range strings.Split(string(data), "\n") {
		m := headerRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		out = append(out, tableHeader{parts: splitKey(m[1]), line: i + 1})
	}
	return out
}

// splitKey splits a dotted TOML key into parts, stripping optional quotes on
// each segment. Our harness/profile names are bare keys (letters, digits, '-',
// '_'), but quoted segments are handled defensively.
func splitKey(key string) []string {
	var parts []string
	for _, seg := range strings.Split(key, ".") {
		seg = strings.TrimSpace(seg)
		seg = strings.Trim(seg, `"'`)
		parts = append(parts, seg)
	}
	return parts
}

// lineOf returns the line of the first header whose full dotted key matches
// want, or 0.
func lineOf(headers []tableHeader, want string) int {
	for _, h := range headers {
		if strings.Join(h.parts, ".") == want {
			return h.line
		}
	}
	return 0
}

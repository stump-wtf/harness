// Package config parses harness.toml into the core domain types.
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

// rawServer mirrors the [server] table before validation (ADR-0004/0008 remote
// access). authorized_keys accepts either bare key lines or [[server.key]]
// sub-tables carrying a per-key read_only flag; both are merged.
type rawServer struct {
	Enabled            bool              `toml:"enabled"`
	Listen             string            `toml:"listen"`
	AuthorizedKeys     []string          `toml:"authorized_keys"`
	AuthorizedKeysFile string            `toml:"authorized_keys_file"`
	HostKeyPath        string            `toml:"host_key"`
	Keys               []rawAuthzKeyTOML `toml:"key"`
}

// rawAuthzKeyTOML is a [[server.key]] sub-table: an SSH public-key line with an
// optional read_only annotation (ADR-0008 per-key read-only scoping).
type rawAuthzKeyTOML struct {
	Key      string `toml:"key"`
	ReadOnly bool   `toml:"read_only"`
}

// DefaultPath returns the conventional config location,
// $XDG_CONFIG_HOME/harness/harness.toml (falling back to ~/.config).
func DefaultPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "harness.toml"
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "harness", "harness.toml")
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
	//
	// The regex scan cannot distinguish a real "[table]" header from a line that
	// merely looks like one inside a multi-line string value (e.g. a bracketed
	// line in a `description`). Cross-check every scanned header against the
	// decoder's authoritative key set so those false positives are dropped —
	// a bracketed line inside a string is never a defined key.
	headers := scanTables(data)
	defined := definedPaths(md)
	realHeaders := headers[:0:0]
	for _, h := range headers {
		if defined[strings.Join(h.parts, ".")] {
			realHeaders = append(realHeaders, h)
		}
	}
	headers = realHeaders

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
	var serverSeen bool

	for _, h := range headers {
		switch {
		case len(h.parts) == 1 && h.parts[0] == "harness":
			continue // the namespace parent header itself
		case len(h.parts) == 1 && h.parts[0] == "profile":
			continue

		case len(h.parts) == 1 && h.parts[0] == "server":
			// The optional remote-access front door (ADR-0004/0008).
			if serverSeen {
				return nil, newError(filename, h.line, "duplicate [server] table")
			}
			serverSeen = true
			var rs rawServer
			if err := md.PrimitiveDecode(top["server"], &rs); err != nil {
				return nil, newError(filename, h.line, "[server]: %v", err)
			}
			sc, err := buildServer(filename, h.line, rs)
			if err != nil {
				return nil, err
			}
			cfg.Server = sc

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

// buildServer validates and normalizes a [server] table into a
// core.ServerConfig (ADR-0004/0008). Bare authorized_keys entries default to
// read-write; [[server.key]] sub-tables carry an explicit read_only flag.
// Enabling the server without any key source is rejected — an unauthenticated
// remote front door is never allowed (ADR-0008).
func buildServer(filename string, line int, rs rawServer) (core.ServerConfig, error) {
	sc := core.ServerConfig{
		Enabled:            rs.Enabled,
		Listen:             strings.TrimSpace(rs.Listen),
		AuthorizedKeysFile: strings.TrimSpace(rs.AuthorizedKeysFile),
		HostKeyPath:        strings.TrimSpace(rs.HostKeyPath),
	}
	for _, k := range rs.AuthorizedKeys {
		if strings.TrimSpace(k) == "" {
			continue
		}
		sc.AuthorizedKeys = append(sc.AuthorizedKeys, core.AuthorizedKey{Line: strings.TrimSpace(k)})
	}
	for _, k := range rs.Keys {
		if strings.TrimSpace(k.Key) == "" {
			return core.ServerConfig{}, newError(filename, line, "[[server.key]]: missing required key \"key\"")
		}
		sc.AuthorizedKeys = append(sc.AuthorizedKeys, core.AuthorizedKey{
			Line:     strings.TrimSpace(k.Key),
			ReadOnly: k.ReadOnly,
		})
	}
	if sc.Enabled && len(sc.AuthorizedKeys) == 0 && sc.AuthorizedKeysFile == "" {
		return core.ServerConfig{}, newError(filename, line,
			"[server]: enabled = true requires authorized_keys or authorized_keys_file (ADR-0008: no unauthenticated remote access)")
	}
	return sc, nil
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

// definedPaths returns the set of every key path the decoder actually parsed,
// dotted (e.g. "harness.foo"). Real table headers appear here; text that only
// looks like a header inside a string value does not — this is what lets Parse
// reject false headers from the source scan.
func definedPaths(md toml.MetaData) map[string]bool {
	m := make(map[string]bool)
	for _, k := range md.Keys() {
		m[strings.Join([]string(k), ".")] = true
	}
	return m
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

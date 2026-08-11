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
	"unicode"

	"github.com/BurntSushi/toml"
	"github.com/robfig/cron/v3"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// rawHarness mirrors a harness TOML table before validation/normalization.
// Enabled is a pointer so we can tell "absent" (default false) from an explicit
// value without ambiguity.
type rawHarness struct {
	Cmd          string   `toml:"cmd"`
	Args         []string `toml:"args"`
	Prompt       string   `toml:"prompt"`
	Model        string   `toml:"model"`
	AutoAccept   bool     `toml:"auto_accept"`
	MaxTurns     *int     `toml:"max_turns"`
	Quiet        *bool    `toml:"quiet"`
	Workdir      string   `toml:"workdir"`
	EnvFile      string   `toml:"env_file"`
	RestartDelay int      `toml:"restart_delay"`
	Restart      string   `toml:"restart"`
	Backend      string   `toml:"backend"`
	Description  string   `toml:"description"`
	Enabled      *bool    `toml:"enabled"`
	TmuxSocket   string   `toml:"tmux_socket"`
	Schedule     string   `toml:"schedule"`
}

// rawProfile mirrors a [profile.*] TOML table before validation.
type rawProfile struct {
	Description string   `toml:"description"`
	Harnesses   []string `toml:"harnesses"`
	Autostart   bool     `toml:"autostart"`
}

// rawDaemon mirrors the [daemon] table before validation.
type rawDaemon struct {
	WatchConfig *bool `toml:"watch_config"`
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
	var serverSeen, daemonSeen bool

	for _, h := range headers {
		switch {
		case len(h.parts) == 1 && h.parts[0] == "harness":
			continue // the namespace parent header itself
		case len(h.parts) == 1 && h.parts[0] == "profile":
			continue

		case len(h.parts) == 1 && h.parts[0] == "daemon":
			// The optional daemon-level config (issue #98: watch_config).
			if daemonSeen {
				return nil, newError(filename, h.line, "duplicate [daemon] table")
			}
			daemonSeen = true
			var rd rawDaemon
			if err := md.PrimitiveDecode(top["daemon"], &rd); err != nil {
				return nil, newError(filename, h.line, "[daemon]: %v", err)
			}
			cfg.Daemon = core.DaemonConfig{WatchConfig: rd.WatchConfig}

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
			h, ok := cfg.Harnesses[member]
			if !ok {
				return nil, newError(filename, pp.line,
					"profile %q references unknown harness %q", pp.profile.Name, member)
			}
			// A scheduled harness must not be profile-startable: profile
			// autostart (and use-profile) would fire the one-shot outside its
			// schedule, the exact coupling the enabled exclusion forbids
			// (issue #66).
			if h.Schedule != "" {
				return nil, newError(filename, pp.line,
					"profile %q includes scheduled harness %q (\"schedule\" and profile membership are mutually exclusive)", pp.profile.Name, member)
			}
		}
	}

	return cfg, nil
}

// addHarness validates a raw harness table and registers it on cfg.
func addHarness(cfg *core.Config, filename, name string, line int, rh rawHarness) error {
	// "/" is reserved for the `<project>/<harness>` namespace: a (TOML-quoted)
	// global name containing it could shadow or clobber a registered project
	// harness. Governing: ADR-0009, SPEC-0004 REQ "Project Naming And
	// Namespacing".
	if strings.Contains(name, "/") {
		return newError(filename, line,
			"harness %q: name must not contain \"/\" (reserved for project namespacing)", name)
	}
	// Global semantics: `enabled` defaults to false (autostart is opt-in) and
	// workdir/env_file are stored verbatim.
	return registerHarness(cfg, filename, name, line, rh, false, nil)
}

// registerHarness is the shared validate/normalize/register body behind the
// global config's addHarness and the project file's addProjectHarness —
// SPEC-0004 REQ "Project File Schema" requires identical field meanings, and
// one body is how the two parsers cannot drift. The genuine deltas arrive as
// parameters: defaultEnabled (global autostart is opt-in, project bring-up is
// opt-out) and resolve, applied to workdir/env_file (project files resolve
// relative paths against the project root; nil stores them verbatim).
func registerHarness(cfg *core.Config, filename, name string, line int, rh rawHarness, defaultEnabled bool, resolve func(string) string) error {
	if _, exists := cfg.Harnesses[name]; exists {
		return newError(filename, line, "duplicate harness %q", name)
	}

	// Exactly one of cmd/prompt defines what runs. A prompt harness stores
	// only the prompt: its argv is synthesized at spawn time by the supervisor
	// (core.AgentCommand, ADR-0011), never desugared here — the file stays the
	// source of truth (ADR-0006).
	cmd := strings.TrimSpace(rh.Cmd)
	prompt := strings.TrimSpace(rh.Prompt)
	switch {
	case rh.Prompt != "" && prompt == "":
		return newError(filename, line, "harness %q: \"prompt\" must not be blank", name)
	case cmd == "" && prompt == "":
		return newError(filename, line, "harness %q: missing required key \"cmd\" (or set \"prompt\" for agent one-shot mode)", name)
	case cmd != "" && prompt != "":
		return newError(filename, line, "harness %q: \"prompt\" and \"cmd\" are mutually exclusive", name)
	case prompt != "" && len(rh.Args) > 0:
		return newError(filename, line,
			"harness %q: \"prompt\" and \"args\" are mutually exclusive (args configure a cmd; the agent argv is synthesized at spawn)", name)
	}

	// `model` is config truth only: stored on the harness and folded into the
	// synthesized agent argv at spawn time (core.AgentCommand, ADR-0011),
	// never desugared into args here — a parse-time flag corrupts the TOML
	// round-trip (the form would re-persist synthesized args) and there is no
	// vendor-agnostic place to inject a flag into an arbitrary cmd's argv, so
	// `model` requires `prompt` (a cmd harness passes --model through args
	// itself). Governing: issue #57 (add `model` field for model selection).
	model := strings.TrimSpace(rh.Model)
	switch {
	case rh.Model != "" && model == "":
		return newError(filename, line, "harness %q: \"model\" must not be blank", name)
	case strings.ContainsFunc(model, unicode.IsSpace):
		return newError(filename, line,
			"harness %q: \"model\" must be a single token (model ids carry no whitespace)", name)
	case model != "" && prompt == "":
		return newError(filename, line,
			"harness %q: \"model\" requires \"prompt\" (a cmd harness passes --model through its own args)", name)
	}

	// `auto_accept` is config truth only, same contract as `model`: stored on
	// the harness and folded into the synthesized agent argv at spawn time
	// (core.AgentCommand, ADR-0011) as the vendor's yolo flag, never desugared
	// into args here — a parse-time flag corrupts the TOML round-trip (the
	// form would re-persist synthesized args) and there is no vendor-agnostic
	// place to inject a flag into an arbitrary cmd's argv, so `auto_accept`
	// requires `prompt` (a cmd harness passes its tool's flag through its own
	// args). A plain bool, deliberately: absent and explicit false both mean
	// "attended" — there is no third state worth distinguishing (unlike
	// `enabled`, whose omitted default is context-dependent).
	// Governing: issue #58 (add `auto_accept` field for unattended mode).
	if rh.AutoAccept && prompt == "" {
		return newError(filename, line,
			"harness %q: \"auto_accept\" requires \"prompt\" (a cmd harness passes its tool's flag through its own args)", name)
	}

	// `max_turns` is config truth only, same contract as `model` and
	// `auto_accept`: stored on the harness and folded into the synthesized
	// agent argv at spawn time (core.AgentCommand, ADR-0011) as a --max-turns
	// budget, never desugared into args here — a parse-time flag corrupts the
	// TOML round-trip (the form would re-persist synthesized args) and there
	// is no vendor-agnostic place to inject a flag into an arbitrary cmd's
	// argv, so `max_turns` requires `prompt` (a cmd harness passes its tool's
	// flag through its own args). 0 means unset/unlimited — the flag is not
	// emitted.
	// Governing: issue #59 (add `max_turns` field for budget capping).
	maxTurns := 0
	if rh.MaxTurns != nil {
		if *rh.MaxTurns < 0 {
			return newError(filename, line,
				"harness %q: \"max_turns\" must not be negative (got %d)", name, *rh.MaxTurns)
		}
		if prompt == "" {
			return newError(filename, line,
				"harness %q: \"max_turns\" requires \"prompt\" (a cmd harness passes --max-turns through its own args)", name)
		}
		maxTurns = *rh.MaxTurns
	}

	// `quiet` is config truth only, same contract as `model`/`auto_accept`: a
	// prompt one-shot runs headless by default, and this field opts OUT of
	// that (quiet = false lets the agent stream output to whoever attaches).
	// Normalized via *bool so an omitted key (nil = the headless default) is
	// distinguishable from an explicit `quiet = false`. Since it routes
	// through the synthesized agent argv (core.AgentCommand), an
	// explicitly-set quiet requires `prompt` — a cmd harness passes its
	// tool's own tone flag through args rather than relying on injection into
	// an arbitrary argv.
	// Governing: issue #60 (add `quiet` field for headless output
	// suppression).
	quiet := false
	if prompt != "" {
		// A prompt one-shot is headless by default.
		quiet = true
	}
	if rh.Quiet != nil {
		if prompt == "" {
			return newError(filename, line,
				"harness %q: \"quiet\" requires \"prompt\" (a cmd harness passes its tool's tone flag through its own args)", name)
		}
		quiet = *rh.Quiet
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

	restartPolicy := core.RestartPolicy(rh.Restart)
	if !restartPolicy.Valid() {
		return newError(filename, line,
			"harness %q: invalid restart policy %q (want \"no\", \"always\", \"unless-stopped\", or \"on-failure\")",
			name, rh.Restart)
	}
	if restartPolicy == "" {
		// Omitted key = the documented default. Normalizing here keeps one
		// canonical in-memory spelling, so an explicit `restart = "always"`
		// compares equal to the default everywhere downstream. Prompt
		// harnesses default to "no" instead: a one-shot agent run exiting 0
		// must not respawn (an explicit `restart = ...` still wins).
		restartPolicy = core.RestartAlways
		if prompt != "" {
			restartPolicy = core.RestartNo
		}
	}

	enabled := defaultEnabled
	if rh.Enabled != nil {
		enabled = *rh.Enabled
	}

	// Schedule marks a daemon-owned cron one-shot (issue #66; ADR-0011 prompt
	// harness; the enabled exclusion carves against SPEC-0003's intent model).
	schedule := strings.TrimSpace(rh.Schedule)
	switch {
	case rh.Schedule != "" && schedule == "":
		return newError(filename, line,
			"harness %q: \"schedule\" must not be blank", name)
	case schedule != "" && prompt == "":
		return newError(filename, line,
			"harness %q: \"schedule\" requires \"prompt\" (a scheduled harness is a one-shot agent run)", name)
	case schedule != "" && enabled:
		return newError(filename, line,
			"harness %q: \"schedule\" and \"enabled = true\" are mutually exclusive (use one or the other)", name)
	case schedule != "" && (restartPolicy == core.RestartAlways || restartPolicy == core.RestartUnlessStopped):
		return newError(filename, line,
			"harness %q: \"schedule\" requires restart policy \"no\" or \"on-failure\" (a scheduled run must be allowed to finish; %q respawns it after a clean exit)", name, restartPolicy)
	}
	if schedule != "" {
		// Validate the cron expression eagerly, like every sibling field: a
		// typo must fail the load with a located error, not silently never
		// fire (the scheduler's own parse at apply time is defense in depth).
		if _, err := cron.ParseStandard(schedule); err != nil {
			return newError(filename, line,
				"harness %q: invalid \"schedule\" %q: %v", name, schedule, err)
		}
	}

	if resolve == nil {
		resolve = func(p string) string { return p }
	}

	h := core.Harness{
		Name:         name,
		Cmd:          rh.Cmd,
		Args:         rh.Args,
		AutoAccept:   rh.AutoAccept,
		MaxTurns:     maxTurns,
		Model:        model,
		Prompt:       prompt,
		Quiet:        quiet,
		Workdir:      resolve(rh.Workdir),
		EnvFile:      resolve(rh.EnvFile),
		RestartDelay: time.Duration(rh.RestartDelay) * time.Second,
		Restart:      restartPolicy,
		Backend:      backend,
		Description:  rh.Description,
		Enabled:      enabled,
		TmuxSocket:   rh.TmuxSocket,
		Schedule:     schedule,
	}
	if prompt != "" {
		// Cmd/Args stay EMPTY for a prompt harness (spawn-time synthesis,
		// ADR-0011) — also defensively squashing a whitespace-only cmd.
		h.Cmd, h.Args = "", nil
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

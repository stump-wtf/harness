// Package config parses harness.toml into the core domain types.
//
// Governing: ADR-0006 (TOML stays; [harness.*] tables with bare-[name]
// backward compatibility, [profile.*] tables; file is the source of truth) and
// ADR-0001 (BurntSushi/toml, dropping the python tomllib dependency).
// Validation errors carry a source line for the SPEC-0001 reload banner.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/BurntSushi/toml"
	"github.com/robfig/cron/v3"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/hours"
)

// rawHarness mirrors a harness TOML table before validation/normalization.
// Enabled is a pointer so we can tell "absent" (default false) from an explicit
// value without ambiguity.
type rawHarness struct {
	Harness string   `toml:"harness"`
	Args    []string `toml:"args"`
	// Argv is a `command` harness's whole process (SPEC-0017 REQ-2), and is
	// rejected on every other kind. A plain slice checked on presence
	// (non-nil), so `argv = []` on a claude-code harness is still refused.
	Argv       []string `toml:"argv"`
	Prompt     string   `toml:"prompt"`
	PromptFile string   `toml:"prompt_file"`
	Model      string   `toml:"model"`
	// AutoAccept is a pointer so a `command` harness can reject the key on
	// presence (SPEC-0017 REQ-3): `auto_accept = false` there does nothing,
	// which is still a mistake worth hearing about at load.
	AutoAccept        *bool    `toml:"auto_accept"`
	MaxTurns          *int     `toml:"max_turns"`
	Quiet             *bool    `toml:"quiet"`
	Workdir           string   `toml:"workdir"`
	EnvFile           string   `toml:"env_file"`
	RestartDelay      int      `toml:"restart_delay"`
	Restart           string   `toml:"restart"`
	Backend           string   `toml:"backend"`
	Description       string   `toml:"description"`
	Enabled           *bool    `toml:"enabled"`
	TmuxSocket        string   `toml:"tmux_socket"`
	Schedule          string   `toml:"schedule"`
	CatchUp           *bool    `toml:"catch_up"`
	Timeout           *string  `toml:"timeout"`
	OnOverlap         *string  `toml:"on_overlap"`
	KeepRuns          *int     `toml:"keep_runs"`
	HarvestTrajectory *bool    `toml:"harvest_trajectory"`
	MCPAllow          []string `toml:"mcp_allow"`
	// ExportTelemetry is the per-harness telemetry opt-in; nil follows
	// [telemetry] export_all (SPEC-0015 REQ-1).
	ExportTelemetry *bool `toml:"export_telemetry"`

	// Triggers binds the harness to [channel.*]/[webhook.*] sources
	// (SPEC-0014 REQ "Triggers Key"). A plain slice, unlike the pointer
	// fields above: nil and [] both mean "no event sources", and there is no
	// exclusion that fires on presence rather than content — an empty list
	// binds nothing and so excludes nothing.
	Triggers []string `toml:"triggers"`

	// OperatingHours gates a resident harness to weekly windows (ADR-0019).
	// Plain string like Schedule: blank-vs-absent is checked the same way
	// (rh.OperatingHours != "" && trimmed == "" is the blank-value error).
	OperatingHours string `toml:"operating_hours"`
	// HoursShutdown and HoursShutdownTimeout are pointers, like CatchUp/
	// OnOverlap: the "requires operating_hours" exclusion (SPEC-0012 REQ
	// "Operating Hours Exclusions") is checked on PRESENCE, not value — an
	// explicit `hours_shutdown = "graceful"` (the same as the default) must
	// still be rejected without operating_hours, exactly as an explicit
	// `catch_up = false` is rejected without schedule.
	HoursShutdown        *string `toml:"hours_shutdown"`
	HoursShutdownTimeout *string `toml:"hours_shutdown_timeout"`

	// Removed keys, still decoded so their presence can be REJECTED with a
	// migration error. TOML decoding here ignores unknown keys, so deleting
	// these fields outright would make a pre-enum config load clean and then
	// run something else entirely: `cmd = "npm"` + `args = ["run", "dev"]`
	// silently becomes the default crush adapter invoked as `crush run dev`.
	// Delete-not-deprecate still owes the user a loud failure.
	RemovedCmd   string `toml:"cmd"`
	RemovedAgent string `toml:"agent"`
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
	// RemovedOTelEndpoint is decoded only so its presence can be REJECTED
	// with a migration error (SPEC-0015 REQ-13), like rawHarness's removed
	// keys: unknown keys fail anyway, but this one deserves the way forward.
	RemovedOTelEndpoint *string `toml:"otel_endpoint"`
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
	// MetricsListen / MetricsTokenFile configure the Prometheus listener
	// (ADR-0020, SPEC-0013 REQ-1). The token is read from the file at daemon
	// start; harness.toml only ever holds its path (ADR-0008).
	MetricsListen    string `toml:"metrics_listen"`
	MetricsTokenFile string `toml:"metrics_token_file"`
	// HarnessD is an optional directory whose *.toml files are loaded as
	// additional harness definitions after the main config. Each file may
	// contain [harness.*] tables only (no [server], [profile.*], or [daemon]).
	// Files are sorted lexicographically; duplicate harness names across files
	// or with the main config are rejected. This lets operators add/remove
	// harness configs one file at a time without editing the main config.
	// A leading ~ expands to the home directory and a relative path resolves
	// against the config file's own directory (see resolveConfigPath).
	HarnessD string `toml:"harness_d"`

	// The opt-in webhook listener SPEC-0014 adds. Absent means no HTTP port
	// is opened at all, however many [webhook.*] tables are declared.
	WebhookListen      string `toml:"webhook_listen"`
	WebhookTLSCertFile string `toml:"webhook_tls_cert_file"`
	WebhookTLSKeyFile  string `toml:"webhook_tls_key_file"`
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

	// Array-of-tables headers never reach the switch below, so validate them
	// here — before checkUndecoded can misattribute their contents.
	hasServer := false
	for _, h := range headers {
		if len(h.parts) == 1 && h.parts[0] == "server" {
			hasServer = true
			break
		}
	}
	if err := checkArrayTables(data, filename, hasServer); err != nil {
		return nil, err
	}

	// Decode the [harness.*], [profile.*] and trigger-source namespaces
	// lazily.
	var harnessNS, profileNS, channelNS, webhookNS map[string]toml.Primitive
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
	if p, ok := top["channel"]; ok {
		if err := md.PrimitiveDecode(p, &channelNS); err != nil {
			return nil, newError(filename, lineOf(headers, "channel"), "[channel]: %v", err)
		}
	}
	if p, ok := top["webhook"]; ok {
		if err := md.PrimitiveDecode(p, &webhookNS); err != nil {
			return nil, newError(filename, lineOf(headers, "webhook"), "[webhook]: %v", err)
		}
	}

	cfg := &core.Config{
		Harnesses: map[string]core.Harness{},
		Profiles:  map[string]core.Profile{},
		Telemetry: core.DefaultTelemetryConfig(),
	}

	// Defer profile member validation until every harness is known.
	type pendingProfile struct {
		profile core.Profile
		line    int
	}
	var pending []pendingProfile
	var serverSeen, daemonSeen, telemetrySeen bool
	var harnessDPath string

	// Sources and the harnesses that bind them form one config view across
	// the main file and every drop-in, so references between them are
	// resolved after the last file rather than as each table is read.
	// Governing: SPEC-0014 REQ "Triggers Key".
	st := newLoadState()

	for _, h := range headers {
		switch {
		case len(h.parts) == 1 && h.parts[0] == "harness":
			continue // the namespace parent header itself
		case len(h.parts) == 1 && h.parts[0] == "profile":
			continue
		// The trigger-source namespace parents. These cases sit BEFORE the
		// bare-[name] fallback below, or a lone `[channel]` header would be
		// registered as a backward-compatible harness named "channel".
		// Governing: SPEC-0014 REQ "Channel Source Table", REQ "Webhook
		// Source Table".
		case len(h.parts) == 1 && (h.parts[0] == core.SourceKindChannel || h.parts[0] == core.SourceKindWebhook):
			if err := checkSourceNamespaceParent(md, filename, h.line, h.parts[0]); err != nil {
				return nil, err
			}

		case len(h.parts) == 2 && h.parts[0] == core.SourceKindChannel:
			name := h.parts[1]
			var rc rawChannel
			if err := md.PrimitiveDecode(channelNS[name], &rc); err != nil {
				return nil, newError(filename, h.line, "[channel.%s]: %v", name, err)
			}
			if err := addChannel(cfg, st, filename, name, h.line, rc); err != nil {
				return nil, err
			}

		case len(h.parts) == 2 && h.parts[0] == core.SourceKindWebhook:
			name := h.parts[1]
			var rw rawWebhook
			if err := md.PrimitiveDecode(webhookNS[name], &rw); err != nil {
				return nil, newError(filename, h.line, "[webhook.%s]: %v", name, err)
			}
			if err := addWebhook(cfg, st, filename, name, h.line, rw); err != nil {
				return nil, err
			}

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
			if rd.RemovedOTelEndpoint != nil {
				return nil, removedOTelEndpointErr(filename, lineOfKeyInTable(data, "daemon", "otel_endpoint"))
			}
			cfg.Daemon = core.DaemonConfig{WatchConfig: rd.WatchConfig}

		case len(h.parts) == 1 && h.parts[0] == "telemetry":
			// The global telemetry export table (ADR-0022, SPEC-0015 REQ-2).
			if telemetrySeen {
				return nil, newError(filename, h.line, "duplicate [telemetry] table")
			}
			telemetrySeen = true
			var rt rawTelemetry
			if err := md.PrimitiveDecode(top["telemetry"], &rt); err != nil {
				return nil, newError(filename, h.line, "[telemetry]: %v", err)
			}
			tc, err := buildTelemetry(filename, data, h.line, rt)
			if err != nil {
				return nil, err
			}
			cfg.Telemetry = tc

		case len(h.parts) == 2 && h.parts[0] == "telemetry" && h.parts[1] == "headers":
			return nil, telemetryHeadersErr(filename, h.line)

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
			harnessDPath = strings.TrimSpace(rs.HarnessD)

		case len(h.parts) == 1:
			// Bare [name] table — backward-compatible harness (ADR-0006).
			name := h.parts[0]
			var rh rawHarness
			if err := md.PrimitiveDecode(top[name], &rh); err != nil {
				return nil, newError(filename, h.line, "[%s]: %v", name, err)
			}
			if err := addHarness(cfg, st, filename, name, h.line, rh); err != nil {
				return nil, err
			}

		case len(h.parts) == 2 && h.parts[0] == "harness":
			name := h.parts[1]
			var rh rawHarness
			if err := md.PrimitiveDecode(harnessNS[name], &rh); err != nil {
				return nil, newError(filename, h.line, "[harness.%s]: %v", name, err)
			}
			if err := addHarness(cfg, st, filename, name, h.line, rh); err != nil {
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

	// Fail loudly on any key the schema does not know (issue #2): a typo in
	// a known table was previously silently dropped.
	if err := checkUndecoded(md, data, filename); err != nil {
		return nil, err
	}

	// Load additional harness definitions from [server] harness_d directory.
	// Each *.toml file may contain [harness.*] tables only — no [server],
	// [profile.*], or [daemon]. Files are sorted lexicographically for
	// deterministic merge order; duplicate names are rejected.
	//
	// This runs BEFORE profile validation, not after: a [profile.*] in the main
	// config naming a drop-in harness is the whole point of the directory, and
	// validating membership first rejected it as an unknown harness.
	if harnessDPath != "" {
		if err := loadHarnessD(cfg, st, resolveConfigPath(harnessDPath, filename)); err != nil {
			return nil, err
		}
	}

	// Every file has been read, so `triggers` references can finally be
	// resolved against the whole view: a drop-in harness may bind a source
	// from the main file, and vice versa.
	// Governing: SPEC-0014 REQ "Triggers Key".
	if err := resolveTriggers(cfg, st); err != nil {
		return nil, err
	}
	warnNonLoopbackWebhook(cfg)

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
			// The same coupling, for the same reason: profile autostart (and
			// `use-profile`) would start a triggered one-shot with no event,
			// so its run would have no event file and nothing to act on.
			// Governing: SPEC-0014 REQ "Triggered Harness Exclusions".
			if len(h.Triggers) > 0 {
				return nil, newError(filename, pp.line,
					"profile %q includes triggered harness %q (\"triggers\" and profile membership are mutually exclusive: autostart would fire the one-shot with no event)", pp.profile.Name, member)
			}
		}
	}

	return cfg, nil
}

// addHarness validates a raw harness table and registers it on cfg, recording
// where it was declared so a `triggers` reference resolved after the last file
// can still report the right line.
func addHarness(cfg *core.Config, st *loadState, filename, name string, line int, rh rawHarness) error {
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
	if err := registerHarness(cfg, filename, name, line, rh, false, nil); err != nil {
		return err
	}
	st.harnessAt[name] = declSite{file: filename, line: line}
	return nil
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

	// Reject the keys the `harness` enum replaced, before anything else: a
	// config carrying them was written against the old schema, and every
	// other error message would be a red herring.
	if strings.TrimSpace(rh.RemovedCmd) != "" {
		return newError(filename, line,
			"harness %q: \"cmd\" was replaced by the \"harness\" enum — set harness = \"crush\"|\"claude-code\"|\"codex\" for an agent, or harness = \"command\" with argv = [%q, …] to run an arbitrary program",
			name, strings.TrimSpace(rh.RemovedCmd))
	}
	if strings.TrimSpace(rh.RemovedAgent) != "" {
		return newError(filename, line,
			"harness %q: \"agent\" was renamed to \"harness\" — use harness = %q",
			name, strings.TrimSpace(rh.RemovedAgent))
	}

	// The `harness` enum key selects the adapter (and, for a long-running
	// harness, the executable it runs). It is REQUIRED and has no default: an
	// omitted key used to mean "crush", so a typo'd table name, a half-written
	// stanza, or a stray `[harness.x]` silently launched an agent instead of
	// failing the load. What runs is the single most consequential thing a
	// harness declares — it is worth one explicit word. A prompt harness
	// stores only the prompt: its argv is synthesized at spawn time from the
	// same adapter (ADR-0011), never desugared here — the file stays the
	// source of truth (ADR-0006).
	adapter := strings.TrimSpace(rh.Harness)
	switch {
	case rh.Harness == "":
		return newError(filename, line,
			"harness %q: missing required key \"harness\" (want one of: crush, claude-code, codex, generic, command — use \"command\" with argv = [\"…\"] for an arbitrary program)",
			name)
	case adapter == "":
		return newError(filename, line, "harness %q: \"harness\" must not be blank", name)
	}
	switch adapter {
	case "crush", "claude-code", "codex", "generic", core.AdapterCommand:
	default:
		return newError(filename, line,
			"harness %q: unknown harness kind %q (want one of: crush, claude-code, codex, generic, command)",
			name, adapter)
	}
	// `generic` runs sh; it has no prompt synthesis. It used to borrow
	// Crush's, so an operator whose CLI was not in the list wrote `generic` +
	// `prompt` and got `crush run <prompt>`, or a crash loop naming a binary
	// they never configured. Checked before either prompt key is validated or
	// prompt_file is read: whatever those say, this harness cannot run them.
	// Governing: ADR-0023, SPEC-0017 REQ "Generic Kind Rejects Prompts".
	if adapter == "generic" {
		for _, k := range []struct{ key, val string }{{"prompt", rh.Prompt}, {"prompt_file", rh.PromptFile}} {
			if k.val != "" {
				return newError(filename, line,
					"harness %q: \"generic\" runs sh and has no prompt synthesis, so it takes no %q; use harness = \"crush\"|\"claude-code\"|\"codex\" for a prompt one-shot, or harness = \"command\" with argv to run another program without a shell",
					name, k.key)
			}
		}
	}
	if err := checkCommandKeys(adapter, rh); err != nil {
		return newError(filename, line, "harness %q: %v", name, err)
	}
	prompt := strings.TrimSpace(rh.Prompt)
	switch {
	case rh.Prompt != "" && prompt == "":
		return newError(filename, line, "harness %q: \"prompt\" must not be blank", name)
	case prompt != "" && len(rh.Args) > 0:
		return newError(filename, line,
			"harness %q: \"prompt\" and \"args\" are mutually exclusive (args configure a long-running harness; the agent argv is synthesized at spawn)", name)
	}

	// `prompt_file` names a file whose contents are the instruction — the
	// alternative to an inline `prompt` for a specification too long to sit on
	// one TOML line. Only the PATH is stored; the supervisor reads it at spawn
	// (ADR-0018). The path is resolved and checked eagerly for the reason
	// SPEC-0008 parses the cron expression eagerly: a scheduled one-shot whose
	// instruction file is missing would otherwise fire into a no-op with
	// nobody attached to notice. That is deliberately stricter than
	// `env_file`, which tolerates a missing file because a harness with no
	// extra environment still runs correctly — a harness with no prompt has
	// nothing to run at all.
	// Governing: ADR-0018; SPEC-0006 REQ "Prompt Source".
	promptFile := strings.TrimSpace(rh.PromptFile)
	switch {
	case rh.PromptFile != "" && promptFile == "":
		return newError(filename, line, "harness %q: \"prompt_file\" must not be blank", name)
	case promptFile != "" && prompt != "":
		return newError(filename, line,
			"harness %q: \"prompt\" and \"prompt_file\" are mutually exclusive (inline the instruction or name a file holding it, not both)", name)
	case promptFile != "" && len(rh.Args) > 0:
		return newError(filename, line,
			"harness %q: \"prompt_file\" and \"args\" are mutually exclusive (args configure a long-running harness; the agent argv is synthesized at spawn)", name)
	}

	// Either prompt source makes this an agent one-shot, so every
	// prompt-dependent key below tests isAgent rather than `prompt` alone —
	// otherwise a prompt_file harness would be rejected for setting `model` or
	// `schedule` and would inherit the always-restart cmd default.
	isAgent := prompt != "" || promptFile != ""

	// Resolve prompt_file against the file that declared it, deliberately
	// NOT through the shared `resolve` (which is nil for the global config, so
	// workdir/env_file there stay raw until spawn expands ~). A relative
	// prompt_file cannot afford that treatment: config.Load runs from the CLI
	// and from the daemon, which systemd starts with an arbitrary cwd, so a
	// cwd-relative path would validate in one process and fail in the other.
	// Resolving against the declaring file — the same rule `harness_d` already
	// uses — makes the path mean one thing everywhere. A project file passes
	// its own resolver, which anchors on the project root instead.
	promptFilePath := promptFile
	if promptFile != "" {
		if resolve != nil {
			promptFilePath = resolve(promptFile)
		} else {
			promptFilePath = resolveConfigPath(promptFile, filename)
		}
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
	case model != "" && !isAgent:
		return newError(filename, line,
			"harness %q: \"model\" requires \"prompt\" or \"prompt_file\" (a cmd harness passes --model through its own args)", name)
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
	autoAccept := rh.AutoAccept != nil && *rh.AutoAccept
	if autoAccept && !isAgent {
		return newError(filename, line,
			"harness %q: \"auto_accept\" requires \"prompt\" or \"prompt_file\" (a cmd harness passes its tool's flag through its own args)", name)
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
		if !isAgent {
			return newError(filename, line,
				"harness %q: \"max_turns\" requires \"prompt\" or \"prompt_file\" (a cmd harness passes --max-turns through its own args)", name)
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
	if isAgent {
		// A prompt one-shot is headless by default.
		quiet = true
	}
	if rh.Quiet != nil {
		if !isAgent {
			return newError(filename, line,
				"harness %q: \"quiet\" requires \"prompt\" or \"prompt_file\" (a cmd harness passes its tool's tone flag through its own args)", name)
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
		if isAgent {
			restartPolicy = core.RestartNo
		}
	}

	enabled := defaultEnabled
	if rh.Enabled != nil {
		enabled = *rh.Enabled
	}

	// Schedule marks a daemon-owned cron one-shot. The exclusions below are
	// load-bearing, not defensive: they are what lets a key on [harness.*]
	// stay unambiguous where ADR-0013 originally wanted a [job.*] table kind.
	// Governing: ADR-0013; SPEC-0008 REQ "Schedule Key", REQ "Schedule
	// Exclusions"; issue #66; ADR-0011 (prompt harness; the enabled exclusion
	// carves against SPEC-0003's intent model).
	schedule := strings.TrimSpace(rh.Schedule)
	switch {
	case rh.Schedule != "" && schedule == "":
		return newError(filename, line,
			"harness %q: \"schedule\" must not be blank", name)
	case schedule != "" && !isAgent:
		return newError(filename, line,
			"harness %q: \"schedule\" requires \"prompt\" or \"prompt_file\" (a scheduled harness is a one-shot agent run)", name)
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
	// `triggers` binds the harness to event sources. It combines with
	// `schedule` — a harness may fire on a clock and on an event — and
	// carries the same exclusions for the same reasons: a triggered harness
	// is a one-shot agent run, so autostart intent, profile membership and a
	// respawning restart policy would each fire it with no event to act on.
	//
	// Only the SHAPE of each reference is checked here. Whether the named
	// source exists is answered once every file in the view has been read
	// (resolveTriggers), because a drop-in harness may bind a main-file
	// source and vice versa.
	// Governing: ADR-0021; SPEC-0014 REQ "Triggers Key", REQ "Triggered
	// Harness Exclusions".
	triggers, err := parseTriggers(filename, name, line, rh.Triggers)
	if err != nil {
		return err
	}
	switch {
	case len(triggers) > 0 && !isAgent:
		return newError(filename, line,
			"harness %q: \"triggers\" requires \"prompt\" or \"prompt_file\" (a triggered harness is a one-shot agent run)", name)
	case len(triggers) > 0 && enabled:
		return newError(filename, line,
			"harness %q: \"triggers\" and \"enabled = true\" are mutually exclusive (autostart intent and on-demand firing are distinct)", name)
	case len(triggers) > 0 && (restartPolicy == core.RestartAlways || restartPolicy == core.RestartUnlessStopped):
		return newError(filename, line,
			"harness %q: \"triggers\" requires restart policy \"no\" or \"on-failure\" (a triggered run must be allowed to finish; %q would respawn the one-shot with no event)", name, restartPolicy)
	}
	// Triggered is the predicate SPEC-0014 extends SPEC-0008's run machinery
	// to: a webhook-only harness gets run records, logs, timeout, on_overlap
	// and keep_runs exactly as a cron one-shot does.
	triggered := schedule != "" || len(triggers) > 0

	// catch_up decides what happens to firings that elapsed while nobody was
	// evaluating them. Three things can miss one: a cron `schedule`, a
	// channel source (whose stream can be down across a laptop's night), and
	// `operating_hours` (whose window can open while the daemon is stopped).
	// A webhook source cannot: a delivery that arrived with nothing listening
	// was refused at the socket, and Harness is a trigger, not a queue.
	//
	// Rejected on presence, not only when true, for the same reason as every
	// exclusion above: a key that silently does nothing is a mistake the
	// operator should hear about at load, not discover at 03:00.
	// Governing: SPEC-0008 REQ "Missed Window Handling"; SPEC-0014 REQ
	// "Triggered Harness Exclusions", REQ "Channel Catch-Up"; issue #117.
	hasChannelTrigger := false
	for _, t := range triggers {
		if strings.HasPrefix(t, core.SourceKindChannel+".") {
			hasChannelTrigger = true
			break
		}
	}
	// operating_hours counts only on a harness with `triggers`: REQ
	// "Operating Hours On Triggered Harnesses" is what gives catch_up a
	// meaning there (one run at the first in-hours evaluation after an
	// out-of-hours skip). A RESIDENT hours-gated harness has no firings to
	// miss, and nothing in the runtime reads CatchUp for one, so accepting
	// the key there would bring back the silent no-op this check exists
	// to refuse.
	hoursGatedFirings := strings.TrimSpace(rh.OperatingHours) != "" && len(triggers) > 0
	if rh.CatchUp != nil && schedule == "" && !hasChannelTrigger && !hoursGatedFirings {
		return newError(filename, line,
			"harness %q: \"catch_up\" requires \"schedule\", a channel trigger, or \"operating_hours\" on a harness with \"triggers\" (nothing else can miss a firing: a webhook delivery with nobody listening was refused at the socket, and a resident harness has no firings)", name)
	}
	catchUp := rh.CatchUp != nil && *rh.CatchUp

	// timeout, on_overlap and keep_runs shape the runs a firing produces, so
	// like catch_up they are rejected on presence when nothing fires this
	// harness. SPEC-0014 widens the predicate from "has a schedule" to
	// "is triggered": an event source produces runs the same way a clock
	// does. With either, each key gets its documented default, so every
	// triggered harness has a bounded run and a bounded history whether or
	// not the operator thought to ask.
	// Governing: ADR-0013; SPEC-0008 REQ "Run Timeout", REQ "Overlap Policy",
	// REQ "Run History"; ADR-0021; SPEC-0014 REQ "Triggered Harness
	// Exclusions", REQ "Overlap Default For Triggered Harnesses"; issue #119.
	for _, k := range []struct {
		key string
		set bool
	}{{"timeout", rh.Timeout != nil}, {"on_overlap", rh.OnOverlap != nil}, {"keep_runs", rh.KeepRuns != nil}} {
		if k.set && !triggered {
			return newError(filename, line,
				"harness %q: %q requires \"schedule\" or \"triggers\" (it applies to the runs a firing produces, and nothing fires this harness)", name, k.key)
		}
	}
	var (
		timeout  time.Duration
		overlap  core.OverlapPolicy
		keepRuns int
	)
	if triggered {
		timeout = core.DefaultRunTimeout
		if rh.Timeout != nil {
			d, err := time.ParseDuration(strings.TrimSpace(*rh.Timeout))
			if err != nil || d < 0 {
				return newError(filename, line,
					"harness %q: invalid \"timeout\" %q (want a duration such as \"45m\" or \"2h\", or \"0\" for no limit)", name, *rh.Timeout)
			}
			timeout = d
		}
		// The default differs by what fires the harness, and the asymmetry
		// is deliberate. A cron firing that arrives during a run is the SAME
		// work coming round again, so skipping it loses nothing. An event
		// firing is a DIFFERENT event — a second pull request, a second
		// doorbell — and dropping it loses work nothing will re-deliver,
		// because Harness is a trigger and not a queue. So a schedule skips
		// and a trigger queues. An explicit value always wins.
		// Governing: SPEC-0014 REQ "Overlap Default For Triggered
		// Harnesses".
		overlap = core.OverlapSkip
		if len(triggers) > 0 {
			overlap = core.OverlapQueue
		}
		if rh.OnOverlap != nil {
			overlap = core.OverlapPolicy(strings.TrimSpace(*rh.OnOverlap))
			if overlap == "" || !overlap.Valid() {
				return newError(filename, line,
					"harness %q: invalid \"on_overlap\" %q (want \"skip\", \"queue\", or \"replace\")", name, *rh.OnOverlap)
			}
		}
		keepRuns = core.DefaultKeepRuns
		if rh.KeepRuns != nil {
			if *rh.KeepRuns < 1 {
				return newError(filename, line, "harness %q: \"keep_runs\" must be at least 1 (got %d)", name, *rh.KeepRuns)
			}
			keepRuns = *rh.KeepRuns
		}
	}

	// operating_hours gates a resident harness to weekly windows during which
	// it is allowed to run (ADR-0019): outside them the daemon holds it down
	// without touching `enabled`. Mutually exclusive with `schedule` — a
	// scheduled one-shot is already time-gated by its own cron expression, so
	// stacking a second time gate on it would be redundant at best and
	// contradictory at worst. Governing: ADR-0019; SPEC-0012 REQ "Operating
	// Hours Key", REQ "Operating Hours Exclusions".
	operatingHours := strings.TrimSpace(rh.OperatingHours)
	switch {
	case rh.OperatingHours != "" && operatingHours == "":
		return newError(filename, line,
			"harness %q: \"operating_hours\" must not be blank", name)
	case operatingHours != "" && schedule != "":
		return newError(filename, line,
			"harness %q: \"schedule\" and \"operating_hours\" are mutually exclusive (a scheduled one-shot is already time-gated by its cron expression)", name)
	}
	var hoursExpr hours.Expr
	if operatingHours != "" {
		// Validate (and parse) the expression eagerly, like schedule's cron
		// expression above: a typo must fail the load with a located error,
		// not silently gate nothing.
		expr, err := hours.Parse(operatingHours)
		if err != nil {
			return newError(filename, line,
				"harness %q: invalid \"operating_hours\" %q: %v", name, operatingHours, err)
		}
		hoursExpr = expr
	}

	// hours_shutdown/hours_shutdown_timeout decide how a gated harness's close
	// runs and mean nothing without operating_hours — rejected on presence,
	// not only on a non-default value, for the same reason catch_up is
	// rejected without schedule above: a key that silently does nothing is a
	// mistake the operator should hear about at load. Both are *supervision*
	// keys (core.Harness doc), never consulted at spawn, so neither forces a
	// restart when it changes on reload (internal/supervisor's runAffecting
	// only lists spawn-affecting fields; leaving these two off that list is
	// what keeps that promise). Governing: ADR-0019; SPEC-0012 REQ "Shutdown
	// Mode".
	// Like timeout/on_overlap/keep_runs above, the defaults below apply only
	// alongside the key they belong to: an ungated harness keeps the zero
	// value for both (never consulted, since nothing ever reads them without
	// OperatingHours set first).
	var (
		shutdownMode    core.HoursShutdownMode
		shutdownTimeout time.Duration
	)
	if rh.HoursShutdown != nil && operatingHours == "" {
		return newError(filename, line,
			"harness %q: \"hours_shutdown\" requires \"operating_hours\" (a shutdown mode with no hours to close does nothing)", name)
	}
	if rh.HoursShutdownTimeout != nil && operatingHours == "" {
		return newError(filename, line,
			"harness %q: \"hours_shutdown_timeout\" requires \"operating_hours\" (a shutdown mode with no hours to close does nothing)", name)
	}
	// Both close a RESIDENT session at the end of its window. A triggered
	// run has no resident session to close: it is bounded by `timeout` and
	// ends on its own, so a close mode would have nothing to act on. The
	// schedule half of this is already covered, because operating_hours and
	// schedule are mutually exclusive above; `triggers` is the case
	// SPEC-0014 adds.
	// Governing: SPEC-0014 REQ "Triggered Harness Exclusions".
	for _, k := range []struct {
		key string
		set bool
	}{{"hours_shutdown", rh.HoursShutdown != nil}, {"hours_shutdown_timeout", rh.HoursShutdownTimeout != nil}} {
		if k.set && len(triggers) > 0 {
			return newError(filename, line,
				"harness %q: %q is not accepted on a triggered harness (it closes a resident session; a triggered run is bounded by \"timeout\")", name, k.key)
		}
	}
	if operatingHours != "" {
		shutdownMode = core.HoursShutdownGraceful
		if rh.HoursShutdown != nil {
			shutdownMode = core.HoursShutdownMode(strings.TrimSpace(*rh.HoursShutdown))
			if !shutdownMode.Valid() {
				return newError(filename, line,
					"harness %q: invalid \"hours_shutdown\" %q (want \"graceful\" or \"immediate\")", name, *rh.HoursShutdown)
			}
		}
		shutdownTimeout = core.DefaultHoursShutdownTimeout
		if rh.HoursShutdownTimeout != nil {
			d, err := time.ParseDuration(strings.TrimSpace(*rh.HoursShutdownTimeout))
			if err != nil || d <= 0 {
				return newError(filename, line,
					"harness %q: invalid \"hours_shutdown_timeout\" %q (want a positive duration such as \"15m\")", name, *rh.HoursShutdownTimeout)
			}
			shutdownTimeout = d
		}
	}

	if resolve == nil {
		resolve = func(p string) string { return p }
	}

	h := core.Harness{
		Name:         name,
		Adapter:      adapter,
		Args:         rh.Args,
		Argv:         rh.Argv,
		AutoAccept:   autoAccept,
		MaxTurns:     maxTurns,
		Model:        model,
		Prompt:       prompt,
		PromptFile:   promptFilePath,
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
		CatchUp:      catchUp,
		Timeout:      timeout,
		OnOverlap:    overlap,
		KeepRuns:     keepRuns,

		OperatingHours:       operatingHours,
		HoursExpr:            hoursExpr,
		HoursShutdown:        shutdownMode,
		HoursShutdownTimeout: shutdownTimeout,
		Triggers:             triggers,
	}
	if isAgent {
		// Args stay EMPTY for a prompt harness (spawn-time synthesis,
		// ADR-0011) — also defensively squashing whitespace-only args.
		h.Args = nil
	}
	if h.PromptFile != "" {
		// Check the RESOLVED path so the error names what we would actually
		// open. Eager, for SPEC-0008's reason: a scheduled one-shot pointed at
		// a missing instruction file must fail the load, not fire into a
		// no-op. Spawn re-checks — the file can vanish in between.
		if err := checkPromptFile(h.PromptFile); err != nil {
			return newError(filename, line,
				"harness %q: \"prompt_file\" %s", name, err)
		}
	}

	// SPEC-0005 REQ "Capability Scoping": mcp_allow defaults to ["read"].
	mcpAllow := rh.MCPAllow
	if mcpAllow == nil {
		mcpAllow = []string{"read"}
	}

	h.HarvestTrajectory = rh.HarvestTrajectory != nil && *rh.HarvestTrajectory
	if rh.ExportTelemetry != nil {
		v := *rh.ExportTelemetry
		h.ExportTelemetry = &v
	}
	h.MCPAllow = mcpAllow
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
		AuthorizedKeysFile: expandHome(strings.TrimSpace(rs.AuthorizedKeysFile)),
		HostKeyPath:        expandHome(strings.TrimSpace(rs.HostKeyPath)),
		MetricsListen:      strings.TrimSpace(rs.MetricsListen),
		MetricsTokenFile:   expandHome(strings.TrimSpace(rs.MetricsTokenFile)),
	}
	if err := checkMetricsListen(sc.MetricsListen); err != nil {
		return core.ServerConfig{}, newError(filename, line, "[server]: metrics_listen: %v", err)
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
	// The webhook listener is a separate server from the SSH front door
	// above, with its own bind address and its own TLS pair (SPEC-0014 REQ
	// "Webhook Listener"). `enabled` does not gate it: naming an address is
	// the opt-in.
	if err := buildWebhookServer(&sc, filename, line, rs); err != nil {
		return core.ServerConfig{}, err
	}
	return sc, nil
}

// checkMetricsListen rejects a metrics_listen that could never bind: it must
// be empty (the loopback default), "off", or host:port with a numeric port.
// Whether a non-loopback address has its token is the daemon's startup check,
// not the parser's — the token lives in a file this loader never reads.
// Governing: SPEC-0013 REQ-1.
func checkMetricsListen(addr string) error {
	if addr == "" || addr == "off" {
		return nil
	}
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("%q is not host:port or \"off\": %w", addr, err)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
		return fmt.Errorf("%q has no valid port", addr)
	}
	return nil
}

// checkPromptFile validates a resolved prompt_file path at load time by doing
// exactly what spawn will do — core.ReadPromptFile — and discarding the text.
// Sharing the reader is the point: a file the load accepts is by construction a
// file the spawn can run, so the two checks cannot drift.
// Governing: ADR-0018; SPEC-0006 REQ "Prompt Source".
func checkPromptFile(path string) error {
	_, err := core.ReadPromptFile(path)
	return err
}

// resolveConfigPath turns a path read out of the config file into one the
// process can actually open: a leading ~ becomes the user's home directory, and
// a relative path resolves against the directory holding the config file rather
// than the daemon's working directory (ADR-0005 runs it from systemd, where cwd
// is not the config's directory and nothing relative would resolve).
func resolveConfigPath(p, configFile string) string {
	p = expandHome(p)
	if p == "" || filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(filepath.Dir(configFile), p)
}

// expandHome expands a leading ~ (or ~/) in p to the user's home directory.
func expandHome(p string) string {
	if p == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
		return p
	}
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, p[2:])
		}
	}
	return p
}

// loadHarnessD reads all *.toml files from dir and merges their [harness.*]
// definitions into cfg. Files are sorted lexicographically for deterministic
// ordering. Each file may only contain [harness.*] tables — [server],
// [profile.*], and [daemon] are rejected with a source-located error.
func loadHarnessD(cfg *core.Config, st *loadState, dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("[server] harness_d %q: %w", dir, err)
	}

	// Collect and sort *.toml filenames for deterministic merge order.
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".toml") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("harness_d %s: %w", path, err)
		}
		if err := parseHarnessDFile(cfg, st, data, path); err != nil {
			return err
		}
	}
	return nil
}

// parseHarnessDFile parses a single harness.d TOML file and registers its
// [harness.*] definitions on cfg. Only [harness.*] tables and the trigger
// source tables [channel.*]/[webhook.*] are permitted — no [server],
// [profile.*] or [daemon].
//
// Sources are admitted here for the same reason harnesses are: a drop-in is
// how an operator adds or removes one unit at a time, and a unit that fires on
// a webhook is not a unit until its source travels with it. SPEC-0014 says so
// explicitly ("the global harness.toml and in harness_d drop-in files"), and
// the uniqueness and reference rules span the whole view, which is why the
// shared loadState is threaded in rather than re-created per file.
// Governing: SPEC-0014 REQ "Channel Source Table", REQ "Webhook Source Table".
func parseHarnessDFile(cfg *core.Config, st *loadState, data []byte, filename string) error {
	var top map[string]toml.Primitive
	md, err := toml.Decode(string(data), &top)
	if err != nil {
		return syntaxError(filename, err)
	}

	// Decode the namespaces a drop-in may carry, to access individual members.
	var harnessNS, channelNS, webhookNS map[string]toml.Primitive
	if p, ok := top["harness"]; ok {
		if err := md.PrimitiveDecode(p, &harnessNS); err != nil {
			return newError(filename, lineOf(scanTables(data), "harness"), "[harness]: %v", err)
		}
	}
	if p, ok := top[core.SourceKindChannel]; ok {
		if err := md.PrimitiveDecode(p, &channelNS); err != nil {
			return newError(filename, lineOf(scanTables(data), "channel"), "[channel]: %v", err)
		}
	}
	if p, ok := top[core.SourceKindWebhook]; ok {
		if err := md.PrimitiveDecode(p, &webhookNS); err != nil {
			return newError(filename, lineOf(scanTables(data), "webhook"), "[webhook]: %v", err)
		}
	}

	headers := scanTables(data)
	defined := definedPaths(md)
	for _, h := range headers {
		key := strings.Join(h.parts, ".")
		if !defined[key] {
			continue
		}

		switch {
		case len(h.parts) == 1 && h.parts[0] == "harness":
			continue // namespace parent
		case len(h.parts) == 1 && (h.parts[0] == core.SourceKindChannel || h.parts[0] == core.SourceKindWebhook):
			if err := checkSourceNamespaceParent(md, filename, h.line, h.parts[0]); err != nil {
				return err
			}

		case len(h.parts) == 2 && h.parts[0] == "harness":
			name := h.parts[1]
			p, ok := harnessNS[name]
			if !ok {
				continue
			}
			var rh rawHarness
			if err := md.PrimitiveDecode(p, &rh); err != nil {
				return newError(filename, h.line, "[harness.%s]: %v", name, err)
			}
			if err := addHarness(cfg, st, filename, name, h.line, rh); err != nil {
				return err
			}

		case len(h.parts) == 2 && h.parts[0] == core.SourceKindChannel:
			name := h.parts[1]
			p, ok := channelNS[name]
			if !ok {
				continue
			}
			var rc rawChannel
			if err := md.PrimitiveDecode(p, &rc); err != nil {
				return newError(filename, h.line, "[channel.%s]: %v", name, err)
			}
			if err := addChannel(cfg, st, filename, name, h.line, rc); err != nil {
				return err
			}

		case len(h.parts) == 2 && h.parts[0] == core.SourceKindWebhook:
			name := h.parts[1]
			p, ok := webhookNS[name]
			if !ok {
				continue
			}
			var rw rawWebhook
			if err := md.PrimitiveDecode(p, &rw); err != nil {
				return newError(filename, h.line, "[webhook.%s]: %v", name, err)
			}
			if err := addWebhook(cfg, st, filename, name, h.line, rw); err != nil {
				return err
			}

		default:
			return newError(filename, h.line,
				"harness.d file must not contain [%s] (only [harness.*], [channel.*] and [webhook.*] allowed)", key)
		}
	}
	if err := checkArrayTables(data, filename, false); err != nil {
		return err
	}
	return checkUndecoded(md, data, filename)
}

// checkUndecoded turns BurntSushi's undecoded-key report into a loud,
// source-located error. The lazy PrimitiveDecode dance above only marks keys
// as decoded when they land in a raw struct field, so anything left over is
// a key the schema does not know — a typo (`workir`), a stale key from an
// old config, or a documented-but-unbuilt feature. All of those previously
// produced a harness running with a silently wrong default (issue #2).
//
// Key order from Undecoded() is map-derived and not stable; sort for
// deterministic errors. The line number is a best-effort scan for the key's
// assignment in the source text (0 when not found).
func checkUndecoded(md toml.MetaData, data []byte, filename string) error {
	undecoded := md.Undecoded()
	if len(undecoded) == 0 {
		return nil
	}
	sort.Slice(undecoded, func(i, j int) bool {
		return undecoded[i].String() < undecoded[j].String()
	})
	k := undecoded[0]
	table := strings.Join([]string(k[:len(k)-1]), ".")
	where := "config"
	if table != "" {
		where = "[" + table + "]"
	}
	return newError(filename, lineOfKey(data, k), "unknown key %q in %s", k[len(k)-1], where)
}

// keyAssignRe matches an assignment to a bare or quoted TOML key at the
// start of a line, used to attribute an unknown-key error to a line number.
var keyAssignRe = regexp.MustCompile(`^\s*(?:"([^"]+)"|'([^']+)'|([A-Za-z0-9_-]+))\s*=`)

// lineOfKey finds the 1-based line of the first assignment to the final
// segment of key in data, or 0. Best effort: the schema has no duplicate
// leaf keys within one table in practice, and a wrong-but-nearby line beats
// no line at all.
func lineOfKey(data []byte, key toml.Key) int {
	want := key[len(key)-1]
	for i, line := range strings.Split(string(data), "\n") {
		m := keyAssignRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		name := m[1]
		if name == "" {
			name = m[2]
		}
		if name == "" {
			name = m[3]
		}
		if name == want {
			return i + 1
		}
	}
	return 0
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
// whitespace and a trailing comment. Array-of-tables ("[[…]]") is excluded and
// handled by arrayHeaderRe / checkArrayTables instead.
var headerRe = regexp.MustCompile(`^\s*\[\s*([^\[\]]+?)\s*\]\s*(?:#.*)?$`)

// arrayHeaderRe matches an array-of-tables header line ("[[a.b]]"), tolerating
// leading whitespace and a trailing comment.
var arrayHeaderRe = regexp.MustCompile(`^\s*\[\[\s*([^\[\]]+?)\s*\]\]\s*(?:#.*)?$`)

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

// scanArrayTables extracts every array-of-tables header in file order with its
// line number, the [[…]] counterpart to scanTables.
func scanArrayTables(data []byte) []tableHeader {
	var out []tableHeader
	for i, line := range strings.Split(string(data), "\n") {
		m := arrayHeaderRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		out = append(out, tableHeader{parts: splitKey(m[1]), line: i + 1})
	}
	return out
}

// checkArrayTables validates array-of-tables headers, which scanTables skips
// and the header switch therefore never sees.
//
// [[server.key]] beneath a declared [server] is the only array table in the
// schema (ADR-0008 per-key read-only scoping); serverKeyOK says whether this
// file may carry one. Every other array header used to be dropped in silence.
// Left unvalidated it now reaches checkUndecoded, which reports the table's
// *contents* and so names a perfectly valid key as unknown — "[[harness.foo]]"
// reporting `unknown key "harness"` sends the reader hunting a typo that is
// not there.
func checkArrayTables(data []byte, filename string, serverKeyOK bool) error {
	for _, h := range scanArrayTables(data) {
		path := strings.Join(h.parts, ".")
		switch {
		case path == "server.key" && serverKeyOK:
			continue
		case path == "server.key":
			return newError(filename, h.line, "[[server.key]] requires a [server] table")
		default:
			return newError(filename, h.line, "unrecognized table [[%s]]", path)
		}
	}
	return nil
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

// checkCommandKeys applies the `command` kind's key rules, before any rule
// that assumes an adapter executable: `argv` belongs to `command` alone, and
// `command` takes `argv` in place of `args` and none of the keys that fold
// flags into a synthesized agent argv. Every refusal is on presence, not
// value — a key that silently does nothing is a mistake the operator should
// hear about at load.
//
// Prompts, `schedule` and `triggers` are refused on `command` for now: this
// is the resident slice of SPEC-0017. A prompt needs a delivery path
// (REQ-12) and a one-shot needs the run wiring REQ-3 describes; until they
// exist, loading such a harness would either drop the instruction or run
// the argv on a clock nobody asked to be ignored. The refusals name the gap
// so the error reads as "not yet", not as "wrong".
// Governing: ADR-0023, SPEC-0017 REQ-2 "Command Harness Kind", REQ-3
// "Command Harness Modes And Exclusions".
func checkCommandKeys(adapter string, rh rawHarness) error {
	if adapter != core.AdapterCommand {
		if rh.Argv != nil {
			return fmt.Errorf("\"argv\" is only accepted on harness = \"command\" (a %q harness runs its adapter's executable; use \"args\")", adapter)
		}
		return nil
	}
	if rh.Args != nil {
		return errors.New("\"args\" is not accepted on a command harness: put the whole command line in \"argv\" (argv[0] is the executable)")
	}
	if err := core.CheckCommandArgv(rh.Argv); err != nil {
		return err
	}
	for _, k := range []struct {
		key string
		set bool
	}{{"auto_accept", rh.AutoAccept != nil}, {"max_turns", rh.MaxTurns != nil}, {"quiet", rh.Quiet != nil}} {
		if k.set {
			return fmt.Errorf("%q is not accepted on a command harness: a command harness owns its argv, so put the tool's own flag there", k.key)
		}
	}
	if rh.Model != "" {
		return errors.New("\"model\" is unused: no argv element references {{model}}, so nothing would pass it to the program")
	}
	for _, k := range []struct{ key, val string }{{"prompt", rh.Prompt}, {"prompt_file", rh.PromptFile}} {
		if k.val != "" {
			return fmt.Errorf("%q is not supported on a command harness yet: nothing delivers a prompt to its argv; use harness = \"crush\"|\"claude-code\"|\"codex\" for a prompt one-shot", k.key)
		}
	}
	switch {
	case rh.Schedule != "":
		return errors.New("\"schedule\" is not supported on a command harness yet: a command harness is resident only (use a prompt harness for a scheduled one-shot)")
	case rh.Triggers != nil:
		return errors.New("\"triggers\" is not supported on a command harness yet: a command harness is resident only (use a prompt harness for a triggered one-shot)")
	}
	return nil
}

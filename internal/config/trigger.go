// Trigger source parsing for the global config.
//
// SPEC-0014 adds `[channel.*]` and `[webhook.*]` tables, a `triggers` key on
// `[harness.*]`, and the `[server] webhook_*` keys. This file holds the parts
// that are specific to them: the raw TOML mirrors, the per-table validation,
// and credential resolution from each source's own `env_file`.
//
// Two rules shape everything here. Sources and the harnesses that bind them
// may be declared in different files — the main config and any number of
// `harness_d` drop-ins — so a `triggers` reference cannot be resolved until
// every file has been read; loadState carries that bookkeeping. And a resolved
// credential never becomes a string: it is a core.Secret from the moment it
// leaves the env file, so no error message, log line or projection can print
// it by accident.
//
// Governing: ADR-0021 (on-demand one-shots); SPEC-0014 REQ "Channel Source
// Table", REQ "Webhook Source Table", REQ "Triggers Key", REQ "One Consumer
// Per Endpoint", REQ "Credential Resolution", REQ "Error Handling Standards".
//
// @joestump 09/22/2026 - Introduced with the SPEC-0014 config schema (#454).
package config

import (
	"bufio"
	"fmt"
	"net"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/stump-wtf/harness/internal/core"
)

// rawChannel mirrors a [channel.<name>] TOML table before validation.
// Enabled is a pointer so an omitted key (the `true` default) is
// distinguishable from an explicit `enabled = false`.
type rawChannel struct {
	URL         string            `toml:"url"`
	Headers     map[string]string `toml:"headers"`
	EnvFile     string            `toml:"env_file"`
	Enabled     *bool             `toml:"enabled"`
	Description string            `toml:"description"`
}

// rawWebhook mirrors a [webhook.<name>] TOML table before validation. Every
// optional key that carries a default is a pointer, for the presence-versus-
// value reason the harness table's run keys are: a preset rejects an
// explicitly-set header even when the value happens to match what the preset
// would have used.
type rawWebhook struct {
	Verify          string   `toml:"verify"`
	Secret          string   `toml:"secret"`
	EnvFile         string   `toml:"env_file"`
	Events          []string `toml:"events"`
	SignatureHeader *string  `toml:"signature_header"`
	SignaturePrefix *string  `toml:"signature_prefix"`
	EventHeader     *string  `toml:"event_header"`
	DeliveryHeader  *string  `toml:"delivery_header"`
	MaxBody         *string  `toml:"max_body"`
	RateLimit       *string  `toml:"rate_limit"`
	Enabled         *bool    `toml:"enabled"`
	Description     string   `toml:"description"`
}

// declSite is where something was declared: which file, and the line of its
// table header. Errors raised after every file has been read still have to
// point the operator at the line that caused them, and by then the parser is
// no longer standing in that file.
type declSite struct {
	file string
	line int
}

// loadState is the cross-file bookkeeping a global load needs. The main config
// and its `harness_d` drop-ins form one config view: a harness in a drop-in may
// bind a source from the main file and vice versa, and a source name must be
// unique across all of them. So source declarations and harness declarations
// are recorded as they are read, and the references between them are resolved
// once, after the last file.
//
// A project load passes nil: project files reject both sources and `triggers`
// outright, so there is nothing to defer.
// Governing: SPEC-0014 REQ "Triggers Key", REQ "Channel Source Table".
type loadState struct {
	// channelAt and webhookAt record where each source was declared, keyed by
	// bare name. They are separate maps because the two namespaces are
	// separate: `channel.sb` and `webhook.sb` may coexist.
	channelAt map[string]declSite
	webhookAt map[string]declSite
	// harnessAt records where each harness was declared, so an unresolvable
	// `triggers` reference names the right file and line.
	harnessAt map[string]declSite
	// endpoints maps a normalized channel URL to the source that claimed it,
	// for REQ "One Consumer Per Endpoint".
	endpoints map[string]string
}

func newLoadState() *loadState {
	return &loadState{
		channelAt: map[string]declSite{},
		webhookAt: map[string]declSite{},
		harnessAt: map[string]declSite{},
		endpoints: map[string]string{},
	}
}

// checkSourceName validates a source's table name against the grammar every
// reference, protocol reply and metric label has to survive.
func checkSourceName(filename string, line int, kind, name string) error {
	if !core.SourceNameRe.MatchString(name) {
		return newError(filename, line,
			"[%s.%s]: invalid source name %q (want 1-64 characters matching %s — it appears in triggers references, protocol replies and metric labels)",
			kind, name, name, core.SourceNameRe.String())
	}
	return nil
}

// addChannel validates a [channel.*] table and registers it on cfg.
// Governing: SPEC-0014 REQ "Channel Source Table", REQ "One Consumer Per
// Endpoint", REQ "Credential Resolution".
func addChannel(cfg *core.Config, st *loadState, filename, name string, line int, rc rawChannel) error {
	if err := checkSourceName(filename, line, core.SourceKindChannel, name); err != nil {
		return err
	}
	if prev, dup := st.channelAt[name]; dup {
		return newError(filename, line,
			"duplicate trigger source [channel.%s] (already declared at %s:%d)", name, prev.file, prev.line)
	}

	raw := strings.TrimSpace(rc.URL)
	if raw == "" {
		return newError(filename, line, "[channel.%s]: missing required key \"url\"", name)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return newError(filename, line, "[channel.%s]: invalid \"url\" %q: %v", name, raw, err)
	}
	// Credentials in the URL are refused rather than moved: a URL that
	// carries one leaks it into every log line, proxy and error string that
	// ever prints the endpoint, and the operator is better served by being
	// told where it does belong.
	if u.User != nil {
		return newError(filename, line,
			"[channel.%s]: \"url\" must not carry userinfo — put the credential in headers, e.g. headers = { Authorization = \"Bearer ${TOKEN}\" }", name)
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
	case "http":
		if !isLoopbackHost(u.Hostname()) {
			return newError(filename, line,
				"[channel.%s]: \"url\" must use https for a remote host (the session's credentials would cross the network in cleartext); http is accepted only for localhost, 127.0.0.0/8 and ::1", name)
		}
	default:
		return newError(filename, line,
			"[channel.%s]: invalid \"url\" scheme %q (want \"https\", or \"http\" for a loopback host)", name, u.Scheme)
	}
	if u.Hostname() == "" {
		return newError(filename, line, "[channel.%s]: \"url\" %q names no host", name, raw)
	}

	// One session per endpoint: a channel server rings ONE of the sessions
	// connected to an endpoint, so a second session in the same daemon would
	// swallow the doorbells the first should hear. Naming both sources is the
	// point — the operator has to know which pair collided.
	norm := core.NormalizeEndpoint(raw)
	if other, clash := st.endpoints[norm]; clash {
		prev := st.channelAt[other]
		return newError(filename, line,
			"[channel.%s] and [channel.%s] (%s:%d) name the same endpoint %q: a channel server rings only one session per endpoint, so the second would swallow the first's doorbells",
			name, other, prev.file, prev.line, norm)
	}

	envFile := strings.TrimSpace(rc.EnvFile)
	if rc.EnvFile != "" && envFile == "" {
		return newError(filename, line, "[channel.%s]: \"env_file\" must not be blank", name)
	}
	envPath := ""
	if envFile != "" {
		envPath = resolveConfigPath(envFile, filename)
	}

	src := core.ChannelSource{
		Name:        name,
		URL:         raw,
		EnvFile:     envPath,
		Enabled:     rc.Enabled == nil || *rc.Enabled,
		Description: rc.Description,
	}
	if len(rc.Headers) > 0 {
		res := newEnvResolver(envPath)
		src.Headers = make(map[string]core.Secret, len(rc.Headers))
		// Sorted, so a config with two bad headers always reports the same
		// one: map order would make the error text flap between runs.
		for _, k := range sortedKeys(rc.Headers) {
			if strings.TrimSpace(k) == "" {
				return newError(filename, line, "[channel.%s]: \"headers\" has an empty header name", name)
			}
			v, err := res.expand(rc.Headers[k])
			if err != nil {
				return newError(filename, line, "[channel.%s]: headers.%s: %v", name, k, err)
			}
			src.Headers[k] = v
		}
		if w := res.warning(); w != "" {
			cfg.Warnings = append(cfg.Warnings, w)
		}
	}

	if cfg.Channels == nil {
		cfg.Channels = map[string]core.ChannelSource{}
	}
	cfg.Channels[name] = src
	cfg.ChannelOrder = append(cfg.ChannelOrder, name)
	st.channelAt[name] = declSite{file: filename, line: line}
	st.endpoints[norm] = name
	return nil
}

// addWebhook validates a [webhook.*] table and registers it on cfg.
// Governing: SPEC-0014 REQ "Webhook Source Table", REQ "Credential
// Resolution".
func addWebhook(cfg *core.Config, st *loadState, filename, name string, line int, rw rawWebhook) error {
	if err := checkSourceName(filename, line, core.SourceKindWebhook, name); err != nil {
		return err
	}
	if prev, dup := st.webhookAt[name]; dup {
		return newError(filename, line,
			"duplicate trigger source [webhook.%s] (already declared at %s:%d)", name, prev.file, prev.line)
	}

	verify := core.VerifyScheme(strings.TrimSpace(rw.Verify))
	switch {
	case rw.Verify == "":
		return newError(filename, line,
			"[webhook.%s]: missing required key \"verify\" (want one of: %s)", name, verifyList())
	case !verify.Valid():
		return newError(filename, line,
			"[webhook.%s]: unknown \"verify\" scheme %q (want one of: %s)", name, rw.Verify, verifyList())
	}

	// A preset fixes its own headers. Rejecting an override rather than
	// ignoring it is the difference between a config that fails the load and
	// one that reads as if it verified a header it never looked at.
	if verify.Preset() {
		for _, k := range []struct {
			key string
			set bool
		}{
			{"signature_header", rw.SignatureHeader != nil},
			{"signature_prefix", rw.SignaturePrefix != nil},
			{"event_header", rw.EventHeader != nil},
			{"delivery_header", rw.DeliveryHeader != nil},
		} {
			if !k.set {
				continue
			}
			if verify == core.VerifyStandardWebhooks && k.key == "event_header" {
				return newError(filename, line,
					"[webhook.%s]: %q is not accepted with verify = %q (the scheme reads its event name from the body, not a header)", name, k.key, verify)
			}
			return newError(filename, line,
				"[webhook.%s]: %q is not accepted with verify = %q (the preset fixes its own headers)", name, k.key, verify)
		}
	}

	src := core.WebhookSource{
		Name:        name,
		Verify:      verify,
		Enabled:     rw.Enabled == nil || *rw.Enabled,
		Description: rw.Description,
		MaxBody:     core.DefaultWebhookMaxBody,
		RateLimit:   core.DefaultWebhookRateLimit,
	}

	if sig, prefix, event, delivery, ok := core.PresetHeaders(verify); ok {
		// Fill the preset's headers in, so every consumer downstream reads
		// one set of fields and never re-derives them per scheme.
		src.SignatureHeader, src.SignaturePrefix = sig, prefix
		src.EventHeader, src.DeliveryHeader = event, delivery
	} else {
		if rw.SignatureHeader != nil {
			src.SignatureHeader = strings.TrimSpace(*rw.SignatureHeader)
		}
		if rw.SignaturePrefix != nil {
			// NOT trimmed: a prefix is matched against the header byte for
			// byte, and trimming one that legitimately ends in a space would
			// make every delivery fail verification for an invisible reason.
			src.SignaturePrefix = *rw.SignaturePrefix
		}
		if rw.EventHeader != nil {
			src.EventHeader = strings.TrimSpace(*rw.EventHeader)
		}
		if rw.DeliveryHeader != nil {
			src.DeliveryHeader = strings.TrimSpace(*rw.DeliveryHeader)
		}
		switch {
		case verify == core.VerifyHMACSHA256 && src.SignatureHeader == "":
			return newError(filename, line,
				"[webhook.%s]: verify = \"hmac-sha256\" requires \"signature_header\" (there is no header to read the signature from otherwise)", name)
		case verify == core.VerifyBearer && rw.SignatureHeader != nil:
			return newError(filename, line,
				"[webhook.%s]: \"signature_header\" is not accepted with verify = \"bearer\" (a bearer token is read from Authorization)", name)
		case verify == core.VerifyBearer && rw.SignaturePrefix != nil:
			return newError(filename, line,
				"[webhook.%s]: \"signature_prefix\" is not accepted with verify = \"bearer\" (it strips a prefix from a signature, and bearer has none)", name)
		}
	}

	for _, e := range rw.Events {
		if strings.TrimSpace(e) == "" {
			return newError(filename, line, "[webhook.%s]: \"events\" has a blank entry", name)
		}
		src.Events = append(src.Events, strings.TrimSpace(e))
	}
	// An allowlist needs something to match against. For the two
	// operator-configured schemes that is `event_header`; the presets that
	// carry an event name supply their own, and standard-webhooks reads it
	// from the body.
	if len(src.Events) > 0 && verify.ReadsEventFromHeader() && src.EventHeader == "" {
		return newError(filename, line,
			"[webhook.%s]: \"events\" requires \"event_header\" with verify = %q (there is no event name to match against otherwise)", name, verify)
	}

	if rw.MaxBody != nil {
		n, err := core.ParseByteSize(*rw.MaxBody)
		if err != nil {
			return newError(filename, line, "[webhook.%s]: invalid \"max_body\" %q: %v", name, *rw.MaxBody, err)
		}
		if n > core.MaxWebhookMaxBody {
			return newError(filename, line,
				"[webhook.%s]: \"max_body\" %q exceeds the %s ceiling (a verifier holds the whole body in memory to sign over it)",
				name, *rw.MaxBody, core.FormatByteSize(core.MaxWebhookMaxBody))
		}
		src.MaxBody = n
	}
	if rw.RateLimit != nil {
		rl, err := core.ParseRateLimit(*rw.RateLimit)
		if err != nil {
			return newError(filename, line, "[webhook.%s]: invalid \"rate_limit\" %q: %v", name, *rw.RateLimit, err)
		}
		src.RateLimit = rl
	}

	// The secret and its env_file are both required, and the secret must be
	// exactly one reference: harness.toml is routinely committed to a
	// dotfiles repository, so a literal is refused rather than warned about.
	envFile := strings.TrimSpace(rw.EnvFile)
	switch {
	case rw.EnvFile != "" && envFile == "":
		return newError(filename, line, "[webhook.%s]: \"env_file\" must not be blank", name)
	case envFile == "":
		return newError(filename, line,
			"[webhook.%s]: missing required key \"env_file\" (the file \"secret\" resolves from)", name)
	}
	src.EnvFile = resolveConfigPath(envFile, filename)

	secretRef := strings.TrimSpace(rw.Secret)
	switch {
	case rw.Secret != "" && secretRef == "":
		return newError(filename, line, "[webhook.%s]: \"secret\" must not be blank", name)
	case secretRef == "":
		return newError(filename, line,
			"[webhook.%s]: missing required key \"secret\" (want exactly one reference, e.g. secret = \"${GH_HOOK_SECRET}\")", name)
	case !isSoleRef(secretRef):
		// The message must not echo the value: the whole point of failing
		// here is that this string is a credential.
		return newSentinelError(filename, line, ErrLiteralSecret,
			"[webhook.%s]: \"secret\" must be exactly one reference to a name in env_file, e.g. secret = \"${GH_HOOK_SECRET}\" — harness.toml is routinely committed, so a literal value is refused", name)
	}
	res := newEnvResolver(src.EnvFile)
	secret, err := res.expand(secretRef)
	if err != nil {
		return newError(filename, line, "[webhook.%s]: \"secret\": %v", name, err)
	}
	src.Secret = secret
	if w := res.warning(); w != "" {
		cfg.Warnings = append(cfg.Warnings, w)
	}

	if cfg.Webhooks == nil {
		cfg.Webhooks = map[string]core.WebhookSource{}
	}
	cfg.Webhooks[name] = src
	cfg.WebhookOrder = append(cfg.WebhookOrder, name)
	st.webhookAt[name] = declSite{file: filename, line: line}
	return nil
}

// resolveTriggers checks every harness's `triggers` references against the
// finished config view. It runs after the last drop-in, because a harness in
// the main file may bind a source a drop-in declares and vice versa — the same
// reason profile membership is validated at the end of Parse.
// Governing: SPEC-0014 REQ "Triggers Key".
func resolveTriggers(cfg *core.Config, st *loadState) error {
	for _, name := range cfg.HarnessOrder {
		h := cfg.Harnesses[name]
		if len(h.Triggers) == 0 {
			continue
		}
		at := st.harnessAt[name]
		for _, t := range h.Triggers {
			ref, err := core.ParseTriggerRef(t)
			if err != nil {
				// Shape was already checked at registration; this is the
				// belt to that braces, and it keeps resolveTriggers correct
				// on its own terms rather than by assuming a caller.
				return newSentinelError(at.file, at.line, ErrUnknownSourceRef,
					"harness %q: invalid \"triggers\" entry %q: %v", name, t, err)
			}
			ok := false
			switch ref.Kind {
			case core.SourceKindChannel:
				_, ok = cfg.Channels[ref.Name]
			case core.SourceKindWebhook:
				_, ok = cfg.Webhooks[ref.Name]
			}
			if !ok {
				return newSentinelError(at.file, at.line, ErrUnknownSourceRef,
					"harness %q: \"triggers\" names %q, and no [%s.%s] table declares it%s",
					name, t, ref.Kind, ref.Name, nearestSourceHint(cfg, ref))
			}
		}
	}
	return nil
}

// nearestSourceHint appends ", did you mean …?" when the config declares a
// source of the OTHER kind under the same name. That is the mistake this error
// sees most: `triggers = ["webhook.sb"]` against a `[channel.sb]` table.
func nearestSourceHint(cfg *core.Config, ref core.TriggerRef) string {
	other := core.SourceKindWebhook
	found := false
	if ref.Kind == core.SourceKindWebhook {
		other = core.SourceKindChannel
		_, found = cfg.Channels[ref.Name]
	} else {
		_, found = cfg.Webhooks[ref.Name]
	}
	if !found {
		return ""
	}
	return fmt.Sprintf(" (a [%s.%s] table is declared — did you mean %s.%s?)", other, ref.Name, other, ref.Name)
}

// parseTriggers validates the SHAPE of a harness's `triggers` list: every
// entry is a well-formed `channel.<name>` / `webhook.<name>` reference and no
// entry repeats. Whether the named source exists is resolveTriggers' job.
// Governing: SPEC-0014 REQ "Triggers Key".
func parseTriggers(filename, name string, line int, raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, entry := range raw {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			return nil, newError(filename, line, "harness %q: \"triggers\" has a blank entry", name)
		}
		ref, err := core.ParseTriggerRef(trimmed)
		if err != nil {
			return nil, newSentinelError(filename, line, ErrUnknownSourceRef,
				"harness %q: invalid \"triggers\" entry %q: %v", name, entry, err)
		}
		canonical := ref.String()
		if seen[canonical] {
			return nil, newError(filename, line,
				"harness %q: \"triggers\" lists %q twice (a source fires a harness once per event, so the repeat does nothing)", name, canonical)
		}
		seen[canonical] = true
		out = append(out, canonical)
	}
	return out, nil
}

// buildWebhookServer folds the [server] webhook_* keys into sc. It is split
// out of buildServer only so the TLS-pair rule and its reasoning sit beside
// the rest of the trigger schema.
// Governing: SPEC-0014 REQ "Webhook Listener".
func buildWebhookServer(sc *core.ServerConfig, filename string, line int, rs rawServer) error {
	sc.WebhookListen = strings.TrimSpace(rs.WebhookListen)
	sc.WebhookTLSCertFile = expandHome(strings.TrimSpace(rs.WebhookTLSCertFile))
	sc.WebhookTLSKeyFile = expandHome(strings.TrimSpace(rs.WebhookTLSKeyFile))
	// Both or neither. A half-configured pair is overwhelmingly an operator
	// who believes the listener is encrypted, so it fails the load rather
	// than starting in cleartext with a warning nobody reads.
	switch {
	case sc.WebhookTLSCertFile != "" && sc.WebhookTLSKeyFile == "":
		return newError(filename, line,
			"[server]: \"webhook_tls_cert_file\" is set without \"webhook_tls_key_file\" (both are required to serve HTTPS; the listener would otherwise serve cleartext)")
	case sc.WebhookTLSKeyFile != "" && sc.WebhookTLSCertFile == "":
		return newError(filename, line,
			"[server]: \"webhook_tls_key_file\" is set without \"webhook_tls_cert_file\" (both are required to serve HTTPS; the listener would otherwise serve cleartext)")
	}
	return nil
}

// warnNonLoopbackWebhook appends the startup warning REQ "Webhook Listener"
// requires for a non-loopback bind with no TLS. It is a warning and not an
// error because the listener may legitimately sit behind a TLS-terminating
// proxy on the same host; `harness doctor` flags it either way.
// Governing: SPEC-0014 REQ "Webhook Listener".
func warnNonLoopbackWebhook(cfg *core.Config) {
	addr := cfg.Server.WebhookListen
	if addr == "" || cfg.Server.WebhookTLSCertFile != "" {
		return
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		// Not host:port — leave the complaint to whoever binds it.
		return
	}
	if host == "" {
		// A bare ":8080" binds every interface, which is the loudest case.
		host = "0.0.0.0"
	}
	if isLoopbackHost(host) {
		return
	}
	cfg.Warnings = append(cfg.Warnings,
		fmt.Sprintf("[server] webhook_listen %q is not a loopback address and no TLS is configured: deliveries and their signatures cross the network in cleartext", addr))
}

// isLoopbackHost reports whether host is localhost, an address in 127.0.0.0/8,
// or ::1 — the hosts REQ "Channel Source Table" lets `http` reach, and the
// binds REQ "Webhook Listener" does not warn about.
func isLoopbackHost(host string) bool {
	h := strings.ToLower(strings.Trim(strings.TrimSpace(host), "[]"))
	if h == "localhost" {
		return true
	}
	if ip := net.ParseIP(h); ip != nil {
		return ip.IsLoopback()
	}
	return false
}

// verifyList renders the accepted `verify` values for an error message.
func verifyList() string {
	parts := make([]string, len(core.VerifySchemes))
	for i, s := range core.VerifySchemes {
		parts[i] = string(s)
	}
	return strings.Join(parts, ", ")
}

// sortedKeys returns m's keys in sorted order, so an error raised while
// walking a TOML table names the same key on every run.
func sortedKeys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// envRefRe matches a ${NAME} reference. NAME is the env-file key grammar:
// letters, digits and underscores, not starting with a digit.
var envRefRe = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// soleRefRe matches a string that is exactly one reference and nothing else.
var soleRefRe = regexp.MustCompile(`^\$\{[A-Za-z_][A-Za-z0-9_]*\}$`)

// isSoleRef reports whether s consists of exactly one ${NAME} reference.
func isSoleRef(s string) bool { return soleRefRe.MatchString(s) }

// envResolver expands ${NAME} references from ONE file: the source's own
// `env_file`, never the daemon's environment.
//
// That restriction is the whole design. config.Load runs in the CLI and in the
// daemon, which systemd starts with a different environment; resolving from
// the process environment would make `harness triggers` and the daemon
// disagree about what a source is configured with, and the disagreement would
// surface as a 401 nobody could reproduce. Reading one declared file means
// both processes reach the same answer or fail the same way.
//
// The file is read lazily and at most once, so a source with no references
// never touches the disk and a missing file is only an error for a source that
// actually needed it.
// Governing: ADR-0008; SPEC-0014 REQ "Credential Resolution".
type envResolver struct {
	path   string
	loaded bool
	values map[string]string
	err    error
	// perm is the resolved file's mode, kept so the group/other-readable
	// warning can be raised once per source rather than per reference.
	lax bool
}

func newEnvResolver(path string) *envResolver { return &envResolver{path: path} }

// expand resolves every ${NAME} in raw and returns the result as a Secret.
// A value with no reference at all is returned verbatim (a non-secret header
// like `X-Client = "harness"` is legitimate); the caller decides whether a
// literal is acceptable for the key it is filling.
func (r *envResolver) expand(raw string) (core.Secret, error) {
	refs := envRefRe.FindAllStringSubmatch(raw, -1)
	if len(refs) == 0 {
		return core.Secret(raw), nil
	}
	if err := r.load(); err != nil {
		return "", err
	}
	var missing error
	out := envRefRe.ReplaceAllStringFunc(raw, func(m string) string {
		name := envRefRe.FindStringSubmatch(m)[1]
		v, ok := r.values[name]
		if !ok {
			if missing == nil {
				// Name the reference, never the file's contents.
				missing = fmt.Errorf("env_file %q does not define %s", r.path, name)
			}
			return ""
		}
		return v
	})
	if missing != nil {
		return "", missing
	}
	return core.Secret(out), nil
}

// load reads the env file once. Its format is the one `env_file` already uses
// on [harness.*] — KEY=VALUE, `#` comments, an optional `export ` prefix, and
// optional surrounding quotes — deliberately duplicated here rather than
// imported: internal/supervisor depends on internal/config, so sharing the
// reader the other way would be an import cycle. The two are pinned together
// by TestEnvFileFormatMatchesSupervisor.
func (r *envResolver) load() error {
	if r.loaded {
		return r.err
	}
	r.loaded = true
	if r.path == "" {
		r.err = fmt.Errorf("a ${NAME} reference requires \"env_file\" (references resolve only from the source's own env_file, never from the daemon's environment)")
		return r.err
	}
	info, err := os.Stat(r.path)
	switch {
	case err != nil && os.IsNotExist(err):
		r.err = fmt.Errorf("env_file %q does not exist", r.path)
		return r.err
	case err != nil:
		r.err = fmt.Errorf("env_file %q is not readable: %w", r.path, err)
		return r.err
	case info.IsDir():
		r.err = fmt.Errorf("env_file %q is a directory, not a file", r.path)
		return r.err
	}
	// 0o077 is group+other. A credential file anyone on the box can read is
	// a warning, not a failure: the operator may be on a single-user machine,
	// and refusing to start over it would be worse than saying so.
	r.lax = info.Mode().Perm()&0o077 != 0

	f, err := os.Open(r.path)
	if err != nil {
		r.err = fmt.Errorf("env_file %q is not readable: %w", r.path, err)
		return r.err
	}
	defer f.Close()

	r.values = map[string]string{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		eq := strings.IndexByte(line, '=')
		if eq <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:eq])
		val := unquoteEnvValue(strings.TrimSpace(line[eq+1:]))
		r.values[key] = val
	}
	if err := sc.Err(); err != nil {
		r.err = fmt.Errorf("env_file %q is not readable: %w", r.path, err)
		return r.err
	}
	return nil
}

// warning returns the group/other-readable finding for this resolver's file,
// or "" when there is nothing to say. It is only meaningful after the file was
// actually read, which is deliberate: a source with no references never needed
// the file, so it has no business complaining about its mode.
func (r *envResolver) warning() string {
	if !r.loaded || r.err != nil || !r.lax {
		return ""
	}
	return fmt.Sprintf("env_file %q is readable by group or other: a trigger source's credentials live in it (chmod 600)", r.path)
}

// unquoteEnvValue strips one layer of surrounding single or double quotes,
// matching supervisor.parseEnvFile's handling of the same format.
func unquoteEnvValue(v string) string {
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	return v
}

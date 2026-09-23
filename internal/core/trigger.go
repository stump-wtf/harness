// Package core's trigger source schema.
//
// SPEC-0014 adds two kinds of *trigger source* to the config: a `[channel.*]`
// table, which the daemon dials out to and listens on, and a `[webhook.*]`
// table, which it serves an authenticated route for. A `[harness.*]` binds
// sources with `triggers`, and a harness carrying `schedule`, `triggers` or
// both is a *triggered harness*. These are the parsed, validated records the
// runtime consumes; nothing here connects, listens or fires.
//
// Resolved credentials live on these records (REQ "Credential Resolution"
// requires them read at load and on every reload) but never leave them as
// plain text: they are typed Secret, whose every rendering is "***".
//
// Governing: ADR-0021 (on-demand one-shots); SPEC-0014 REQ "Channel Source
// Table", REQ "Webhook Source Table", REQ "Triggers Key", REQ "One Consumer
// Per Endpoint", REQ "Credential Resolution".
//
// @joestump 09/22/2026 - Introduced with the SPEC-0014 config schema (#454).
package core

import (
	"fmt"
	"math"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SourceNameRe is the grammar every `[channel.<name>]` and `[webhook.<name>]`
// must match. The name appears in `triggers` references, in protocol replies
// and in metric labels, so it is deliberately narrower than TOML's own key
// grammar. Governing: SPEC-0014 REQ "Channel Source Table".
var SourceNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// Secret is a resolved credential — a `[webhook.*] secret` or a
// `[channel.*] headers` value. Its type is the enforcement: String, GoString,
// MarshalJSON and MarshalText all render "***", so a Secret that reaches a log
// line, a protocol frame, a `describe` projection or a %v of its enclosing
// struct prints the mask rather than the value. Reveal is the only way out,
// and it is deliberately ugly to type at a call site that should not have it.
//
// This is not defence in depth for its own sake: SPEC-0014 REQ "Credential
// Resolution" forbids these values reaching state.json, run records, logs,
// protocol frames or `describe`, and every one of those surfaces formats
// structs generically. A string field would be one `%+v` away from leaking.
// Governing: ADR-0008 (secrets); SPEC-0014 REQ "Credential Resolution".
type Secret string

// redacted is what every rendering of a Secret produces.
const redacted = "***"

// String implements fmt.Stringer, so `%v`/`%s` of a Secret — or of a struct
// holding one, since fmt calls Stringer on nested fields — prints the mask.
func (s Secret) String() string { return redacted }

// GoString implements fmt.GoStringer, so `%#v` is masked too.
func (s Secret) GoString() string { return strconv.Quote(redacted) }

// MarshalJSON masks the value for encoding/json, which every protocol frame
// and state file goes through.
func (s Secret) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(redacted)), nil }

// MarshalText masks the value for encoders that prefer TextMarshaler (TOML,
// YAML, and encoding/json for map keys).
func (s Secret) MarshalText() ([]byte, error) { return []byte(redacted), nil }

// Reveal returns the actual credential. Call it only where the value is about
// to be used — an outbound header, an HMAC computation — never to store,
// format or log it.
func (s Secret) Reveal() string { return string(s) }

// Empty reports whether the secret holds no value, without revealing it.
func (s Secret) Empty() bool { return string(s) == "" }

// VerifyScheme is how a `[webhook.*]` route authenticates a delivery
// (SPEC-0014 REQ "Webhook Verification"). Four of the six are *presets*: they
// fix their own header names, so setting a header key alongside one is a parse
// error rather than a silently ignored override.
type VerifyScheme string

const (
	// VerifyBearer compares a bearer token in Authorization.
	VerifyBearer VerifyScheme = "bearer"
	// VerifyHMACSHA256 verifies an HMAC-SHA256 over the raw body, read from
	// the operator-named signature_header.
	VerifyHMACSHA256 VerifyScheme = "hmac-sha256"
	// VerifyGitHub is the GitHub preset: X-Hub-Signature-256 over the body,
	// with X-GitHub-Event and X-GitHub-Delivery.
	VerifyGitHub VerifyScheme = "github"
	// VerifyGitea is the Gitea preset: X-Gitea-Signature over the body, with
	// X-Gitea-Event and X-Gitea-Delivery.
	VerifyGitea VerifyScheme = "gitea"
	// VerifyGitLab is the GitLab preset: the X-Gitlab-Token shared secret,
	// with X-Gitlab-Event and X-Gitlab-Event-UUID.
	VerifyGitLab VerifyScheme = "gitlab"
	// VerifyStandardWebhooks is the Standard Webhooks preset
	// (https://www.standardwebhooks.com): webhook-signature over
	// webhook-id, webhook-timestamp and the body. Switchboard's outbound
	// notify hooks speak it. Its event name comes from the body, not a
	// header, which is why event_header is rejected alongside it.
	VerifyStandardWebhooks VerifyScheme = "standard-webhooks"
)

// VerifySchemes is every accepted value, in the order error messages list
// them. Governing: SPEC-0014 REQ "Webhook Source Table".
var VerifySchemes = []VerifyScheme{
	VerifyBearer, VerifyHMACSHA256, VerifyGitHub, VerifyGitea, VerifyGitLab, VerifyStandardWebhooks,
}

// Valid reports whether v is a known scheme. The empty string is NOT valid:
// `verify` is required, and an omitted key must fail the load rather than
// default into whichever scheme happened to be first.
func (v VerifyScheme) Valid() bool {
	for _, s := range VerifySchemes {
		if v == s {
			return true
		}
	}
	return false
}

// Preset reports whether v fixes its own header names. A preset rejects
// signature_header, signature_prefix, event_header and delivery_header,
// because accepting them would let a config *look* like it overrode a header
// the verifier then ignored. Governing: SPEC-0014 REQ "Webhook Source Table".
func (v VerifyScheme) Preset() bool {
	switch v {
	case VerifyGitHub, VerifyGitea, VerifyGitLab, VerifyStandardWebhooks:
		return true
	}
	return false
}

// ReadsEventFromHeader reports whether v names its event type in a header, and
// so can carry an `events` allowlist matched against one. Only the two
// operator-configured schemes do; each preset either fixes the header itself
// or (standard-webhooks) reads the event name out of the body.
func (v VerifyScheme) ReadsEventFromHeader() bool {
	return v == VerifyBearer || v == VerifyHMACSHA256
}

// RateLimit is a `[webhook.*] rate_limit` — at most Count deliveries per
// Window. A zero Count means no limit, which is what the literal `"0"`
// parses to. Governing: SPEC-0014 REQ "Webhook Rate Limit".
type RateLimit struct {
	// Count is the number of deliveries allowed per Window. 0 means no limit.
	Count int
	// Window is the period Count applies over. Zero when Count is 0.
	Window time.Duration
}

// DefaultWebhookRateLimit is the `rate_limit` an omitted key resolves to.
var DefaultWebhookRateLimit = RateLimit{Count: 60, Window: time.Minute}

// Unlimited reports whether r imposes no limit.
func (r RateLimit) Unlimited() bool { return r.Count <= 0 }

// String renders r back into the config grammar, so a round-trip through a
// config writer produces the text the operator wrote.
func (r RateLimit) String() string {
	if r.Unlimited() {
		return "0"
	}
	unit := ""
	switch r.Window {
	case time.Second:
		unit = "s"
	case time.Minute:
		unit = "m"
	case time.Hour:
		unit = "h"
	default:
		// Unreachable via ParseRateLimit; render something honest rather
		// than a grammar-shaped lie if a caller built one by hand.
		return strconv.Itoa(r.Count) + "/" + r.Window.String()
	}
	return strconv.Itoa(r.Count) + "/" + unit
}

// ParseRateLimit parses `<n>/<s|m|h>`, or the literal "0" for no limit.
// Governing: SPEC-0014 REQ "Webhook Rate Limit".
func ParseRateLimit(s string) (RateLimit, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "0" {
		return RateLimit{}, nil
	}
	n, unit, ok := strings.Cut(trimmed, "/")
	if !ok {
		return RateLimit{}, fmt.Errorf("want \"<n>/<s|m|h>\" such as \"60/m\", or \"0\" for no limit")
	}
	count, err := strconv.Atoi(strings.TrimSpace(n))
	if err != nil || count < 1 {
		return RateLimit{}, fmt.Errorf("the count must be a positive integer")
	}
	var window time.Duration
	switch strings.TrimSpace(unit) {
	case "s":
		window = time.Second
	case "m":
		window = time.Minute
	case "h":
		window = time.Hour
	default:
		return RateLimit{}, fmt.Errorf("unknown period %q (want \"s\", \"m\" or \"h\")", unit)
	}
	return RateLimit{Count: count, Window: window}, nil
}

const (
	// DefaultWebhookMaxBody is the `max_body` an omitted key resolves to.
	DefaultWebhookMaxBody int64 = 1 << 20 // 1MiB
	// MaxWebhookMaxBody is the ceiling `max_body` may name. A verifier holds
	// the whole body in memory to compute a signature over it, so an
	// operator cannot be allowed to turn one route into an OOM.
	MaxWebhookMaxBody int64 = 25 << 20 // 25MiB
)

// byteSizeRe matches the `max_body` grammar: an integer and a B/KiB/MiB
// suffix. Binary suffixes only — "MB" is deliberately not accepted, because a
// config that says MB and means MiB is a config whose author is guessing.
var byteSizeRe = regexp.MustCompile(`^([0-9]+)\s*(B|KiB|MiB)$`)

// ParseByteSize parses a `max_body` value into bytes.
// Governing: SPEC-0014 REQ "Webhook Source Table".
func ParseByteSize(s string) (int64, error) {
	m := byteSizeRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, fmt.Errorf("want an integer with a \"B\", \"KiB\" or \"MiB\" suffix, such as \"1MiB\"")
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("the size is too large")
	}
	var mult int64
	switch m[2] {
	case "B":
		mult = 1
	case "KiB":
		mult = 1 << 10
	case "MiB":
		mult = 1 << 20
	}
	if n < 1 {
		return 0, fmt.Errorf("the size must be at least 1 byte")
	}
	// Overflow only. The webhook ceiling is deliberately NOT enforced here:
	// this is a generic size parser, and the caller's check produces the
	// better message (it can name the ceiling and say why it exists). A
	// ceiling check here would pre-empt it with "too large" and tell the
	// operator nothing.
	if n > math.MaxInt64/mult {
		return 0, fmt.Errorf("the size is too large")
	}
	return n * mult, nil
}

// FormatByteSize renders n back into the `max_body` grammar, choosing the
// largest suffix that divides it exactly so a round-trip through a config
// writer reproduces what the operator wrote.
func FormatByteSize(n int64) string {
	switch {
	case n >= 1<<20 && n%(1<<20) == 0:
		return strconv.FormatInt(n/(1<<20), 10) + "MiB"
	case n >= 1<<10 && n%(1<<10) == 0:
		return strconv.FormatInt(n/(1<<10), 10) + "KiB"
	default:
		return strconv.FormatInt(n, 10) + "B"
	}
}

// ChannelSource is one `[channel.<name>]` table: an MCP Streamable HTTP
// endpoint the daemon holds a listen-only session to. Each
// `notifications/claude/channel` it receives fires every harness that binds
// it. Governing: SPEC-0014 REQ "Channel Source Table".
type ChannelSource struct {
	// Name is the table name, unique across the main config and its
	// drop-ins.
	Name string
	// URL is the server's MCP Streamable HTTP endpoint, verbatim as written.
	// It is https unless its host is loopback, and carries no userinfo.
	URL string
	// Headers are the request headers sent on every request of the session,
	// with `${NAME}` references already expanded from EnvFile. Values are
	// Secret because that is overwhelmingly what they are: the Authorization
	// a vended endpoint is reached with.
	Headers map[string]Secret
	// EnvFile is the resolved path `${NAME}` references in Headers were read
	// from. Empty when no reference was used.
	EnvFile string
	// Enabled is false only for a table that sets `enabled = false`: the
	// source stays declared and bindable, and the daemon never connects it.
	Enabled bool
	// Description is operator prose.
	Description string
}

// HeaderNames returns c's header names sorted, so every caller that renders
// or fingerprints them agrees on an order. Names only — REQ "Credential
// Resolution" permits showing these and forbids showing their values.
func (c ChannelSource) HeaderNames() []string {
	out := make([]string, 0, len(c.Headers))
	for k := range c.Headers {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// WebhookSource is one `[webhook.<name>]` table: an authenticated route,
// `POST /hooks/<name>`, on the opt-in HTTP listener. Each verified delivery
// that passes the source's filters fires every harness that binds it.
// Governing: SPEC-0014 REQ "Webhook Source Table".
type WebhookSource struct {
	// Name is the table name, unique across the main config and its
	// drop-ins, and the last path segment of the route.
	Name string
	// Verify is how a delivery is authenticated. Required.
	Verify VerifyScheme
	// Secret is the resolved credential the scheme verifies with.
	Secret Secret
	// EnvFile is the resolved path Secret was read from. Always set: a
	// webhook source must name one, because a literal secret is rejected.
	EnvFile string
	// Events is an optional allowlist matched against the delivery's event
	// name. Empty means every event passes.
	Events []string
	// SignatureHeader carries the signature. Operator-set for hmac-sha256;
	// filled in from the preset for github/gitea/gitlab/standard-webhooks.
	SignatureHeader string
	// SignaturePrefix is stripped from the signature before decoding, e.g.
	// "sha256=". hmac-sha256 only, and empty by default.
	SignaturePrefix string
	// EventHeader names the event type. Operator-set for bearer/hmac-sha256;
	// filled in from the preset where the preset defines one. Empty for
	// standard-webhooks, which reads its event name from the body.
	EventHeader string
	// DeliveryHeader carries a unique delivery ID, used for replay
	// de-duplication. Operator-set for bearer/hmac-sha256; filled in from
	// the preset where the preset defines one.
	DeliveryHeader string
	// MaxBody is the largest accepted body in bytes, at most
	// MaxWebhookMaxBody.
	MaxBody int64
	// RateLimit bounds deliveries to this route.
	RateLimit RateLimit
	// Enabled is false only for a table that sets `enabled = false`: the
	// source stays declared, and its route answers 404 exactly as an
	// undeclared name does.
	Enabled bool
	// Description is operator prose.
	Description string
}

// presetHeaders is the header set each preset scheme fixes. A preset's entry
// is authoritative: the parser rejects an operator trying to set any of these
// keys, then fills them in from here, so the verifier and the config can never
// disagree about which header it reads.
// Governing: SPEC-0014 REQ "Webhook Source Table", REQ "Webhook Verification".
var presetHeaders = map[VerifyScheme]struct {
	Signature string
	Prefix    string
	Event     string
	Delivery  string
}{
	VerifyGitHub:           {Signature: "X-Hub-Signature-256", Prefix: "sha256=", Event: "X-GitHub-Event", Delivery: "X-GitHub-Delivery"},
	VerifyGitea:            {Signature: "X-Gitea-Signature", Event: "X-Gitea-Event", Delivery: "X-Gitea-Delivery"},
	VerifyGitLab:           {Signature: "X-Gitlab-Token", Event: "X-Gitlab-Event", Delivery: "X-Gitlab-Event-UUID"},
	VerifyStandardWebhooks: {Signature: "webhook-signature", Delivery: "webhook-id"},
}

// PresetHeaders returns the headers scheme v fixes, and whether v is a preset
// at all. The parser fills a preset source's header fields from this, so the
// rest of the daemon reads one set of fields regardless of scheme.
func PresetHeaders(v VerifyScheme) (signature, prefix, event, delivery string, ok bool) {
	p, ok := presetHeaders[v]
	if !ok {
		return "", "", "", "", false
	}
	return p.Signature, p.Prefix, p.Event, p.Delivery, true
}

// NormalizeEndpoint canonicalizes a channel `url` for the one-consumer-per-
// endpoint check: it lowercases the scheme and host, elides the scheme's
// default port, and drops a trailing "/" from the path. It is comparison-only
// — ChannelSource.URL keeps what the operator wrote — so a normalization the
// operator disagrees with can never change what the daemon dials.
// Governing: SPEC-0014 REQ "One Consumer Per Endpoint".
func NormalizeEndpoint(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return strings.TrimSpace(raw)
	}
	u.Scheme = strings.ToLower(u.Scheme)
	host := strings.ToLower(u.Hostname())
	port := u.Port()
	if (u.Scheme == "https" && port == "443") || (u.Scheme == "http" && port == "80") {
		port = ""
	}
	u.Host = host
	if port != "" {
		u.Host = host + ":" + port
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u.String()
}

// TriggerRef is a parsed `triggers` entry: a source kind and name, e.g.
// `channel.sb`. Governing: SPEC-0014 REQ "Triggers Key".
type TriggerRef struct {
	// Kind is "channel" or "webhook".
	Kind string
	// Name is the source's table name.
	Name string
}

// String renders the reference back into config form.
func (r TriggerRef) String() string { return r.Kind + "." + r.Name }

// Trigger source kinds, as they are spelled in a `triggers` reference and in
// the table header that declares them.
const (
	// SourceKindChannel is a `[channel.*]` table.
	SourceKindChannel = "channel"
	// SourceKindWebhook is a `[webhook.*]` table.
	SourceKindWebhook = "webhook"
)

// ParseTriggerRef splits a `triggers` entry into its kind and name. It
// validates the SHAPE only: whether the named source exists is the parser's
// job, because that answer depends on the whole config view including
// drop-ins.
func ParseTriggerRef(s string) (TriggerRef, error) {
	kind, name, ok := strings.Cut(strings.TrimSpace(s), ".")
	if !ok || (kind != SourceKindChannel && kind != SourceKindWebhook) {
		return TriggerRef{}, fmt.Errorf("want \"channel.<name>\" or \"webhook.<name>\"")
	}
	if !SourceNameRe.MatchString(name) {
		return TriggerRef{}, fmt.Errorf("%q is not a valid source name", name)
	}
	return TriggerRef{Kind: kind, Name: name}, nil
}

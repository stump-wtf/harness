// Package trigger holds the event envelope and, in later stories, the source
// manager that fans a firing out to the harnesses bound to it.
//
// The envelope is the contract between a trigger source and an agent run.
// SPEC-0014 is emphatic about what it is NOT: event text never becomes prompt
// text, argv, a working directory, or any other environment variable. It
// reaches the run as one JSON object in a `0600` file whose absolute path is
// named by `HARNESS_EVENT_FILE`, and the agent decides what to do with it.
//
// That is the whole security posture of triggered runs. A webhook body is
// attacker-controlled by construction — anyone who can open a pull request can
// put words in it — so the only safe place for it is a file the agent reads
// knowing it is untrusted. Interpolating it anywhere would make a delivery
// able to rewrite the instruction it fired.
//
// `version` is `1` and stays `1`: ADR-0023's command kind consumes these
// fields, so the shape is a contract the moment it ships.
//
// Governing: ADR-0021 (on-demand one-shots); SPEC-0014 REQ "Event Delivery To
// The Run", REQ "Manual Trigger With Event"; ADR-0008 (secrets never reach a
// record, a log or a frame).
//
// @joestump 09/22/2026 - Introduced with SPEC-0014 event delivery (#456).
package trigger

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"mime"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/stump-wtf/harness/internal/core"
)

// EnvelopeVersion is the envelope schema version. ADR-0023's command kind
// treats these fields as a contract, so a change here is a change to another
// record's inputs, not an internal detail.
const EnvelopeVersion = 1

// DefaultMaxEventBytes bounds a replayed envelope for a harness that binds no
// webhook source. A channel notification is already capped at 64 KiB by REQ
// "Channel Notification Handling", so this only ever applies to a hand-written
// or hand-edited file.
const DefaultMaxEventBytes int64 = 1 << 20 // 1MiB

// Kind is which sort of source produced an event.
type Kind string

const (
	// KindChannel is a `notifications/claude/channel` from a channel source.
	KindChannel Kind = Kind(core.SourceKindChannel)
	// KindWebhook is a verified delivery to a webhook source's route.
	KindWebhook Kind = Kind(core.SourceKindWebhook)
)

// Envelope is what a run's event file holds: one JSON object describing the
// event that fired it.
type Envelope struct {
	// Version is EnvelopeVersion.
	Version int `json:"version"`
	// Kind is KindChannel or KindWebhook, and decides which of Channel and
	// Webhook is populated.
	Kind Kind `json:"kind"`
	// Source is the source reference, e.g. "channel.sb".
	Source string `json:"source"`
	// EventID is the delivery ID from the scheme's delivery header, or a
	// daemon-generated unique ID when the scheme has none.
	EventID string `json:"event_id"`
	// ReceivedAt is when the daemon received the event, RFC 3339 UTC.
	ReceivedAt time.Time `json:"received_at"`
	// ReplayedAt is set only on a manual replay (`harness trigger --event`),
	// so a replayed file is distinguishable from the original it was copied
	// from. Everything else about the two is identical.
	ReplayedAt *time.Time `json:"replayed_at,omitempty"`
	// Channel carries a channel notification; nil for a webhook.
	Channel *ChannelEvent `json:"channel,omitempty"`
	// Webhook carries a webhook delivery; nil for a channel notification.
	Webhook *WebhookEvent `json:"webhook,omitempty"`
}

// ChannelEvent is a `notifications/claude/channel` as received. Both fields
// are passed through verbatim: the payload is opaque to Harness, which knows
// only that it arrived.
type ChannelEvent struct {
	Content string            `json:"content"`
	Meta    map[string]string `json:"meta,omitempty"`
}

// WebhookEvent is a verified delivery. Exactly one of Body, BodyText and
// BodyBase64 is set (Encoding decides which).
type WebhookEvent struct {
	// Event is the delivery's event name, from the scheme's event header or,
	// for standard-webhooks, from the body. Empty when the scheme has none.
	Event string `json:"event,omitempty"`
	// Delivery is the raw delivery ID the sender supplied, when it did.
	Delivery string `json:"delivery,omitempty"`
	// ContentType is the delivery's Content-Type, verbatim.
	ContentType string `json:"content_type,omitempty"`
	// Headers is the allowlisted subset of the request's headers
	// (AllowedHeaders). It never carries an Authorization, a signature or a
	// Cookie.
	Headers map[string]string `json:"headers,omitempty"`
	// Body is the parsed body, present only when the content type is JSON
	// and the body parses. An agent reading the file gets structure rather
	// than a string it has to unquote.
	Body json.RawMessage `json:"body,omitempty"`
	// BodyText is the body as text, for a body that is valid UTF-8 but not
	// JSON (a form post, XML, plain text).
	BodyText string `json:"body_text,omitempty"`
	// BodyBase64 is the body base64-encoded, for a body that is not valid
	// UTF-8. JSON cannot carry arbitrary bytes, and dropping them would make
	// the event file a lie about what arrived.
	BodyBase64 string `json:"body_base64,omitempty"`
}

// SetBody stores body under REQ "Event Delivery To The Run"'s three-way rule:
// `body` when the content type is JSON and the body parses, `body_text` when
// it is valid UTF-8, and `body_base64` otherwise.
//
// The content type gate matters as much as the parse. A `text/plain` body that
// happens to read as JSON (`42`, or `"hello"`) is text the sender called text,
// and promoting it to a JSON value would change what the agent sees based on
// an accident of its contents.
func (w *WebhookEvent) SetBody(contentType string, body []byte) {
	w.Body, w.BodyText, w.BodyBase64 = nil, "", ""
	if isJSONContentType(contentType) && json.Valid(body) {
		w.Body = json.RawMessage(append([]byte(nil), body...))
		return
	}
	if utf8.Valid(body) {
		w.BodyText = string(body)
		return
	}
	w.BodyBase64 = base64.StdEncoding.EncodeToString(body)
}

// isJSONContentType reports whether ct names a JSON media type, including the
// `+json` structured suffix (`application/vnd.github+json`, which is what a
// GitHub delivery actually sends). Parameters are ignored, so
// `application/json; charset=utf-8` counts.
func isJSONContentType(ct string) bool {
	mt, _, err := mime.ParseMediaType(strings.TrimSpace(ct))
	if err != nil {
		// Not parseable as a media type; fall back to a prefix match so a
		// sloppy header does not cost the agent its structure.
		mt = strings.ToLower(strings.TrimSpace(strings.SplitN(ct, ";", 2)[0]))
	}
	return mt == "application/json" || mt == "text/json" || strings.HasSuffix(mt, "+json")
}

// AllowedHeaders returns the subset of a delivery's headers the envelope may
// carry, per REQ "Event Delivery To The Run": `Content-Type`, `User-Agent`,
// and the scheme's event and delivery headers — plus `webhook-timestamp` for
// standard-webhooks, whose timestamp is part of what an agent may want to see.
//
// It is an ALLOWLIST rather than a denylist of the obvious secrets, and that
// is the point. A denylist has to enumerate every header a sender might use to
// carry a credential, and gets it wrong the first time a new scheme appears;
// an allowlist is wrong only in the direction of carrying too little.
// `Authorization`, the signature or token header, and `Cookie` are therefore
// excluded by construction rather than by a rule that names them — and a
// defensive check below still drops them if a caller ever passes a source
// whose configured event or delivery header IS one of those names.
//
// Lookup is case-insensitive, because HTTP header names are.
// Governing: SPEC-0014 REQ "Event Delivery To The Run"; ADR-0008.
func AllowedHeaders(src core.WebhookSource, get func(name string) string) map[string]string {
	want := []string{"Content-Type", "User-Agent"}
	if src.EventHeader != "" {
		want = append(want, src.EventHeader)
	}
	if src.DeliveryHeader != "" {
		want = append(want, src.DeliveryHeader)
	}
	if src.Verify == core.VerifyStandardWebhooks {
		want = append(want, StandardWebhooksTimestampHeader)
	}
	out := map[string]string{}
	for _, name := range want {
		if isNeverCarried(name) || (src.SignatureHeader != "" && strings.EqualFold(name, src.SignatureHeader)) {
			continue
		}
		if v := get(name); v != "" {
			out[name] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// StandardWebhooksTimestampHeader is the timestamp half of a Standard Webhooks
// signature. It is carried in the envelope because it describes the delivery;
// `webhook-signature` never is, because it authenticates it.
const StandardWebhooksTimestampHeader = "webhook-timestamp"

// neverCarried are headers that must not reach an event file whatever a source
// names them, because each one is or can be a credential.
var neverCarried = []string{"authorization", "cookie", "set-cookie", "proxy-authorization", "webhook-signature"}

func isNeverCarried(name string) bool {
	lower := strings.ToLower(strings.TrimSpace(name))
	for _, n := range neverCarried {
		if lower == n {
			return true
		}
	}
	return false
}

// Encode renders e as the bytes an event file holds: indented, so an operator
// who opens one can read it, and newline-terminated.
func (e *Envelope) Encode() ([]byte, error) {
	b, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("trigger: encode event: %w", err)
	}
	return append(b, '\n'), nil
}

// Validate checks an envelope's shape. It is what stands between a
// hand-supplied `--event` file and the run machinery, so it is strict about
// the fields the daemon will act on and silent about the payload, which is
// opaque by design.
func (e *Envelope) Validate() error {
	if e == nil {
		return fmt.Errorf("event is empty")
	}
	if e.Version != EnvelopeVersion {
		return fmt.Errorf("unsupported event version %d (want %d)", e.Version, EnvelopeVersion)
	}
	switch e.Kind {
	case KindChannel:
		if e.Channel == nil {
			return fmt.Errorf(`kind is "channel" but the envelope carries no "channel" object`)
		}
		if e.Webhook != nil {
			return fmt.Errorf(`kind is "channel" but the envelope also carries a "webhook" object`)
		}
	case KindWebhook:
		if e.Webhook == nil {
			return fmt.Errorf(`kind is "webhook" but the envelope carries no "webhook" object`)
		}
		if e.Channel != nil {
			return fmt.Errorf(`kind is "webhook" but the envelope also carries a "channel" object`)
		}
	default:
		return fmt.Errorf("unknown event kind %q (want %q or %q)", e.Kind, KindChannel, KindWebhook)
	}
	ref, err := core.ParseTriggerRef(e.Source)
	if err != nil {
		return fmt.Errorf("invalid source %q: %v", e.Source, err)
	}
	// The kind and the source have to agree, or a channel envelope could name
	// a webhook source and be recorded against it.
	if string(e.Kind) != ref.Kind {
		return fmt.Errorf("kind %q does not match source %q", e.Kind, e.Source)
	}
	if strings.TrimSpace(e.EventID) == "" {
		return fmt.Errorf("missing event_id")
	}
	if e.ReceivedAt.IsZero() {
		return fmt.Errorf("missing received_at")
	}
	if e.Webhook != nil {
		set := 0
		for _, b := range []bool{len(e.Webhook.Body) > 0, e.Webhook.BodyText != "", e.Webhook.BodyBase64 != ""} {
			if b {
				set++
			}
		}
		if set > 1 {
			return fmt.Errorf("the webhook carries more than one of body, body_text and body_base64")
		}
		if e.Webhook.BodyBase64 != "" {
			if _, err := base64.StdEncoding.DecodeString(e.Webhook.BodyBase64); err != nil {
				return fmt.Errorf("body_base64 is not valid base64")
			}
		}
		for name := range e.Webhook.Headers {
			// A hand-edited file must not be able to smuggle a credential
			// into a run's environment-adjacent state by naming it a header.
			if isNeverCarried(name) {
				return fmt.Errorf("the webhook carries the %s header, which an event file never may", name)
			}
		}
	}
	return nil
}

// ParseEnvelope decodes and validates an event file's bytes, refusing anything
// over maxBytes. maxBytes of 0 or less means DefaultMaxEventBytes.
//
// The size check runs BEFORE the decode, not after: the point of a cap is to
// avoid holding an unbounded document in memory, and a check on the decoded
// value has already paid that cost.
func ParseEnvelope(b []byte, maxBytes int64) (*Envelope, error) {
	if maxBytes <= 0 {
		maxBytes = DefaultMaxEventBytes
	}
	if int64(len(b)) > maxBytes {
		return nil, fmt.Errorf("event is %d bytes, over the %d-byte limit for this harness", len(b), maxBytes)
	}
	var e Envelope
	if err := json.Unmarshal(b, &e); err != nil {
		return nil, fmt.Errorf("event is not valid JSON: %w", err)
	}
	if err := e.Validate(); err != nil {
		return nil, err
	}
	return &e, nil
}

// Replay returns a copy of e stamped with a replay time, which is the only
// difference between a replayed run's event file and the original's.
func (e *Envelope) Replay(at time.Time) *Envelope {
	out := *e
	t := at.UTC()
	out.ReplayedAt = &t
	return &out
}

// MaxEventBytes is the size cap for a manually supplied envelope: the largest
// `max_body` among the harness's webhook sources, or DefaultMaxEventBytes when
// it binds none.
//
// Deriving it from the harness's own sources rather than using one global cap
// is what keeps a replay honest: an operator replaying a delivery that the
// listener accepted must not be refused for being too large, and one replaying
// something far larger than any of this harness's routes would take is
// supplying a file that could never have come from them.
// Governing: SPEC-0014 REQ "Manual Trigger With Event".
func MaxEventBytes(h core.Harness, cfg *core.Config) int64 {
	max := int64(0)
	for _, ref := range h.Triggers {
		parsed, err := core.ParseTriggerRef(ref)
		if err != nil || parsed.Kind != core.SourceKindWebhook {
			continue
		}
		if src, ok := cfg.Webhooks[parsed.Name]; ok && src.MaxBody > max {
			max = src.MaxBody
		}
	}
	if max <= 0 {
		return DefaultMaxEventBytes
	}
	return max
}

// Binds reports whether h's `triggers` lists ref.
func Binds(h core.Harness, ref string) bool {
	for _, t := range h.Triggers {
		if t == ref {
			return true
		}
	}
	return false
}

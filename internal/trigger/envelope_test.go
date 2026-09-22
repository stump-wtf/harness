package trigger

// Tests for the event envelope: the body three-way rule, the header allowlist,
// validation, and the per-harness size cap.
//
// The allowlist tests carry the most weight. REQ "Event Delivery To The Run"
// is the boundary between an attacker-supplied HTTP request and a file an
// agent reads, and the failure mode — a signature or bearer token landing in
// a run's event file — is silent, durable, and only visible to whoever later
// reads the file.
//
// Governing: ADR-0021; SPEC-0014 REQ "Event Delivery To The Run", REQ "Manual
// Trigger With Event"; ADR-0008.
//
// @joestump 09/22/2026 - Introduced with SPEC-0014 event delivery (#456).

import (
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

func channelEnvelope() *Envelope {
	return &Envelope{
		Version:    EnvelopeVersion,
		Kind:       KindChannel,
		Source:     "channel.sb",
		EventID:    "ev-1",
		ReceivedAt: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC),
		Channel:    &ChannelEvent{Content: "PR #9 opened", Meta: map[string]string{"todo_id": "t1", "queue": "reviews"}},
	}
}

func webhookEnvelope() *Envelope {
	e := &Envelope{
		Version:    EnvelopeVersion,
		Kind:       KindWebhook,
		Source:     "webhook.gitea-pr",
		EventID:    "abc",
		ReceivedAt: time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC),
		Webhook:    &WebhookEvent{Event: "pull_request", Delivery: "abc", ContentType: "application/json"},
	}
	e.Webhook.SetBody("application/json", []byte(`{"action":"opened"}`))
	return e
}

func TestEnvelopeRoundTrips(t *testing.T) {
	for _, e := range []*Envelope{channelEnvelope(), webhookEnvelope()} {
		b, err := e.Encode()
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if !strings.HasSuffix(string(b), "\n") {
			t.Error("an event file should end in a newline")
		}
		got, err := ParseEnvelope(b, 0)
		if err != nil {
			t.Fatalf("ParseEnvelope: %v", err)
		}
		if got.Source != e.Source || got.EventID != e.EventID || got.Kind != e.Kind {
			t.Errorf("round trip lost identity: %+v", got)
		}
		if !got.ReceivedAt.Equal(e.ReceivedAt) {
			t.Errorf("received_at = %v, want %v", got.ReceivedAt, e.ReceivedAt)
		}
	}
	// The channel payload is passed through verbatim: Harness treats it as
	// opaque, so a round trip that "cleaned" it would be changing data the
	// agent is meant to read exactly as the server sent it.
	b, _ := channelEnvelope().Encode()
	got, err := ParseEnvelope(b, 0)
	if err != nil {
		t.Fatal(err)
	}
	if got.Channel.Content != "PR #9 opened" || got.Channel.Meta["todo_id"] != "t1" {
		t.Errorf("channel payload = %+v", got.Channel)
	}
}

// TestSetBodyThreeWayRule covers the `body` / `body_text` / `body_base64`
// split. The content-type gate is tested as carefully as the parse: a
// text/plain body that happens to read as JSON is text the sender called text.
func TestSetBodyThreeWayRule(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		body        string
		wantField   string
	}{
		{"json body", "application/json", `{"a":1}`, "body"},
		{"json with charset", "application/json; charset=utf-8", `{"a":1}`, "body"},
		{"github's vendor json", "application/vnd.github+json", `{"a":1}`, "body"},
		{"json content type, unparseable body", "application/json", `{not json`, "body_text"},
		{"form post", "application/x-www-form-urlencoded", "a=1&b=2", "body_text"},
		{"plain text that looks like json", "text/plain", `{"a":1}`, "body_text"},
		{"xml", "application/xml", "<a/>", "body_text"},
		{"empty body", "application/json", "", "body_text"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := &WebhookEvent{}
			w.SetBody(tc.contentType, []byte(tc.body))
			got := map[string]bool{
				"body":        len(w.Body) > 0,
				"body_text":   w.BodyText != "",
				"body_base64": w.BodyBase64 != "",
			}
			// An empty body sets nothing at all, which is its own answer.
			if tc.body == "" {
				for k, v := range got {
					if v {
						t.Errorf("an empty body set %s", k)
					}
				}
				return
			}
			for k, v := range got {
				if v != (k == tc.wantField) {
					t.Errorf("%s set = %v, want %v (only %s should be set)", k, v, k == tc.wantField, tc.wantField)
				}
			}
		})
	}

	// Invalid UTF-8 must survive as base64 rather than being mangled or
	// dropped: the event file has to say what actually arrived.
	raw := []byte{0xff, 0xfe, 'h', 'i'}
	w := &WebhookEvent{}
	w.SetBody("application/octet-stream", raw)
	if w.BodyBase64 == "" {
		t.Fatal("a non-UTF-8 body was not stored as base64")
	}
	back, err := base64.StdEncoding.DecodeString(w.BodyBase64)
	if err != nil || string(back) != string(raw) {
		t.Errorf("body_base64 does not decode back to the original bytes")
	}
	if w.BodyText != "" {
		t.Error("a non-UTF-8 body must not also be stored as text")
	}

	// Setting a body twice leaves exactly one field set, not two.
	w.SetBody("application/json", []byte(`{"a":1}`))
	if w.BodyBase64 != "" || w.BodyText != "" {
		t.Error("SetBody did not clear the fields it no longer uses")
	}
}

// TestAllowedHeadersIsAnAllowlist is the credential boundary. Each assertion
// names a header that must not reach an event file, and the test drives a
// request carrying all of them at once — the realistic shape, since a real
// delivery carries its signature alongside its event name.
func TestAllowedHeadersIsAnAllowlist(t *testing.T) {
	src := core.WebhookSource{
		Name: "gh", Verify: core.VerifyGitHub,
		SignatureHeader: "X-Hub-Signature-256", SignaturePrefix: "sha256=",
		EventHeader: "X-GitHub-Event", DeliveryHeader: "X-GitHub-Delivery",
	}
	sent := map[string]string{
		"Content-Type":        "application/json",
		"User-Agent":          "GitHub-Hookshot/abc",
		"X-GitHub-Event":      "pull_request",
		"X-GitHub-Delivery":   "d-1",
		"X-Hub-Signature-256": "sha256=DEADBEEF",
		"Authorization":       "Bearer super-secret",
		"Cookie":              "session=super-secret",
		"X-Custom":            "not asked for",
	}
	get := func(name string) string {
		for k, v := range sent {
			if strings.EqualFold(k, name) {
				return v
			}
		}
		return ""
	}
	got := AllowedHeaders(src, get)

	want := map[string]string{
		"Content-Type":      "application/json",
		"User-Agent":        "GitHub-Hookshot/abc",
		"X-GitHub-Event":    "pull_request",
		"X-GitHub-Delivery": "d-1",
	}
	if len(got) != len(want) {
		t.Errorf("AllowedHeaders = %v, want exactly %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("header %s = %q, want %q", k, got[k], v)
		}
	}
	// Named explicitly, so a failure says which one leaked rather than only
	// that the map was the wrong size.
	for _, banned := range []string{"Authorization", "Cookie", "X-Hub-Signature-256", "X-Custom"} {
		if _, ok := got[banned]; ok {
			t.Errorf("%s reached the envelope", banned)
		}
	}
	// And by value, because a header could in principle arrive under a
	// different name than the one the source configured.
	encoded, err := json.Marshal(&WebhookEvent{Headers: got})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "super-secret") || strings.Contains(string(encoded), "DEADBEEF") {
		t.Errorf("a credential survived into the encoded event: %s", encoded)
	}
}

// TestAllowedHeadersStandardWebhooks covers the one preset whose timestamp is
// carried and whose signature is not. They share a prefix, which is exactly
// how a denylist-shaped implementation gets this wrong.
func TestAllowedHeadersStandardWebhooks(t *testing.T) {
	sig, prefix, event, delivery, _ := core.PresetHeaders(core.VerifyStandardWebhooks)
	src := core.WebhookSource{
		Name: "sb", Verify: core.VerifyStandardWebhooks,
		SignatureHeader: sig, SignaturePrefix: prefix, EventHeader: event, DeliveryHeader: delivery,
	}
	sent := map[string]string{
		"content-type":      "application/json",
		"webhook-id":        "msg_1",
		"webhook-timestamp": "1758585600",
		"webhook-signature": "v1,SIGNATURE-BYTES",
	}
	got := AllowedHeaders(src, func(n string) string { return sent[strings.ToLower(n)] })

	if got["webhook-id"] != "msg_1" || got[StandardWebhooksTimestampHeader] != "1758585600" {
		t.Errorf("standard-webhooks envelope headers = %v, want the id and the timestamp", got)
	}
	if _, ok := got["webhook-signature"]; ok {
		t.Error("webhook-signature reached the envelope: it authenticates the delivery, it does not describe it")
	}
	if got["Content-Type"] != "application/json" {
		t.Errorf("Content-Type lookup is not case-insensitive: %v", got)
	}
}

// TestAllowedHeadersRefusesACredentialHeaderByName is the defensive half: even
// if a source were somehow configured with `event_header = "Authorization"`,
// the allowlist must still drop it.
func TestAllowedHeadersRefusesACredentialHeaderByName(t *testing.T) {
	src := core.WebhookSource{Name: "b", Verify: core.VerifyBearer, EventHeader: "Authorization", DeliveryHeader: "Cookie"}
	// Only the credential-named headers are present, so anything that comes
	// back is one of them.
	got := AllowedHeaders(src, func(n string) string {
		switch strings.ToLower(n) {
		case "authorization", "cookie":
			return "super-secret"
		}
		return ""
	})
	if len(got) != 0 {
		t.Errorf("AllowedHeaders = %v, want nothing: every header it would carry is a credential", got)
	}

	// And the same for a source that names the signature header as its event
	// header, which the fixed neverCarried list does not cover — the
	// signature is refused because the SOURCE declares it, not because of its
	// name.
	hmac := core.WebhookSource{Name: "h", Verify: core.VerifyHMACSHA256, SignatureHeader: "X-Sig", EventHeader: "X-Sig"}
	if got := AllowedHeaders(hmac, func(string) string { return "sha256=DEADBEEF" }); got["X-Sig"] != "" {
		t.Errorf("the source's own signature header reached the envelope: %v", got)
	}
}

func TestEnvelopeValidation(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Envelope)
		want   string
	}{
		{"wrong version", func(e *Envelope) { e.Version = 2 }, "version"},
		{"unknown kind", func(e *Envelope) { e.Kind = "queue" }, "kind"},
		{"channel kind with no channel", func(e *Envelope) { e.Channel = nil }, "channel"},
		{"channel kind with a webhook", func(e *Envelope) { e.Webhook = &WebhookEvent{} }, "webhook"},
		{"kind and source disagree", func(e *Envelope) { e.Source = "webhook.gh" }, "does not match"},
		{"malformed source", func(e *Envelope) { e.Source = "sb" }, "source"},
		{"missing event_id", func(e *Envelope) { e.EventID = " " }, "event_id"},
		{"missing received_at", func(e *Envelope) { e.ReceivedAt = time.Time{} }, "received_at"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := channelEnvelope()
			tc.mutate(e)
			err := e.Validate()
			if err == nil {
				t.Fatal("Validate accepted a malformed envelope")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}

	webhookCases := []struct {
		name   string
		mutate func(*Envelope)
		want   string
	}{
		{"webhook kind with no webhook", func(e *Envelope) { e.Webhook = nil }, "webhook"},
		{"two bodies", func(e *Envelope) { e.Webhook.BodyText = "also text" }, "more than one"},
		{"bad base64", func(e *Envelope) { e.Webhook.Body = nil; e.Webhook.BodyBase64 = "!!!not base64!!!" }, "base64"},
		{"a credential smuggled in as a header", func(e *Envelope) {
			e.Webhook.Headers = map[string]string{"Authorization": "Bearer x"}
		}, "Authorization"},
	}
	for _, tc := range webhookCases {
		t.Run(tc.name, func(t *testing.T) {
			e := webhookEnvelope()
			tc.mutate(e)
			err := e.Validate()
			if err == nil {
				t.Fatal("Validate accepted a malformed envelope")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}

	// The control: the fixtures themselves validate, so the table above is
	// failing on the mutation rather than on something the fixture got wrong.
	for _, e := range []*Envelope{channelEnvelope(), webhookEnvelope()} {
		if err := e.Validate(); err != nil {
			t.Errorf("a well-formed envelope was rejected: %v", err)
		}
	}
}

func TestParseEnvelopeSizeCap(t *testing.T) {
	e := channelEnvelope()
	e.Channel.Content = strings.Repeat("x", 4096)
	b, _ := e.Encode()

	if _, err := ParseEnvelope(b, int64(len(b)-1)); err == nil {
		t.Fatal("ParseEnvelope accepted an over-cap envelope")
	} else if !strings.Contains(err.Error(), "limit") {
		t.Errorf("error %q should name the limit", err)
	}
	if _, err := ParseEnvelope(b, int64(len(b))); err != nil {
		t.Errorf("an exactly-at-cap envelope was rejected: %v", err)
	}
	if _, err := ParseEnvelope(b, 0); err != nil {
		t.Errorf("a zero cap should mean the 1MiB default: %v", err)
	}
	if _, err := ParseEnvelope([]byte("{not json"), 0); err == nil {
		t.Error("ParseEnvelope accepted invalid JSON")
	}
}

// TestMaxEventBytesFollowsTheHarnessSources covers the per-harness cap. A
// global cap would refuse a replay of a delivery the listener itself accepted.
func TestMaxEventBytesFollowsTheHarnessSources(t *testing.T) {
	cfg := &core.Config{Webhooks: map[string]core.WebhookSource{
		"small": {Name: "small", MaxBody: 1 << 10},
		"big":   {Name: "big", MaxBody: 8 << 20},
	}}
	cases := []struct {
		name     string
		triggers []string
		want     int64
	}{
		{"no triggers at all", nil, DefaultMaxEventBytes},
		{"channel only", []string{"channel.sb"}, DefaultMaxEventBytes},
		{"one webhook", []string{"webhook.small"}, 1 << 10},
		{"the largest of several", []string{"webhook.small", "webhook.big"}, 8 << 20},
		{"an unknown webhook", []string{"webhook.gone"}, DefaultMaxEventBytes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := core.Harness{Name: "h", Triggers: tc.triggers}
			if got := MaxEventBytes(h, cfg); got != tc.want {
				t.Errorf("MaxEventBytes = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestReplayStampsAndCopies(t *testing.T) {
	orig := webhookEnvelope()
	at := time.Date(2026, 9, 22, 13, 0, 0, 0, time.UTC)
	replayed := orig.Replay(at)

	if orig.ReplayedAt != nil {
		t.Error("Replay mutated the original")
	}
	if replayed.ReplayedAt == nil || !replayed.ReplayedAt.Equal(at) {
		t.Errorf("replayed_at = %v, want %v", replayed.ReplayedAt, at)
	}
	// Everything else identical: a replay is a copy, not a translation.
	replayed.ReplayedAt = nil
	a, _ := orig.Encode()
	b, _ := replayed.Encode()
	if string(a) != string(b) {
		t.Errorf("Replay changed something other than replayed_at:\n%s\n---\n%s", a, b)
	}
	if err := orig.Replay(at).Validate(); err != nil {
		t.Errorf("a replayed envelope does not validate: %v", err)
	}
}

func TestBinds(t *testing.T) {
	h := core.Harness{Triggers: []string{"webhook.gh", "channel.sb"}}
	for _, ref := range []string{"webhook.gh", "channel.sb"} {
		if !Binds(h, ref) {
			t.Errorf("Binds(%q) = false", ref)
		}
	}
	for _, ref := range []string{"webhook.sb", "channel.gh", "", "gh"} {
		if Binds(h, ref) {
			t.Errorf("Binds(%q) = true", ref)
		}
	}
}

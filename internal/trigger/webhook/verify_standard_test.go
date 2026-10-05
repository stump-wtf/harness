package webhook

// The `standard-webhooks` verifier: the published interop vector, the
// tolerance edges, and every refusal. Each rejection test signs CORRECTLY
// and then breaks exactly one thing, so removing the matching check in
// verify_standard.go turns the test red — none of them pass merely because
// the input was garbage to begin with.
//
// Governing: SPEC-0014 REQ "Standard Webhooks Verification" (all nine
// scenarios), REQ "Webhook Filtering"; Switchboard SPEC-0024.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// clockAt returns an injectable clock frozen at unix secs.
func clockAt(secs int64) func() time.Time {
	at := time.Unix(secs, 0).UTC()
	return func() time.Time { return at }
}

// standardSign computes the v1 signature of `id.ts.body` under key, which is
// the ALREADY-DECODED secret — the only key the verifier accepts.
func standardSign(key []byte, id, ts string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(id))
	mac.Write([]byte("."))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// standardHeaders builds a well-formed standard-webhooks delivery's headers.
func standardHeaders(id, ts, sig string) map[string]string {
	return map[string]string{
		"Content-Type":      "application/json",
		"Webhook-Id":        id,
		"Webhook-Timestamp": ts,
		"Webhook-Signature": sig,
	}
}

// swFixture is a route secret and its decoded key. 32 bytes, the middle of
// the 24-64 window the parser accepts.
var swFixture = struct {
	key    []byte
	secret string
}{
	key:    []byte("0123456789abcdef0123456789abcdef"),
	secret: "whsec_" + base64.StdEncoding.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
}

// swVerifier builds a verifier over swFixture's secret with the clock pinned
// at unix secs.
func swVerifier(t *testing.T, at int64) standardWebhooksVerifier {
	t.Helper()
	src := swSource(t, "sw", swFixture.secret)
	v, err := NewVerifier(src, clockAt(at))
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v.(standardWebhooksVerifier)
}

// swSource is an enabled standard-webhooks source named name, with the
// preset's header fields filled in exactly as the config parser fills them
// (core.PresetHeaders is the table both read).
func swSource(t *testing.T, name, secret string) core.WebhookSource {
	t.Helper()
	src := bearerSource(name)
	src.Verify = core.VerifyStandardWebhooks
	src.Secret = core.Secret(secret)
	sig, prefix, event, delivery, ok := core.PresetHeaders(core.VerifyStandardWebhooks)
	if !ok {
		t.Fatal("standard-webhooks is not a preset")
	}
	src.SignatureHeader, src.SignaturePrefix, src.EventHeader, src.DeliveryHeader = sig, prefix, event, delivery
	return src
}

// readVector returns the published interop reference vector (testdata/
// standard-webhooks-vector.txt, with its provenance in the file header).
func readVector(t *testing.T) (secret, id, ts, payload, sig string) {
	t.Helper()
	raw, err := os.ReadFile("testdata/standard-webhooks-vector.txt")
	if err != nil {
		t.Fatalf("read vector: %v", err)
	}
	fields := map[string]string{}
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, "#") || !strings.Contains(line, " = ") {
			continue
		}
		k, v, _ := strings.Cut(line, " = ")
		// A trailing " # " comment is not part of the value: the secret line
		// carries gitleaks:allow, because the published vector trips
		// generic-api-key and every scan here fetches all branches.
		v, _, _ = strings.Cut(v, " # ")
		fields[k] = v
	}
	for _, k := range []string{"secret", "msg_id", "timestamp", "payload", "signature"} {
		if fields[k] == "" {
			t.Fatalf("vector is missing %q", k)
		}
	}
	return fields["secret"], fields["msg_id"], fields["timestamp"], fields["payload"], fields["signature"]
}

// TestStandardWebhooksInteropVector pins the published reference vector: a
// delivery the reference libraries sign verifies here, with the clock set to
// the vector's own past timestamp. The same delivery signed with the raw
// secret STRING as the key must fail — the key is the decoded bytes.
func TestStandardWebhooksInteropVector(t *testing.T) {
	secret, id, ts, payload, sig := readVector(t)
	src := swSource(t, "sw", secret)
	v, err := NewVerifier(src, clockAt(1614265330))
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	h := http.Header{}
	for k, val := range standardHeaders(id, ts, sig) {
		h.Set(k, val)
	}
	if err := v.Verify(h, []byte(payload)); err != nil {
		t.Fatalf("reference vector does not verify: %v", err)
	}

	// The key is the decoded bytes, never the string: keying with the raw
	// base64 text produces a different signature, and the verifier must
	// refuse it.
	rawSig := standardSign([]byte(strings.TrimPrefix(secret, "whsec_")), id, ts, []byte(payload))
	h.Set("Webhook-Signature", "v1,"+rawSig)
	if err := v.Verify(h, []byte(payload)); err == nil {
		t.Fatal("a signature keyed by the raw secret string verified; want refusal")
	}
}

// TestStandardWebhooksHexShapedSecret: Switchboard's OLD inbound secrets were
// `whsec_` plus 64 hex characters. Hex is valid base64, so such a secret
// passes the load check (it decodes to 48 bytes), but a sender that signs
// with the raw hex string — the old behavior — gets a 401 from every
// delivery, and only signing with the decoded bytes verifies. This is why
// Switchboard must mint `whsec_` plus base64 (Switchboard SPEC-0024).
func TestStandardWebhooksHexShapedSecret(t *testing.T) {
	hex := "whsec_" + strings.Repeat("a1", 32)
	src := swSource(t, "sw", hex)
	v, err := NewVerifier(src, clockAt(1700000000))
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}

	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(hex, "whsec_"))
	if err != nil {
		t.Fatalf("hex secret does not decode: %v", err)
	}
	const id = "msg_hexshaped"
	const ts = "1700000000"
	body := []byte(`{"type":"todo.ready"}`)

	h := http.Header{}
	for k, val := range standardHeaders(id, ts, "v1,"+standardSign(decoded, id, ts, body)) {
		h.Set(k, val)
	}
	if err := v.Verify(h, body); err != nil {
		t.Fatalf("signing with the decoded key did not verify: %v", err)
	}

	// The old sender behavior: key the MAC with the hex text itself.
	h.Set("Webhook-Signature", "v1,"+standardSign([]byte(strings.TrimPrefix(hex, "whsec_")), id, ts, body))
	if err := v.Verify(h, body); err == nil {
		t.Fatal("a signature keyed by the raw hex text verified; want refusal")
	}
}

// TestStandardWebhooksToleranceEdges: 299 s off the clock passes and 301 s
// fails, in both directions, on deliveries that are otherwise perfectly
// signed — so removing the tolerance check turns this test red.
func TestStandardWebhooksToleranceEdges(t *testing.T) {
	const now = int64(1700000000)
	v := swVerifier(t, now)
	const id = "msg_tolerance"
	body := []byte(`{"type":"todo.ready"}`)
	for _, d := range []struct {
		name string
		off  int64
		ok   bool
	}{
		{"299s past", -299, true},
		{"299s future", 299, true},
		{"300s past", -300, true},
		{"300s future", 300, true},
		{"301s past", -301, false},
		{"301s future", 301, false},
	} {
		ts := fmt.Sprint(now + d.off)
		sig := standardSign(swFixture.key, id, ts, body)
		h := http.Header{}
		for k, val := range standardHeaders(id, ts, "v1,"+sig) {
			h.Set(k, val)
		}
		err := v.Verify(h, body)
		if (err == nil) != d.ok {
			t.Errorf("%s: Verify = %v, want ok=%v", d.name, err, d.ok)
			continue
		}
		if err != nil && !strings.Contains(err.Error(), "timestamp_out_of_tolerance") {
			t.Errorf("%s: error %q does not name the reason", d.name, err)
		}
	}
}

// TestStandardWebhooksRejectsTampering: a delivery that was authentic stays
// authentic under no edit of its content. The signed list is honored entry
// by entry — a wrong first entry does not stop a right second one, which is
// what lets a rotation's dual signing through.
func TestStandardWebhooksRejectsTampering(t *testing.T) {
	v := swVerifier(t, 1700000000)
	const id = "msg_tamper"
	const ts = "1700000000"
	body := []byte(`{"type":"todo.ready","todo_id":"t-1"}`)
	sig := standardSign(swFixture.key, id, ts, body)
	otherKey := []byte(strings.Repeat("f", 32))

	t.Run("tampered body", func(t *testing.T) {
		h := http.Header{}
		for k, val := range standardHeaders(id, ts, "v1,"+sig) {
			h.Set(k, val)
		}
		err := v.Verify(h, []byte(`{"type":"todo.ready","todo_id":"t-2"}`))
		if err == nil || !strings.Contains(err.Error(), "bad_signature") {
			t.Errorf("Verify = %v, want bad_signature", err)
		}
	})
	t.Run("altered delivery id", func(t *testing.T) {
		h := http.Header{}
		for k, val := range standardHeaders("msg_other", ts, "v1,"+sig) {
			h.Set(k, val)
		}
		if err := v.Verify(h, body); err == nil {
			t.Error("an altered webhook-id verified against the original signature")
		}
	})
	t.Run("second signature matches", func(t *testing.T) {
		wrong := standardSign(otherKey, id, ts, body)
		h := http.Header{}
		for k, val := range standardHeaders(id, ts, "v1,"+wrong+" v1,"+sig) {
			h.Set(k, val)
		}
		if err := v.Verify(h, body); err != nil {
			t.Errorf("a list whose second entry matches: %v", err)
		}
	})
	t.Run("old or new secret during rotation", func(t *testing.T) {
		// The sender signs with both its secrets; a route holding either
		// one must accept.
		both := "v1," + standardSign(otherKey, id, ts, body) + " v1," + standardSign(swFixture.key, id, ts, body)
		h := http.Header{}
		for k, val := range standardHeaders(id, ts, both) {
			h.Set(k, val)
		}
		if err := v.Verify(h, body); err != nil {
			t.Errorf("dual-signed delivery refused: %v", err)
		}
	})
	t.Run("only asymmetric entries", func(t *testing.T) {
		asym := standardSign(swFixture.key, id, ts, body) // the right bytes, the wrong version
		h := http.Header{}
		for k, val := range standardHeaders(id, ts, "v1a,"+asym) {
			h.Set(k, val)
		}
		err := v.Verify(h, body)
		if err == nil || !strings.Contains(err.Error(), "bad_signature") {
			t.Errorf("Verify = %v, want bad_signature", err)
		}
	})
	t.Run("undecodable entries are skipped, not fatal", func(t *testing.T) {
		h := http.Header{}
		for k, val := range standardHeaders(id, ts, "v1,!!not-base64!! v1a,x v1,"+sig) {
			h.Set(k, val)
		}
		if err := v.Verify(h, body); err != nil {
			t.Errorf("a list with junk before the matching entry: %v", err)
		}
	})
	t.Run("wrong secret entirely", func(t *testing.T) {
		h := http.Header{}
		for k, val := range standardHeaders(id, ts, "v1,"+standardSign([]byte("a-brand-new-secret-value-32-bytes!"), id, ts, body)) {
			h.Set(k, val)
		}
		if err := v.Verify(h, body); err == nil {
			t.Error("a signature under a different secret verified")
		}
	})
}

// TestStandardWebhooksRefusesMalformed: missing and malformed headers are a
// 401 with the reason named, and no error ever quotes what was presented.
func TestStandardWebhooksRefusesMalformed(t *testing.T) {
	v := swVerifier(t, 1700000000)
	const id = "msg_malformed"
	const ts = "1700000000"
	body := []byte(`{"type":"todo.ready"}`)
	sentinelSecret := swFixture.secret

	cases := []struct {
		name   string
		id     string
		ts     string
		sig    string
		reason string
	}{
		{"no id", "", ts, "v1,SENTINEL-AAAA", "missing_header"},
		{"no timestamp", id, "", "v1,SENTINEL-AAAA", "missing_header"},
		{"no signature", id, ts, "", "missing_header"},
		{"two id headers", id, ts, "v1,SENTINEL-AAAA", "malformed_header"},
		{"id too long", strings.Repeat("x", 257), ts, "v1,SENTINEL-AAAA", "malformed_header"},
		{"id with a space", "msg x", ts, "v1,SENTINEL-AAAA", "malformed_header"},
		{"empty id", "", ts, "v1,SENTINEL-AAAA", "missing_header"},
		{"timestamp not decimal", id, "yesterday", "v1,SENTINEL-AAAA", "malformed_header"},
		{"no v1 entries at all", id, ts, "v0,SENTINEL-AAAA v1a,SENTINEL-AAAA", "bad_signature"},
	}
	for _, c := range cases {
		h := http.Header{}
		for k, val := range standardHeaders(c.id, c.ts, c.sig) {
			h.Set(k, val)
		}
		if c.name == "two id headers" {
			h.Add("Webhook-Id", id)
		}
		err := v.Verify(h, body)
		if err == nil {
			t.Errorf("%s: Verify = nil, want refusal", c.name)
			continue
		}
		if !errors.Is(err, ErrUnauthorized) {
			t.Errorf("%s: error %v does not wrap ErrUnauthorized", c.name, err)
		}
		if !strings.Contains(err.Error(), c.reason) {
			t.Errorf("%s: error %q does not name the reason %q", c.name, err, c.reason)
		}
		if strings.Contains(err.Error(), "SENTINEL") || strings.Contains(err.Error(), sentinelSecret) {
			t.Errorf("%s: error %q echoes a presented value", c.name, err)
		}
	}
}

// TestStandardWebhooksEventName: the event name is the body's top-level
// `type`, and only a JSON object with a string `type` has one.
func TestStandardWebhooksEventName(t *testing.T) {
	v := swVerifier(t, 1700000000)
	cases := []struct {
		name, body, want string
	}{
		{"object", `{"type":"todo.ready","todo_id":"t-1"}`, "todo.ready"},
		{"nested only", `{"a":{"type":"todo.ready"}}`, ""},
		{"non-string type", `{"type":7}`, ""},
		{"array body", `["todo.ready"]`, ""},
		{"not json", `hello`, ""},
		{"empty body", ``, ""},
	}
	for _, c := range cases {
		if got := v.EventName([]byte(c.body)); got != c.want {
			t.Errorf("%s: EventName = %q, want %q", c.name, got, c.want)
		}
	}
}

// TestStandardWebhooksMalformedSecretRefusesRoute: the second lock behind
// the parser's whsec_ check — a route whose secret does not decode is built
// refusing, and answers 401 to a delivery signed perfectly for the key the
// secret would have had.
func TestStandardWebhooksMalformedSecretRefusesRoute(t *testing.T) {
	src := bearerSource("sw")
	src.Verify = core.VerifyStandardWebhooks
	src.Secret = core.Secret("whsec_!!!not-base64!!!")
	if _, err := NewVerifier(src, nil); !errors.Is(err, ErrSchemeUnavailable) {
		t.Fatalf("NewVerifier = %v, want ErrSchemeUnavailable", err)
	}

	f := &fakeFirer{}
	srv, logs := startServer(t, Options{Firer: f}, testConfig([]core.WebhookSource{src}, "sw"))
	const id, ts = "msg_refused", "1700000000"
	body := []byte(`{"type":"todo.ready"}`)
	sig := standardSign([]byte("0123456789abcdef0123456789abcdef"), id, ts, body)
	hdr := standardHeaders(id, ts, "v1,"+sig)
	resp, _ := do(t, "POST", "http://"+srv.Addr()+"/hooks/sw", hdr, string(body))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", resp.StatusCode)
	}
	if n := len(f.fired()); n != 0 {
		t.Errorf("%d deliveries fired through a route with an unusable secret", n)
	}
	if !strings.Contains(logs.String(), "refuses every delivery") {
		t.Error("building a refusing route logged no warning")
	}
}

// TestStandardWebhooksNotifyHookWiring: a Switchboard-shaped notify hook,
// posted through the real listener, fires the bound harness with the event
// name taken from the body's `type`, the run's event_id taken from
// webhook-id — and an events allowlist filters on the body — while the
// signature and the secret stay out of the logs, the response and the
// envelope.
func TestStandardWebhooksNotifyHookWiring(t *testing.T) {
	src := swSource(t, "switchboard", swFixture.secret)
	src.Events = []string{"todo.ready"}
	f := &fakeFirer{}
	srv, logs := startServer(t, Options{Firer: f, Now: clockAt(1700000000)}, testConfig([]core.WebhookSource{src}, "switchboard"))
	url := "http://" + srv.Addr() + "/hooks/switchboard"

	const sentinelSig = "SENTINEL-signature-nobody-should-see"
	sign := func(id string, body []byte) map[string]string {
		ts := "1700000000"
		s := standardSign(swFixture.key, id, ts, body)
		return standardHeaders(id, ts, "v1,"+s+" v1,"+sentinelSig)
	}

	// The body's type is not in the allowlist: accepted, ignored, nothing
	// fires.
	ignored := []byte(`{"type":"todo.created","todo_id":"t-9"}`)
	resp, _ := do(t, "POST", url, sign("msg_created", ignored), string(ignored))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("filtered delivery: status %d, want 202", resp.StatusCode)
	}
	if n := len(f.fired()); n != 0 {
		t.Fatalf("%d deliveries fired through the events filter", n)
	}

	// The notify hook itself: one firing, event_id = webhook-id, the event
	// name from the body, the body carried as parsed JSON.
	ready := []byte(`{"type":"todo.ready","todo_id":"t-1"}`)
	resp, rbody := do(t, "POST", url, sign("msg_ready", ready), string(ready))
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("notify hook: status %d, body %s", resp.StatusCode, rbody)
	}
	evs := f.fired()
	if len(evs) != 1 {
		t.Fatalf("fired %d times, want 1", len(evs))
	}
	ev := evs[0]
	if ev.Source != "webhook.switchboard" || ev.EventID != "msg_ready" {
		t.Errorf("envelope source/event_id = %q/%q", ev.Source, ev.EventID)
	}
	if ev.Webhook == nil || ev.Webhook.Event != "todo.ready" || ev.Webhook.Delivery != "msg_ready" {
		t.Errorf("envelope webhook = %+v", ev.Webhook)
	}
	if string(ev.Webhook.Body) != string(ready) {
		t.Errorf("event file body = %s, want %s", ev.Webhook.Body, ready)
	}
	if _, ok := ev.Webhook.Headers["webhook-id"]; !ok {
		t.Error("envelope headers carry no webhook-id")
	}
	if _, ok := ev.Webhook.Headers["webhook-timestamp"]; !ok {
		t.Error("envelope headers carry no webhook-timestamp")
	}
	if _, ok := ev.Webhook.Headers["webhook-signature"]; ok {
		t.Error("envelope headers carry webhook-signature")
	}

	// The signature and the secret never reach the logs, the response or
	// the event file. The sentinel rides in the signature list; the secret
	// is never on the wire, so its absence from the log buffer is what is
	// checked.
	for _, sink := range []string{logs.String(), string(rbody), string(ready)} {
		if strings.Contains(sink, sentinelSig) || strings.Contains(sink, swFixture.secret) {
			t.Error("a sentinel credential reached a log, response or event file")
		}
	}
	if strings.Contains(logs.String(), swFixture.secret) {
		t.Error("the route secret reached the log buffer")
	}
}

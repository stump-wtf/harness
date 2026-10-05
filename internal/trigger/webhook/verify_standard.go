package webhook

// The `standard-webhooks` scheme: the Standard Webhooks (standardwebhooks.com)
// v1 symmetric signature, the scheme Switchboard signs its notify hooks with
// (Switchboard ADR-0029 / SPEC-0024), and any sender following the spec.
//
// A delivery carries `webhook-id`, `webhook-timestamp` and
// `webhook-signature: v1,<base64> …`. The expected signature is HMAC-SHA256
// keyed by the secret the `whsec_`-prefixed base64 DECODES to — never the
// string — over `<id>.<timestamp>.<raw body>`, byte for byte as received. A
// delivery passes when ANY `v1` entry matches, compared in constant time, so
// a sender mid-rotation signing with both secrets verifies against a route
// holding either one. Unknown versions (`v1a`) and entries that do not decode
// are skipped, not rejected.
//
// The timestamp must sit within 300 s of the server's clock, read through the
// same injectable seam every constructor gets; the fixed interop vector in
// testdata pins a past timestamp and passes only because the clock is set.
//
// Every refusal names a reason code — `missing_header`, `malformed_header`,
// `timestamp_out_of_tolerance`, `bad_signature` — and never a value: the
// pipeline logs the error text, and the header values here are all
// credential-adjacent (the signature literally is one).
//
// Governing: ADR-0021; SPEC-0014 REQ "Standard Webhooks Verification",
// REQ "Webhook Filtering" (the event name), Security Requirements
// "Authentication"; Switchboard SPEC-0024.
//
// @joestump-agent 10/05/2026 - Introduced with the standard-webhooks
// verifier (#466).

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// standardSecretPrefix begins every Standard Webhooks symmetric secret; the
// HMAC key is what follows it, base64-decoded (SPEC-0014 item 1).
const standardSecretPrefix = "whsec_"

// toleranceSeconds is how far a delivery's webhook-timestamp may lie from the
// server's clock, in either direction (SPEC-0014 item 3).
const toleranceSeconds = 300

// standardIDMaxLen bounds webhook-id, which is signed content and the
// delivery ID: past 256 bytes of it no sender is paging anyone useful.
const standardIDMaxLen = 256

// standardWebhooksVerifier checks the Standard Webhooks v1 signature.
// standardWebhooksKey hands back the decoded key; the verifier is immutable
// and shared by every delivery to its route.
type standardWebhooksVerifier struct {
	key []byte
	now func() time.Time
}

// newStandardWebhooksVerifier builds the standard-webhooks verifier. A secret
// the config parser would have refused (no whsec_, not padded base64, wrong
// decoded length) refuses the route here too rather than keying a MAC with
// whatever the string happens to be.
func newStandardWebhooksVerifier(src core.WebhookSource, now func() time.Time) (Verifier, error) {
	if now == nil {
		now = time.Now
	}
	key, err := standardWebhooksKey(src.Secret)
	if err != nil {
		return nil, fmt.Errorf("%w: [webhook.%s] %v", ErrSchemeUnavailable, src.Name, err)
	}
	return standardWebhooksVerifier{key: key, now: now}, nil
}

// standardWebhooksKey decodes the route secret into the HMAC key: the bytes
// after the whsec_ prefix, base64-decoded. Switchboard mints exactly this
// shape (SPEC-0024); the parser already refuses anything else, and this is
// the second lock on the same door. The reason names the rule, never the
// value.
func standardWebhooksKey(secret core.Secret) ([]byte, error) {
	rest, ok := strings.CutPrefix(secret.Reveal(), standardSecretPrefix)
	if !ok {
		return nil, fmt.Errorf("secret lacks the %q prefix", standardSecretPrefix)
	}
	key, err := base64.StdEncoding.DecodeString(rest)
	if err != nil {
		return nil, fmt.Errorf("secret is not padded standard base64 after %q", standardSecretPrefix)
	}
	if len(key) < 24 || len(key) > 64 {
		return nil, fmt.Errorf("secret decodes to %d bytes, want 24 to 64", len(key))
	}
	return key, nil
}

// Verify implements Verifier. Governing: SPEC-0014 REQ "Standard Webhooks
// Verification" items 2-5.
func (v standardWebhooksVerifier) Verify(h http.Header, body []byte) error {
	// All three headers must be there; naming every missing one keeps an
	// operator's first debugging step short. Header NAMES are
	// case-insensitive here, as HTTP headers are (item 2).
	id, err := standardHeader(h, "Webhook-Id")
	if err != nil {
		return err
	}
	ts, err := standardHeader(h, "Webhook-Timestamp")
	if err != nil {
		return err
	}
	sigs, err := standardHeader(h, "Webhook-Signature")
	if err != nil {
		return err
	}

	if !standardIDOK(id) {
		return fmt.Errorf("%w: malformed_header (Webhook-Id is not 1 to %d bytes of visible ASCII)", ErrUnauthorized, standardIDMaxLen)
	}
	tsSecs, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fmt.Errorf("%w: malformed_header (Webhook-Timestamp is not a decimal count of seconds)", ErrUnauthorized)
	}
	now := v.now().Unix()
	if tsSecs < now-toleranceSeconds || tsSecs > now+toleranceSeconds {
		return fmt.Errorf("%w: timestamp_out_of_tolerance (Webhook-Timestamp is more than %d seconds from the server's clock)", ErrUnauthorized, toleranceSeconds)
	}

	// The signed content is the id, the timestamp and the body, exactly as
	// received: any reinterpretation of any of the three verifies a delivery
	// whose bytes were never signed.
	mac := hmac.New(sha256.New, v.key)
	mac.Write([]byte(id))
	mac.Write([]byte("."))
	mac.Write([]byte(ts))
	mac.Write([]byte("."))
	mac.Write(body)
	expected := mac.Sum(nil)

	accepted := false
	for _, entry := range strings.Split(sigs, " ") {
		version, sig, ok := strings.Cut(entry, ",")
		if !ok || version != "v1" {
			continue
		}
		got, err := base64.StdEncoding.DecodeString(sig)
		if err != nil {
			continue
		}
		if hmac.Equal(got, expected) {
			accepted = true
			break
		}
	}
	if !accepted {
		// A list with no v1 entry — only v1a, say — lands here too: the
		// header is well-formed, nothing in it authenticates the delivery.
		return fmt.Errorf("%w: bad_signature (no v1 entry matches the expected signature)", ErrUnauthorized)
	}
	return nil
}

// standardHeader returns the single value of one of the scheme's headers,
// refusing with the reason code SPEC-0014 item "A failure at any item" names:
// a missing or empty header is missing_header, and two of them is
// malformed_header — two signature headers is a request built to confuse
// whichever layer reads "the" header, so none of them is picked.
func standardHeader(h http.Header, name string) (string, error) {
	values := h.Values(name)
	switch {
	case len(values) == 0 || (len(values) == 1 && values[0] == ""):
		return "", fmt.Errorf("%w: missing_header (no %s header)", ErrUnauthorized, name)
	case len(values) > 1:
		return "", fmt.Errorf("%w: malformed_header (more than one %s header)", ErrUnauthorized, name)
	}
	return values[0], nil
}

// standardIDOK reports whether id is 1 to 256 bytes of visible ASCII
// (0x21-0x7E), the shape the spec bounds webhook-id to.
func standardIDOK(id string) bool {
	if id == "" || len(id) > standardIDMaxLen {
		return false
	}
	for i := 0; i < len(id); i++ {
		if id[i] < 0x21 || id[i] > 0x7E {
			return false
		}
	}
	return true
}

// EventName returns the delivery's event name: the body's top-level `type`
// member when the body is a JSON object whose `type` is a string, and ""
// otherwise — a delivery with no event name passes no `events` allowlist
// (SPEC-0014 REQ "Standard Webhooks Verification" item 6, REQ "Webhook
// Filtering"). The server calls it only after Verify has passed.
func (v standardWebhooksVerifier) EventName(body []byte) string {
	var top struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(body, &top); err != nil {
		return ""
	}
	return top.Type
}

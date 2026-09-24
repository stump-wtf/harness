package webhook

// The `hmac-sha256` scheme: a hex HMAC-SHA256 of the raw body, keyed by the
// route's secret, read from the operator's signature_header after an optional
// signature_prefix. The `github` and `gitea` presets are this verifier with
// their headers fixed (verify_presets.go).
//
// The MAC is computed over the body bytes exactly as they arrived — the slice
// the pipeline read under max_body — and nothing has parsed them yet. Verifying
// a decoded and re-encoded body instead would accept a delivery whose bytes
// were never signed, and reject a real one whose JSON happens to re-encode
// differently.
//
// Every refusal names the rule that failed and the header it read, never the
// value presented: the pipeline logs the error text.
//
// Governing: ADR-0021; SPEC-0014 REQ "Webhook Verification" (the
// `hmac-sha256`, `github` and `gitea` rows), Security Requirements
// "Authentication".
//
// @joestump 09/24/2026 - Introduced with the HMAC schemes and presets (#462).

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"

	"github.com/stump-wtf/harness/internal/core"
)

// hmacVerifier checks `<header>: <prefix><hex HMAC-SHA256 of the body>`.
type hmacVerifier struct {
	key    []byte
	header string
	prefix string
}

// newHMACVerifier builds the `hmac-sha256` verifier from the operator's
// signature_header and signature_prefix. The parser already requires the
// header; a source without one is refused here too rather than read from a
// header named "".
func newHMACVerifier(src core.WebhookSource) (Verifier, error) {
	header := strings.TrimSpace(src.SignatureHeader)
	if header == "" {
		return nil, fmt.Errorf("%w: [webhook.%s] verify = %q has no signature_header", ErrSchemeUnavailable, src.Name, src.Verify)
	}
	return hmacVerifier{key: []byte(src.Secret.Reveal()), header: header, prefix: src.SignaturePrefix}, nil
}

// Verify implements Verifier. The prefix is matched byte for byte, as the
// parser promises (it does not trim one); the hex digits in either case.
// Governing: SPEC-0014 REQ "Webhook Verification".
func (v hmacVerifier) Verify(h http.Header, body []byte) error {
	value, err := oneHeader(h, v.header)
	if err != nil {
		return err
	}
	digits, ok := strings.CutPrefix(value, v.prefix)
	if !ok {
		return fmt.Errorf("%w: malformed %s header (want the %q prefix)", ErrUnauthorized, v.header, v.prefix)
	}
	// The length is checked before decoding so a 1 MiB "signature" costs
	// nothing, and because a MAC of any other length cannot match. A
	// signature's length is not a secret, so this early return leaks nothing.
	if len(digits) != hex.EncodedLen(sha256.Size) {
		return fmt.Errorf("%w: malformed %s header (want %d hex digits)", ErrUnauthorized, v.header, hex.EncodedLen(sha256.Size))
	}
	got := make([]byte, sha256.Size)
	if _, err := hex.Decode(got, []byte(digits)); err != nil {
		// hex's error quotes the offending byte; drop it.
		return fmt.Errorf("%w: malformed %s header (not hex)", ErrUnauthorized, v.header)
	}
	mac := hmac.New(sha256.New, v.key)
	mac.Write(body)
	if !hmac.Equal(got, mac.Sum(nil)) {
		return fmt.Errorf("%w: signature mismatch in %s", ErrUnauthorized, v.header)
	}
	return nil
}

// oneHeader returns the single non-empty value of header name. None, an empty
// one, or more than one is a refusal: two signature headers is a request built
// to confuse whichever layer reads "the" header, so none of them is picked.
func oneHeader(h http.Header, name string) (string, error) {
	values := h.Values(name)
	switch {
	case len(values) == 0 || (len(values) == 1 && values[0] == ""):
		return "", fmt.Errorf("%w: missing %s header", ErrUnauthorized, name)
	case len(values) > 1:
		return "", fmt.Errorf("%w: more than one %s header", ErrUnauthorized, name)
	}
	return values[0], nil
}

package webhook

// Delivery verification: the scheme interface, and `bearer`, its first
// implementation.
//
// Every scheme answers one question — did whoever holds the route's secret
// send this exact request? — and answers it before anything else looks at the
// body. A scheme is a constructor in the `schemes` registry keyed by
// core.VerifyScheme, so the HMAC presets (#462) plug in by adding entries and
// nothing in the pipeline changes.
//
// Fail closed, in three places:
//
//   - A route whose scheme has no constructor here is built with a verifier
//     that refuses every delivery. The config parser accepts all six schemes
//     already (#454), and "accepted but not implemented" must never mean
//     "accepted and unauthenticated".
//   - A route with an empty secret gets the same refusing verifier. The parser
//     rejects an empty secret already; this is the second lock on the door.
//   - A verifier returns an error for anything short of a match — missing,
//     malformed, wrong — and the pipeline answers every one of them with the
//     same 401 body.
//
// Governing: ADR-0021; SPEC-0014 REQ "Webhook Verification", Security
// Requirements "Authentication".
//
// @joestump 09/23/2026 - Introduced with the SPEC-0014 webhook listener (#458).

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/stump-wtf/harness/internal/core"
)

// ErrUnauthorized is what every failed verification wraps. The wrapped detail
// says which rule failed — "missing", "malformed", "mismatch" — and never
// carries the value the request presented, so it is safe to log.
var ErrUnauthorized = errors.New("webhook: delivery failed verification")

// ErrSchemeUnavailable reports a `verify` scheme this build cannot check. A
// route built with it refuses every delivery rather than accepting any.
var ErrSchemeUnavailable = errors.New("webhook: verify scheme not available")

// Verifier authenticates one delivery. It sees the request's headers and its
// whole raw body — already bounded by the route's max_body — and returns nil
// only for an authentic delivery. Any error means 401.
//
// Implementations MUST compare in constant time and MUST NOT put the presented
// value in an error: the pipeline logs the error text.
type Verifier interface {
	Verify(h http.Header, body []byte) error
}

// NewVerifierFunc builds the verifier for one source. It returns an error when
// the source cannot be verified at all, and the route is then built refusing.
type NewVerifierFunc func(src core.WebhookSource) (Verifier, error)

// schemes is the scheme registry. #462 adds hmac-sha256, github, gitea and
// gitlab here; standard-webhooks follows.
var schemes = map[core.VerifyScheme]NewVerifierFunc{
	core.VerifyBearer: newBearerVerifier,
}

// NewVerifier returns the verifier for src, or an error naming why none can be
// built. Callers that get an error MUST refuse the route's deliveries; see
// refusing.
func NewVerifier(src core.WebhookSource) (Verifier, error) {
	if src.Secret.Empty() {
		return nil, fmt.Errorf("%w: [webhook.%s] has no resolved secret", ErrSchemeUnavailable, src.Name)
	}
	build, ok := schemes[src.Verify]
	if !ok {
		return nil, fmt.Errorf("%w: [webhook.%s] verify = %q is not implemented yet", ErrSchemeUnavailable, src.Name, src.Verify)
	}
	return build(src)
}

// refusing is the verifier a route gets when no real one could be built. It
// refuses everything, so an unimplemented scheme or a missing secret is a
// closed route and never an open one.
type refusing struct{ why error }

func (r refusing) Verify(http.Header, []byte) error {
	return fmt.Errorf("%w: %v", ErrUnauthorized, r.why)
}

// bearerVerifier checks `Authorization: Bearer <secret>`.
//
// It holds a SHA-256 of the secret, never the secret: comparing digests makes
// the comparison constant-time in the token's LENGTH as well as its content
// (subtle.ConstantTimeCompare returns early on a length mismatch), and it keeps
// the plain value out of one more long-lived struct.
type bearerVerifier struct {
	want [sha256.Size]byte
}

func newBearerVerifier(src core.WebhookSource) (Verifier, error) {
	return bearerVerifier{want: sha256.Sum256([]byte(src.Secret.Reveal()))}, nil
}

// Verify implements Verifier. The scheme word is matched case-insensitively
// (RFC 9110 §11.1); the token is matched exactly.
// Governing: SPEC-0014 REQ "Webhook Verification" (the `bearer` row).
func (b bearerVerifier) Verify(h http.Header, _ []byte) error {
	values := h.Values("Authorization")
	switch {
	case len(values) == 0:
		return fmt.Errorf("%w: missing Authorization header", ErrUnauthorized)
	case len(values) > 1:
		// Two Authorization headers is a request built to confuse whichever
		// layer reads "the" header. Refuse rather than pick one.
		return fmt.Errorf("%w: more than one Authorization header", ErrUnauthorized)
	}
	scheme, token, ok := strings.Cut(strings.TrimSpace(values[0]), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return fmt.Errorf("%w: malformed Authorization header (want the Bearer scheme)", ErrUnauthorized)
	}
	token = strings.TrimLeft(token, " ")
	if token == "" {
		return fmt.Errorf("%w: malformed Authorization header (empty token)", ErrUnauthorized)
	}
	got := sha256.Sum256([]byte(token))
	if subtle.ConstantTimeCompare(got[:], b.want[:]) != 1 {
		return fmt.Errorf("%w: token mismatch", ErrUnauthorized)
	}
	return nil
}

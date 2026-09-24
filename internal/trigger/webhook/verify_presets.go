package webhook

// The forge presets: `github`, `gitea` and `gitlab`.
//
// A preset is a scheme whose headers are fixed, so a GitHub route is a
// three-line table and a wrong header name or prefix is not a mistake anyone
// can make. The header names come from core.PresetHeaders — the same table the
// parser fills a preset source's fields from — so the header the verifier
// reads and the one the config reports can never be two different headers.
//
//   - github: `X-Hub-Signature-256: sha256=<hex HMAC-SHA256 of the body>`.
//   - gitea:  `X-Gitea-Signature: <hex HMAC-SHA256 of the body>`.
//   - gitlab: `X-Gitlab-Token: <secret>`. GitLab signs nothing; it echoes the
//     shared secret, so this is a token compare, not a MAC.
//
// None of the three carries a timestamp, so there is no replay window to
// enforce here; a replayed delivery is caught by its delivery ID in
// de-duplication (#460).
//
// Governing: ADR-0021; SPEC-0014 REQ "Webhook Verification" (the `github`,
// `gitea` and `gitlab` rows), REQ "Webhook Source Table"; design.md
// "Verification presets, not a scheme language".
//
// @joestump 09/24/2026 - Introduced with the HMAC schemes and presets (#462).

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"net/http"

	"github.com/stump-wtf/harness/internal/core"
)

// presetHeader returns the signature header and prefix preset scheme fixes,
// and refuses a source whose fields say otherwise. The parser fills those
// fields from the same table, so a disagreement is a bug upstream — and the
// safe reading of a bug in which header authenticates a route is "none does".
func presetHeader(src core.WebhookSource) (header, prefix string, err error) {
	header, prefix, _, _, ok := core.PresetHeaders(src.Verify)
	if !ok {
		return "", "", fmt.Errorf("%w: [webhook.%s] verify = %q is not a preset", ErrSchemeUnavailable, src.Name, src.Verify)
	}
	if src.SignatureHeader != header || src.SignaturePrefix != prefix {
		return "", "", fmt.Errorf("%w: [webhook.%s] names signature header %q, but verify = %q reads %q",
			ErrSchemeUnavailable, src.Name, src.SignatureHeader, src.Verify, header)
	}
	return header, prefix, nil
}

// newHMACPresetVerifier builds `github` or `gitea`: the hmac-sha256 verifier
// over the preset's own header and prefix.
func newHMACPresetVerifier(src core.WebhookSource) (Verifier, error) {
	header, prefix, err := presetHeader(src)
	if err != nil {
		return nil, err
	}
	return hmacVerifier{key: []byte(src.Secret.Reveal()), header: header, prefix: prefix}, nil
}

// tokenVerifier checks that one header carries the secret verbatim.
//
// Like bearerVerifier it holds a SHA-256 of the secret and compares digests,
// so the comparison is constant-time in the token's length as well as its
// content.
type tokenVerifier struct {
	header string
	want   [sha256.Size]byte
}

// newGitLabVerifier builds `gitlab`: X-Gitlab-Token must equal the secret.
func newGitLabVerifier(src core.WebhookSource) (Verifier, error) {
	header, _, err := presetHeader(src)
	if err != nil {
		return nil, err
	}
	return tokenVerifier{header: header, want: sha256.Sum256([]byte(src.Secret.Reveal()))}, nil
}

// Verify implements Verifier. The token is matched exactly: no scheme word,
// no trimming beyond what HTTP header parsing already does.
// Governing: SPEC-0014 REQ "Webhook Verification" (the `gitlab` row).
func (v tokenVerifier) Verify(h http.Header, _ []byte) error {
	value, err := oneHeader(h, v.header)
	if err != nil {
		return err
	}
	got := sha256.Sum256([]byte(value))
	if subtle.ConstantTimeCompare(got[:], v.want[:]) != 1 {
		return fmt.Errorf("%w: token mismatch in %s", ErrUnauthorized, v.header)
	}
	return nil
}

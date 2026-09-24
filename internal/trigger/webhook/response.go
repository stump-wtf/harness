package webhook

// What the listener writes back.
//
// Every body is fixed-shape JSON naming only the route and its decisions — no
// run output, no environment, no configuration, no reason a verification
// failed. Every response, errors included, carries the full security-header
// set; that is applied once, in writeResponse, so there is no status a handler
// can reach without it.
//
// Error bodies are constants rendered once. That is what makes two 404s — one
// for a name never declared, one for a disabled source — byte-identical rather
// than merely equivalent: they are the same bytes from the same variable.
//
// Governing: ADR-0021; SPEC-0014 REQ "Webhook Responses", Security
// Requirements "Security Headers", "CSRF Protection", "Redirect Validation".
//
// @joestump 09/23/2026 - Introduced with the SPEC-0014 webhook listener (#458).

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/stump-wtf/harness/internal/trigger/source"
)

// securityHeaders is the set every response carries. HSTS is added separately,
// and only when the listener terminates TLS itself: sent over cleartext it is
// ignored by browsers, and sent by a listener behind a proxy it would pin a
// policy the proxy owns.
var securityHeaders = [][2]string{
	{"Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'"},
	{"X-Frame-Options", "DENY"},
	{"X-Content-Type-Options", "nosniff"},
	{"Referrer-Policy", "strict-origin-when-cross-origin"},
	{"Cache-Control", "no-store"},
}

// hstsValue is Strict-Transport-Security under the listener's own TLS.
const hstsValue = "max-age=31536000"

// Error codes, as the "error" member spells them.
const (
	errUnauthorized     = "unauthorized"
	errNotFound         = "not_found"
	errMethodNotAllowed = "method_not_allowed"
	errPayloadTooLarge  = "payload_too_large"
	errUnavailable      = "unavailable"
	errBadRequest       = "bad_request"
)

// errorBodies holds each error body pre-rendered.
var errorBodies = func() map[string][]byte {
	out := map[string][]byte{}
	for _, code := range []string{errUnauthorized, errNotFound, errMethodNotAllowed, errPayloadTooLarge, errUnavailable, errBadRequest} {
		b, _ := json.Marshal(map[string]string{"error": code})
		out[code] = append(b, '\n')
	}
	return out
}()

// Firing is one harness's decision in a 202.
type Firing struct {
	Harness string `json:"harness"`
	// Decision is "started", "queued" or "skipped" — or "error" when the
	// harness could not be fired at all (see decisionError).
	Decision string `json:"decision"`
	// RunID is present for started and skipped, absent for queued and error.
	RunID *int `json:"run_id,omitempty"`
}

// decisionError is a firing that never reached a decision: the harness is
// unknown to the supervisor, it shut down under a reload, or firing it
// panicked. The spec's vocabulary has no word for this, and reporting such a
// harness as "started" or leaving it out would both be lies to the sender. The
// detail is logged by the source manager and never sent: it can name internals.
const decisionError = "error"

// firingsFrom renders the source manager's decisions for the 202. Always a
// non-nil slice, so a fan-out that reached no harness (a reload unbound the
// route mid-request) renders as `"firings": []` rather than disappearing.
//
// It is the ONLY place a source.Decision becomes response JSON, so a new
// decision field (a skip reason such as `outside_hours`, #484) is surfaced by
// editing this function and Firing, and nothing else.
func firingsFrom(ds []source.Decision) []Firing {
	out := make([]Firing, 0, len(ds))
	for _, d := range ds {
		f := Firing{Harness: d.Harness, Decision: string(d.Kind)}
		switch {
		case d.Err != "" || d.Kind == "":
			f.Decision = decisionError
		case d.Kind == "started" || d.Kind == "skipped":
			id := d.RunID
			f.RunID = &id
		}
		out = append(out, f)
	}
	return out
}

// firedBody renders a fired 202. Firings is emitted even when empty.
func firedBody(name, eventID string, firings []Firing) []byte {
	b, _ := json.Marshal(struct {
		Webhook  string   `json:"webhook"`
		EventID  string   `json:"event_id"`
		Decision string   `json:"decision"`
		Firings  []Firing `json:"firings"`
	}{name, eventID, "fired", firings})
	return append(b, '\n')
}

// ignoredBody renders the 202 for a delivery the `events` allowlist dropped.
// It carries no event_id: the delivery fired nothing and made no record, so
// there is nothing for the sender to correlate it with.
func ignoredBody(name string) []byte {
	b, _ := json.Marshal(struct {
		Webhook  string `json:"webhook"`
		Decision string `json:"decision"`
	}{name, "ignored"})
	return append(b, '\n')
}

// writeResponse is the only function that writes a status. It sets the
// security headers first, so no path can answer without them.
func writeResponse(w http.ResponseWriter, tls bool, status int, contentType string, body []byte) {
	h := w.Header()
	for _, kv := range securityHeaders {
		h.Set(kv[0], kv[1])
	}
	if tls {
		h.Set("Strict-Transport-Security", hstsValue)
	}
	h.Set("Content-Type", contentType)
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// writeError answers with one of the fixed error bodies.
func writeError(w http.ResponseWriter, tls bool, status int, code string) {
	writeResponse(w, tls, status, "application/json", errorBodies[code])
}

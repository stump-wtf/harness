package trigger

// Source states — the vocabulary `harness triggers` reports and the source
// manager maintains.
//
// It lives here, beside the envelope, because three packages need to agree on
// it: the channel listener reports into it, the source manager owns it, and
// the protocol projects it. A per-package copy would be three enums that drift.
//
// Governing: ADR-0021; SPEC-0014 REQ "Trigger Visibility", REQ "Channel
// Listener Session", REQ "Webhook Listener".
//
// @joestump 09/23/2026 - Introduced with the SPEC-0014 channel listener (#471).

// SourceState is a trigger source's state.
type SourceState string

const (
	// StateDisabled: the table sets `enabled = false`. It stays declared and
	// bindable, and nothing connects or serves.
	StateDisabled SourceState = "disabled"
	// StateUnbound: no harness lists this source in its `triggers`. Not an
	// error — a source may be declared before the harness that uses it — but
	// nothing opens, because there would be nobody to fire.
	StateUnbound SourceState = "unbound"
	// StateConnecting: a channel session is being established.
	StateConnecting SourceState = "connecting"
	// StateConnected: a channel session's GET stream is open and being read.
	// ONLY that: a session that initialized and answers pings over POST says
	// nothing about whether doorbells can reach it.
	StateConnected SourceState = "connected"
	// StateBackoff: a channel session failed and is waiting to retry.
	StateBackoff SourceState = "backoff"
	// StateError: a channel session failed in a way retrying at speed will
	// not fix — a missing capability, a refused stream, a rejected
	// credential.
	StateError SourceState = "error"
	// StateListening: a webhook source's route is being served.
	StateListening SourceState = "listening"
	// StateNoListener: a webhook source is declared and bound, but no
	// listener is configured, so its route is served nowhere.
	StateNoListener SourceState = "no_listener"
)

// Outcome is what became of one thing a source heard: a delivery or a
// notification. The set is REQ "Trigger Metrics"'s `outcome` label, and
// `harness triggers` reports a counter per value.
//
// It lives beside SourceState for the same reason: the source manager counts
// these, the protocol projects them, and the metrics story (#480) exports
// them — one vocabulary, not three.
//
// @joestump 09/24/2026 - Introduced with `harness triggers` (#476).
type Outcome string

const (
	// OutcomeFired: the event reached the fan-out.
	OutcomeFired Outcome = "fired"
	// OutcomeIgnored: a verified delivery the source's `events` filter
	// dropped.
	OutcomeIgnored Outcome = "ignored"
	// OutcomeDuplicate: a verified delivery whose ID was seen recently.
	OutcomeDuplicate Outcome = "duplicate"
	// OutcomeUnauthorized: a delivery that failed verification.
	OutcomeUnauthorized Outcome = "unauthorized"
	// OutcomeTooLarge: a delivery over the source's `max_body`.
	OutcomeTooLarge Outcome = "too_large"
	// OutcomeRateLimited: a verified delivery over the source's budget.
	OutcomeRateLimited Outcome = "rate_limited"
	// OutcomeInvalid: a message dropped for violating the transport's rules
	// (REQ "Channel Notification Handling") or the envelope's.
	OutcomeInvalid Outcome = "invalid"
)

// Outcomes is every Outcome, in the order REQ "Trigger Metrics" lists them.
// A report ranges over it so every source reports every outcome, zeros
// included: a counter that is absent reads the same as one that never fired,
// and only the zero says "counted, and nothing happened".
var Outcomes = []Outcome{
	OutcomeFired, OutcomeIgnored, OutcomeDuplicate, OutcomeUnauthorized,
	OutcomeTooLarge, OutcomeRateLimited, OutcomeInvalid,
}

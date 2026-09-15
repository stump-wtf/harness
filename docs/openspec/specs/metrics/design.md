---
status: draft
date: 2026-09-15
implements: [adr-0019]
---

# Design: Prometheus Metrics

## Where the numbers come from

The supervisor already owns every fact this spec exposes. State, restart counts
and consecutive-failure accounting live in `Manager`/`Supervisor`; the run log
already records session starts and errors. The work is surfacing them, not
computing anything new.

Two families:

**Supervisor state** (`harness_harness_state`, `harness_consecutive_failures`,
`harness_session_active`) is read at scrape time from the Manager, under its
existing lock, and emitted for every declared harness. Reading at scrape time
rather than mirroring into gauges avoids the classic drift where one state
transition forgets to update a counter and the graph is permanently, plausibly
wrong.

**Event counters** (`harness_model_calls_total`, `harness_restarts_total`,
`harness_state_transitions_total`) increment inline where the event happens.

## Classifying a model error

The one piece of genuinely new logic. Adapters surface provider errors as text;
the classifier maps text to one of five classes at the point of observation.

It MUST be adapter-aware rather than a single shared regex list, because the
same condition reads differently per provider — a quota wall may be a 429 with a
reset timestamp, an HTTP 402, or a 200 carrying an error body.

Unmatched errors classify as `other` and MUST also increment
`harness_model_call_errors_unclassified_total{harness}`. A growing unclassified
count is the signal that a provider changed its error text, and without it the
classifier degrades silently into "everything is other" — which looks exactly
like a healthy `quota` count of zero.

That counter is the control for the classifier. Without it we would be trusting
an instrument with no way to notice it had gone blind.

## Listener

A separate `http.Server` from the SSH cockpit, defaulting to `127.0.0.1:9footnote`
(port in SPEC/config), started with the daemon and stopped with it.

Non-loopback bind without a token is refused **at startup**, not at request time.
A daemon that starts and quietly serves agent inventory to the network is worse
than one that refuses to start with a clear message.

## Testing

* A test asserting the 2026-09-14 shape: state `running` = 1, quota errors
  climbing, `last_successful_call_timestamp` frozen — the exact series an alert
  would read.
* A test that `last_successful_call_timestamp` is **omitted** for a
  never-succeeded harness, not zeroed. (A zero reads as 1970 and shows as
  56 years of staleness.)
* A test that every declared harness emits all four state values including zeros.
* A classifier table test per adapter, plus a test that an unrecognised error
  increments the unclassified counter as well as `class="other"`.
* A startup test: non-loopback bind with no token refuses to start.
* A cardinality test: harnesses beyond the cap collapse into `__other__`.

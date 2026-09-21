---
status: draft
date: 2026-09-15
implements: [adr-0020]
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

A separate `http.Server` from the SSH cockpit, defaulting to loopback
(`127.0.0.1`, per REQ-1; the default port is pinned in the `[server]` config),
started with the daemon and stopped with it.

Non-loopback bind without a token is refused **at startup**, not at request time.
A daemon that starts and quietly serves agent inventory to the network is worse
than one that refuses to start with a clear message.

## Implementation decisions

Recorded 2026-09-21 with the implementation (#356), where the spec left a
choice open.

### Seven supervisor states onto four

SPEC-0003 has seven states; REQ-2 exposes four. The mapping, in priority
order: core `failed` → `failed` (terminal; it wins over a lingering crash-loop
flag because operators alert on it by equality); the crash-loop flag or core
`degraded` → `flapping`; core `running` or `starting` → `running` (starting
lasts a moment, and anything else would blip every start); core `stopped`,
`stopping`, or `restarting` outside a crash loop → `stopped` (no process is up
at that instant). `harness_state_transitions_total{to}` uses the seven core
state names instead: a transition event carries no crash-loop flag, so mapping
it to four would miscount.

A harness held by its operating hours (SPEC-0012, `Snapshot.Held`) maps to
`stopped`, second in priority after `failed`. The gate shuts it down on
purpose, so crash-loop history caught mid-hold must not read as `flapping`.
The supervisor never holds a failed harness, so held-and-failed cannot arise;
were it to, `failed` is the state that needs a human. REQ-2's four values are
fixed, so held gets no value of its own. **Follow-up:** if operators need to
tell "held by hours" from "stopped by an operator", add a separate series such
as `harness_harness_held{harness}` (0|1) instead of extending the `state`
enum.

### Where model reachability comes from

The observer (`internal/observe`, #390) reads each agent's own transcript. A
tool call is a successful model call and an agent error mark a failed one; the
last-success timestamp is the item's own time and never moves backwards. Only
harnesses whose adapter writes a readable transcript (`claude-code`, `crush`,
`codex`) and that have a workdir get model series; for any other the values
cannot be computed and are omitted (REQ-6). At this agent-trace version only
crush records provider errors in its transcript.

Classification is by per-adapter tables, then shared provider shapes.
Overload (`529`, `503`) is `transport`, not `quota`: it is the provider's
capacity, with no reset time, and clears by itself. A context-window
rejection is recognised as `other` and does not count as unclassified; the
session guard's pattern list is shared so the two cannot disagree.

### Listener

The default port is 10229. Every port from 9100 to 9999 in Prometheus's
default port allocations list is taken, so any choice there collides with some
exporter.

The bearer token comes from a file named by `[server] metrics_token_file`, and
nowhere else. `harness.toml` holds no secrets (ADR-0008), `HARNESS_*` may not
carry credentials (SPEC-0010), and the daemon's environment is inherited by
every harness it spawns. A token file that is named but unreadable or empty is
also refused at startup. The refusal happens before Autostart, so no agent is
spawned under a daemon that is about to exit.

`metrics_listen = "off"` exists for shared hosts, where loopback is reachable by
every local account.

A bind failure on an address that is already in use is logged and does not stop
the daemon. The scraper already alerts on a down target, and exiting would stop
every supervised harness because one port was taken.

### Cardinality and absence

The cap grants label slots in config order at startup, and a slot is freed when
its harness disappears. Without that, scratchpad harnesses (ADR-0017), which
mint a new name for every run, would push every later harness into
`__other__`. Under `__other__`, counters and the state gauge are summed (so the
state value is a count of harnesses), consecutive failures take the maximum,
and last success and next run take the most recent and the soonest value.

Loss on the observer feed (a full subscriber buffer) and a feed that closes
early both increment `harness_metrics_collection_errors_total{collector="observer"}`.
Once the feed has closed, the model series are omitted rather than left frozen
at their last values.

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

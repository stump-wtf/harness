---
status: draft
date: 2026-09-15
implements: [adr-0020]
related: [adr-0003, adr-0005]
---

# SPEC-0013: Prometheus Metrics

## Overview

The Harness daemon exposes a Prometheus text-format endpoint at `GET /metrics`,
led by the metrics that distinguish **running** from **able to work** — a
distinction the supervisor cannot make and an operator always needs.

Harness is the only component that observes a model call. A queue service sees
claims; a gateway sees requests without knowing which agent gave up. If Harness
does not report model reachability per harness, nothing does.

## Requirements

### REQ-1: The endpoint

The daemon MUST serve `GET /metrics` in Prometheus text format
(`text/plain; version=0.0.4`) via `promhttp`.

It MUST bind to `127.0.0.1` by default. The bind address MUST be configurable in
`harness.toml` under `[server]`. Changing it MUST require a daemon restart, and
that MUST be stated in the config comment, because `harness reload` re-reads
harness definitions and not the listener.

Go collectors (`go_*`, `process_*`) MUST be registered: the daemon is long-lived
and its own memory growth is part of reading the rest.

Authentication is NOT required on a loopback bind. If the bind address is not
loopback, the daemon MUST require a bearer token and MUST refuse to start
otherwise — a non-loopback metrics listener without auth is a configuration
mistake, not a choice, and it is better refused at startup than discovered.

### REQ-2: Harness state

```text
harness_harness_state{harness,state}  gauge   1 for the current state, 0 otherwise
```

`state` is one of `running`, `stopped`, `failed`, `flapping`.

Every declared harness MUST report every state value, including zeros. An absent
series and a zero series are indistinguishable to an alert, and `failed` is
precisely the state an operator wants to alert on by equality.

```text
harness_restarts_total{harness}                  counter
harness_consecutive_failures{harness}            gauge
harness_state_transitions_total{harness,to}      counter
```

`harness_consecutive_failures` mirrors the daemon's own give-up accounting, so a
harness walking its budget toward terminal `failed` is visible before it arrives.

### REQ-3: Model reachability — the mandatory set

```text
harness_model_calls_total{harness,outcome}          counter  outcome: success|error
harness_model_call_errors_total{harness,class}      counter
harness_model_call_errors_unclassified_total{harness}  counter
harness_last_successful_call_timestamp{harness}     gauge    unix seconds
```

`class` MUST be one of `quota`, `auth`, `timeout`, `transport`, `other`.
Classification MUST happen where the error is observed, not by matching text in
an alert rule: provider error strings vary and change without notice.

An error matching no known class MUST increment
`harness_model_call_errors_unclassified_total` as well as `class="other"`.
That counter is the classifier's control: a rising unclassified count is the
signal that a provider changed its error wording, and without it the classifier
degrades silently into "everything is other", which looks exactly like a
healthy `quota` count of zero.

`quota` MUST be distinguished from every other class. It is not fixed by
restarting, it has a reset time outside our control, and it typically strikes
every harness sharing a provider simultaneously — an operational situation
unlike any other error.

`harness_last_successful_call_timestamp` MUST be emitted for every harness that
has ever succeeded in this daemon's lifetime, and MUST be omitted (not zeroed)
for one that has not. A zero would read as 1970 and produce an enormous, wrong
staleness on every dashboard.

Shell tool calls are also counted by the program they run, so an operator can
ask which programs the fleet's agents actually use and which of them keep
failing:

```text
harness_agent_program_calls_total{harness,program,outcome}  counter  outcome: ok|error
```

`program` comes from agent-trace's program list for the call, which names
executables only: an env assignment in front of a command, its arguments, and
shell builtins are never programs. `outcome` is taken from the tool result, not
from any error mark, so one call contributes to exactly one outcome.

A program that has never been counted MUST be absent rather than reported as
zero: the set of names is open-ended, and a zero for every executable on the
host would be the cardinality this family is here to avoid. Once a program has
been counted, both of its outcomes MUST be emitted, so `increase()` can see its
first error.

### REQ-4: Sessions and schedules

```text
harness_sessions_started_total{harness}            counter
harness_session_active{harness}                    gauge  0|1
harness_scheduled_runs_total{harness,outcome}      counter  outcome: success|failure
harness_scheduled_next_run_timestamp{harness}      gauge
```

`harness_session_active` separates "supervised and idle" from "supervised and
working". A doorbell-driven worker legitimately sits at `0`; a worker at `0`
while its queue has pending work is the interesting case, and correlating that
needs both this and the queue's own metrics.

### REQ-5: Cardinality

`harness` is an operator-chosen label, bounded in practice at single digits per
host. Implementations MUST cap distinct harness label values (default 50) and
MUST collapse the overflow into `harness="__other__"` rather than growing
unbounded.

The following MUST NOT be labels: session id, prompt text, model name as free
text, provider credentials, or any part of a harness's environment.

`program` is the one label here that a harness does not choose, so its values
are bounded where the count is taken, not at scrape time: a value MUST match
`^[a-z0-9][a-z0-9._+-]{0,31}$` (agent-trace also names uppercase executables, so
names are folded to lowercase before the match), and at most 64 distinct names
are kept per harness label. Anything failing the grammar — an env assignment, an
argument, a path, shell punctuation — and every name past the cap MUST be
counted as `program="other"`. Folding at count time is what keeps the guarantee:
a name that could carry a credential is then never stored anywhere a scrape could
print it, and the map holding the counts cannot grow without bound.

Model identity is deliberately excluded. A harness's model is configuration that
changes rarely; putting it in a label multiplies every series by it and makes a
model change look like a fleet of new harnesses appearing and old ones dying.

### REQ-6: Honest absence

A value the daemon cannot compute MUST be omitted rather than reported as zero.
Where collection fails, `harness_metrics_collection_errors_total{collector}` MUST
increment, so a broken collector is visible rather than flattening a graph into a
confident zero.

### REQ-7: Runtime memory

Beyond the Go collector's defaults, the endpoint MUST export the Go runtime's
memory breakdown and GC goal from `runtime/metrics`:

```text
go_memory_classes_*_bytes      gauge   every /memory/classes/ series
go_gc_heap_goal_bytes          gauge   /gc/heap/goal:bytes
```

The defaults the diagnosis depends on MUST stay exported: `go_goroutines`,
`go_memstats_heap_objects`, and `go_gc_gomemlimit_bytes` (the soft memory limit
the runtime holds, `math.MaxInt64` for none).

A process-level number (`process_resident_memory_bytes`) says the daemon grew;
it cannot say whether the growth is live heap or headroom the GC has not yet
reclaimed. `go_memory_classes_heap_objects_bytes` is the live heap, and
`go_memory_classes_heap_free_bytes` plus `go_memory_classes_heap_released_bytes`
are the headroom. A leak and a GC tuning question need different fixes, so the
scrape MUST let an operator tell them apart.

These families carry no labels, and the runtime's histogram metrics MUST NOT be
enabled by this requirement: each is a family of buckets, and REQ-5's budget is
for harnesses.

### REQ-8: Profiling listener

The daemon MAY serve `net/http/pprof` when `[daemon] pprof_addr` (or
`--pprof-addr`, `HARNESS_PPROF_ADDR`) names an address. It MUST be off by
default.

It MUST bind loopback only: an address whose host is not a loopback IP or
`localhost` MUST be refused, before any harness starts, with an error naming
the source that supplied it. The address actually bound MUST be checked again
after the bind, because `localhost` resolves at bind time. Unlike REQ-1, no
token unlocks a non-loopback bind: a heap profile names every allocation site,
a goroutine dump carries argument values, and `/debug/pprof/cmdline` is the
daemon's argv. An operator on another host reaches it through an SSH tunnel.

The handlers MUST be mounted on a mux of their own, never
`http.DefaultServeMux`, and MUST NOT be reachable on the metrics listener.

A bind failure (the port is taken) MUST be logged and MUST NOT stop the daemon,
as for REQ-1's listener. The listener MUST shut down with the daemon. Changing
the address needs a daemon restart.

## Scenarios

### Scenario: the 2026-09-14 outage, as it would have appeared

A provider's weekly quota empties. Four harnesses across two hosts fail every
call; all four processes stay up.

* `harness_harness_state{state="running"}` stays at `1` — correctly, the process is up
* `harness_model_call_errors_total{class="quota"}` climbs on all four at once
* `harness_last_successful_call_timestamp` stops advancing on all four
* the simultaneous edge across two hosts identifies it as provider-wide rather
  than four coincidental agent faults

Alert fires in minutes. The actual detection took a human asking a question the
next morning.

### Scenario: a harness walking into terminal failed

A misconfigured harness fails at launch repeatedly.

* `harness_consecutive_failures` climbs 1 → 5
* `harness_restarts_total` climbs in step
* `harness_harness_state{state="failed"}` flips to `1` and stays

The first two give warning; today the first signal is the third, which is
terminal and needs a human.

### Scenario: idle worker versus stuck worker

A doorbell-driven worker has no session.

* `harness_session_active` is `0` — normal and uninteresting alone
* combined with a queue reporting pending work and zero claims, it is the
  signature of a worker that is alive but not consuming

Neither service can conclude this alone; the pair can.

### Scenario: the 2026-09 emulator leak, as it would have appeared

Every spawn leaks a ~98 MiB terminal emulator. A scheduled harness runs every
few minutes; the daemon reaches 12 GB over days.

* `go_goroutines` rises by a constant step per run and never falls back
* `go_memory_classes_heap_objects_bytes` and `go_memstats_heap_objects` rise
  in step with `harness_sessions_started_total`
* `go_gc_heap_goal_bytes` tracks about twice the live heap, which is why the
  footprint doubles the leak

A ratio of goroutines or heap objects to runs that climbs is the leak
signature; a flat ratio with a high footprint is GC headroom. With
`pprof_addr` set, `go tool pprof -sample_index=inuse_space` on
`/debug/pprof/heap` names the allocation site in one command.

### Scenario: a profiler asked to listen on the network

An operator sets `pprof_addr = "0.0.0.0:6060"`.

* the config load (and any reload) fails with the key's line number
* the same value from `HARNESS_PPROF_ADDR` stops `harness daemon` before any
  harness starts, naming the variable
* nothing is bound, so nothing serves heap dumps to the network

## Out of Scope

* Token or cost accounting. The gateway owns spend.
* Per-session tracing. Cairn owns trace capture.
* Prompt or output content. Never a metric.

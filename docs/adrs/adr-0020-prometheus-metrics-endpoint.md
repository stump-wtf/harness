---
status: accepted
date: 2026-09-15
decision-makers: [joestump]
extends: [ADR-0003, ADR-0005]
related: [ADR-0013]
---

# ADR-0020: Harness Exposes Prometheus Metrics, Led by Model Reachability

## Context and Problem Statement

On 2026-09-14 a model provider's weekly quota emptied. Four supervised agents
across two hosts began failing every model call with HTTP 429, and kept failing
for roughly twenty hours.

`harness list` reported all four as **running** the entire time.

That report was not wrong. The processes *were* running. The restart policy *was*
behaving correctly. The supervisor was doing exactly its job — and its job is to
watch a process, which is a different property from whether that process can do
any work. The only place the truth appeared was `harness logs <name>`, which
nothing reads routinely and no alert can watch.

Harness has no metrics surface: no `/metrics`, no Prometheus dependency, no
counters. The operator's fleet already runs VictoriaMetrics and Grafana.

The generalisable finding, which this ADR exists to act on: **a supervisor that
watches processes cannot see a provider refusing them.** Any health check built
on "is it running" reports green through this entire class of outage.

## Decision Drivers

* The failure that cost twenty hours was *green on the only surface that
  reported it*. Whatever we expose must make "running but unable to work" a
  distinct, alertable state from both "running" and "failed".
* `failed` is terminal and needs a human. The transition into it is exactly the
  moment an operator wants to know, and it currently arrives silently.
* Restart churn is the leading indicator. A harness restarting repeatedly is
  either crash-looping or burning a metered budget on launches that fail at
  stream-open; both are worth a graph before they reach `failed`.
* Harness is the component that *knows* a model call failed. Switchboard sees
  claims, not workers — it structurally cannot report this. If Harness does not,
  nothing does.
* The daemon runs as a user-level service on machines that are not public. Its
  exposure decision differs from a public-facing service's and should be made
  explicitly rather than inherited.

## Considered Options

* Option 1 — Status quo: `harness list` plus reading logs
* Option 2 — Make `harness list` show a degraded state, and nothing else
* Option 3 — Log-derived metrics
* Option 4 — `GET /metrics` in Prometheus text format

## Decision Outcome

Chosen option: **Option 4 — `GET /metrics` in Prometheus text format**, because
it is the only option that makes "running but unable to work" alertable and
gives it history.

The Harness daemon exposes **`GET /metrics`** in Prometheus text format via
`prometheus/client_golang`'s `promhttp`, on its own listener bound by default to
loopback, with the bind address configurable in `harness.toml`.

The metric set is led by **model reachability**, and the distinction the
2026-09-14 outage needed is mandatory:

```text
harness_harness_state{harness,state}              gauge   running|failed|stopped|flapping
harness_model_calls_total{harness,outcome}        counter success|error
harness_model_call_errors_total{harness,class}    counter quota|auth|timeout|transport|other
harness_last_successful_call_timestamp{harness}   gauge
```

`harness_last_successful_call_timestamp` is the one that closes the gap. A
harness that is `running` with a last-success timestamp receding into the past is
the exact shape of the outage, and it is trivially alertable:

```promql
time() - harness_last_successful_call_timestamp > 900
  and harness_harness_state{state="running"} == 1
```

"Running, and hasn't done anything in fifteen minutes" is a state the supervisor
could never express and an operator always wanted.

Error **class** matters more than error text. `quota` is a distinct operational
situation from `auth` or `transport`: it is not fixed by restarting, it has a
reset time, and it usually affects every harness sharing a provider at once.
Classifying at the point of failure keeps that distinction out of alert regexes.

Binding to loopback by default is deliberate. Harness runs on personal machines
as well as agent boxes, and an always-on listener exposing which agents exist and
how they are faring should be opted into per host, not shipped on.

SPEC-0013 defines names, labels and types.

### Consequences

* Good, because "running but unable to work" becomes alertable, and the entry into
  terminal `failed` becomes a graph with a timestamp rather than a discovery.
* Good, because a fleet-wide provider outage is visible as a simultaneous edge across
  every harness, which distinguishes it from one bad agent.
* Good, because restart churn gets history, so a harness quietly consuming a metered
  budget on failing launches is visible before the budget is gone.
* Bad, because it adds a new dependency and a listener that did not previously exist.
* Bad, because harness names are operator-chosen labels. They are bounded in practice
  (single digits per host) but the cap in SPEC-0013 exists because "in practice"
  is not a guarantee.
* Neutral, because loopback-by-default means remote scraping requires a deliberate
  config change per host, which is the intent.

### Confirmation

* `GET /metrics` on the configured `[server] metrics_listen` address (loopback
  by default) returns Prometheus text carrying the families SPEC-0013 names,
  including `harness_last_successful_call_timestamp`.
* A harness whose model calls fail with HTTP 429 while its process stays up
  shows `harness_harness_state{state="running"} == 1` and a growing
  `time() - harness_last_successful_call_timestamp`.

## Pros and Cons of the Options

### Option 1 — Status quo: `harness list` plus reading logs

* Good, because it needs no new code or listener.
* Bad, because that is the configuration that produced the blind spot.

### Option 2 — A degraded state in `harness list`, and nothing else

* Good, because it gives a human the answer at a glance, and it is tracked
  separately as a complement.
* Bad, because it is a human-facing surface: it cannot alert and it has no
  history, so it answers "is it broken now" and never "when did it start".

### Option 3 — Log-derived metrics

* Good, because the daemon would need no new listener.
* Bad, because it infers a state machine from error text, which is fragile
  precisely where provider error strings vary.

### Option 4 — `GET /metrics` in Prometheus text format

* Good, because the operator's existing Prometheus-compatible stack can scrape,
  graph and alert on it.
* Good, because the daemon classifies errors at the point of failure, so alerts
  need no regexes over error text.
* Bad, because it adds a dependency and a listener.

## More Information

* The incident: four supervised agents down ~20h on a provider quota, reported
  `running` throughout.
* SPEC-0013 (metrics).
* Related and complementary: making `harness list` itself distinguish a degraded
  harness. Metrics give history and alerting; the CLI gives the human answer at a
  glance. Neither replaces the other.

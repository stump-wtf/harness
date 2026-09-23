---
title: "Metrics"
sidebar_position: 8
---

# Metrics

The daemon serves Prometheus metrics at `GET /metrics`, on
`127.0.0.1:10229` by default. They lead with the thing `harness list` cannot
tell you: whether a running agent can actually get a model to answer.

On 2026-09-14 a provider's weekly quota ran out. Four agents failed every model
call for about twenty hours, and `harness list` showed all four as `running`
the whole time. That was accurate, because the processes were up. The
supervisor watches processes, and a process can stay up while the provider
refuses every request it sends. The only record of the refusals was in each
agent's own transcript. The daemon now reads those transcripts and publishes
what it finds next to the process state, so one alert can check both.

## Scraping it

Loopback needs no auth:

```yaml
# prometheus.yml (vmagent accepts the same block)
scrape_configs:
  - job_name: harness
    static_configs:
      - targets: ["127.0.0.1:10229"]
```

To scrape from another machine, bind a non-loopback address **and** give the
daemon a bearer token. If a non-loopback bind has no token, the daemon refuses
to start, before it launches any harness:

```toml
[server]
metrics_listen = "0.0.0.0:10229"
metrics_token_file = "~/.config/harness/metrics.token"   # chmod 600
```

```yaml
scrape_configs:
  - job_name: harness
    authorization:
      type: Bearer
      credentials_file: /etc/prometheus/harness-metrics.token
    static_configs:
      - targets: ["agentbox.lan:10229"]
```

The token has to come from a file, for three reasons:

- `harness.toml` holds no secrets (ADR-0008).
- `HARNESS_*` variables may not carry credentials (SPEC-0010).
- Every harness inherits the daemon's environment when it is spawned, so a
  token set as a daemon environment variable would reach every agent the daemon
  runs.

A file is also what Vault Agent or OpenBao templates write out. If the file is
readable by group or others, the daemon logs a warning.

`metrics_listen = "off"` turns the listener off. You may want that on a shared
machine, because any local account can connect to `127.0.0.1`.

Changing either key **requires a daemon restart**. `harness reload` re-reads
harness definitions but does not rebind listeners. The token file is also read
once, at startup, so rotating the token means rewriting the file and then
restarting the daemon.

If the port is already in use, the daemon logs an error and keeps supervising
without the listener. Your scraper then sees the target as down (`up == 0`),
and it already alerts on that. Exiting would stop every harness on the host
just because one port was taken.

## Alerts

"Running, but no model call has succeeded in fifteen minutes." This is the
shape of the 2026-09-14 outage:

```promql
time() - harness_last_successful_call_timestamp > 900
  and on(instance, harness) harness_harness_state{state="running"} == 1
```

The `on(instance, harness)` clause is required. The state series has an extra
`state` label, and a bare `and` matches only series whose label sets are
identical, so without the clause this expression would never fire.

```promql
# A quota wall. It usually hits every harness that shares a provider at the same time.
sum by (instance, harness) (increase(harness_model_call_errors_total{class="quota"}[10m])) > 0

# Terminal failed state: the supervisor gave up, and only a person can restart the harness.
harness_harness_state{state="failed"} == 1

# Approaching failed: consecutive failures climb before the harness gets there.
harness_consecutive_failures >= 3

# Restart churn: a crash loop, or launches that burn a metered budget.
increase(harness_restarts_total[1h]) > 10

# The error classifier no longer recognises a provider's errors (the wording probably changed).
increase(harness_model_call_errors_unclassified_total[1h]) > 0

# The daemon's own collection is broken or losing events.
increase(harness_metrics_collection_errors_total[15m]) > 0

# The endpoint is unreachable: the daemon is down, or the port could not bind.
up{job="harness"} == 0
```

## What is exported

| Series | Type | Notes |
|---|---|---|
| `harness_harness_state{harness,state}` | gauge | `running`, `stopped`, `failed`, `flapping`. Every declared harness reports all four, with 1 for its current state and 0 for the others. |
| `harness_restarts_total{harness}` | counter | The RESTARTS column of `harness list`. |
| `harness_consecutive_failures{harness}` | gauge | The supervisor's give-up count. Once it exceeds the restart budget, the harness moves to `failed`. |
| `harness_state_transitions_total{harness,to}` | counter | One series per supervisor state (`to` has 7 values), starting at zero. |
| `harness_model_calls_total{harness,outcome}` | counter | `success` counts tool calls. `error` counts agent error marks. |
| `harness_model_call_errors_total{harness,class}` | counter | `quota`, `auth`, `timeout`, `transport`, `other`. Crush harnesses only, for now; see below. |
| `harness_model_call_errors_unclassified_total{harness}` | counter | Errors that matched no known pattern. Each one is also counted as `class="other"`. |
| `harness_last_successful_call_timestamp{harness}` | gauge | Unix seconds. **Absent** until the harness's first success in this daemon's lifetime. It is never reported as zero, because zero would read as 1970. |
| `harness_sessions_started_total{harness}` | counter | Agent sessions the daemon has seen become active. |
| `harness_session_active{harness}` | gauge | 1 when the process is up and the agent wrote to a session in the last 10 minutes. |
| `harness_scheduled_runs_total{harness,outcome}` | counter | Scheduled harnesses only. `success`, or `failure` (a run that failed or timed out). Skipped, missed, cancelled and interrupted runs are not counted. |
| `harness_scheduled_next_run_timestamp{harness}` | gauge | Scheduled harnesses only. Absent when there is no next window. |
| `harness_metrics_collection_errors_total{collector}` | counter | `supervisor`, `schedule`, `observer`, `lifecycle`. `observer` and `lifecycle` also count events the collector lost because it fell behind, so the matching counters read low. |
| `harness_metrics_harnesses_overflowed` | gauge | How many harnesses were folded into `__other__`. |
| `harness_observer_*` | mixed | Health of the transcript reader: delivered and dropped events, ambiguous and unattributed items, parse errors, scan errors, sessions tracked. |
| `go_*`, `process_*` | | The daemon's own runtime. |

### What "running" means

The supervisor has seven states and the metric has four:

| Supervisor state | `state` label |
|---|---|
| `failed` | `failed` |
| held by its `operating_hours` (any state other than `failed`) | `stopped` |
| `degraded`, or any state while the crash-loop flag is set | `flapping` |
| `running`, `starting` | `running` |
| `stopped`, `stopping`, `restarting` (not in a crash loop) | `stopped` |

A harness held outside its operating hours is down on purpose, so it reads
`stopped`, never `failed` or `flapping`. The metric does not tell a held
harness apart from one an operator stopped.

### Where the model-call numbers come from

The daemon reads the transcript each agent writes: Claude Code's JSONL, Crush's
SQLite store, or Codex's session files. It counts a **tool call** as a
successful model call, because the model answered with work. It counts an
**error mark** as a failed one. A turn that ends in plain text with no tool
call is not counted.

Only a harness whose adapter writes a readable transcript (`claude-code`,
`crush`, `codex`) **and** that has a `workdir` gets model-call series. A
`generic` harness, or an agent harness with no workdir, has none. The daemon
omits values it cannot compute rather than reporting a zero, which would look
like a healthy, idle agent.

Crush records provider errors in its transcript as a failed turn. Claude Code
records them as flagged API-error records, which appear once Harness is built
against an agent-trace version that reads them; the classifier already knows
their shape (`rate_limit (429): …`, `server_error: …`). Codex successes are
counted, but its provider errors do not appear yet.

Until an adapter's errors do appear, its harnesses have no error-side series:
`harness_model_calls_total{outcome="error"}`, `harness_model_call_errors_total`
and `harness_model_call_errors_unclassified_total` are **absent** for
`claude-code` and `codex` harnesses today, not zero. A zero would say "no quota
errors" through the very outage the quota alert exists to catch. For those
harnesses, the staleness alert on `harness_last_successful_call_timestamp` is
the one that fires.

### Error classes

The daemon classifies each error when it reads it, so alert rules never need to
match provider wording:

- **quota**: rate limits, spent weekly or monthly allowances, credit balances
  (`429`, `402`, `rate_limit_error`, `insufficient_quota`, "usage limit").
  Restarting does not fix these.
- **auth**: missing, invalid or expired credentials (`401`, `403`, "invalid api
  key", `authentication_error`).
- **timeout**: the provider accepted the request but took too long (`408`,
  `504`, "deadline exceeded").
- **transport**: the provider could not be reached or could not serve the
  request: refused or reset connections, DNS and TLS errors, `500`, `502`,
  `503`, and provider overload (`529 overloaded_error`). Overload counts as
  transport, not quota. It reflects the provider's capacity, has no reset
  time, and clears on its own.
- **other**: everything else. This includes context-window rejections. The
  daemon recognises those (its session guard rotates a Crush session that is
  wedged on them), so they are counted as `other` but never as unclassified.

If the unclassified counter starts rising, a provider has probably changed its
error wording.

### Cardinality

The `harness` label is capped at 50 distinct values. Harnesses beyond the cap
share `harness="__other__"`, where counters add up and `harness_harness_state`
counts how many overflow harnesses are in each state. When a harness is
removed, its slot is freed. Session IDs, prompts, model names, credentials and
environment values never appear as labels.

---
status: proposed
date: 2026-09-21
decision-makers: Joe Stump
extends: [adr-0008, adr-0011]
related: [adr-0007, adr-0020]
---

# ADR-0022: Harness Exports Agent Telemetry as OTLP Logs, OTLP Traces and a Local JSONL Stream

## Context and Problem Statement

Harness is increasingly run as a production service: a daemon on an agent box
supervising workers that drain queues around the clock, operated by someone who
already has a log pipeline and a trace backend and expects every service on the
box to feed them. For those operators the useful record of what an agent *did*
— which tools it called, which calls failed, when the model refused a turn —
lives only in the agent CLI's own session files. Harness can read those files
(ADR-0011, via agent-trace), but nothing leaves the process.

The pieces for export exist and are wired to nothing:

* `internal/otlpexport` converts an agent-trace `otel.Trace` into OTLP/HTTP JSON
  and POSTs it. It has tests and no callers.
* `[daemon] otel_endpoint` is parsed into `core.DaemonConfig.OTelEndpoint` and
  read by nothing. `docs/usage/configuration.md` says so in as many words.
* agent-trace's `otel.BuildTrace` produces a span tree per session, with trace
  and span IDs derived deterministically from the session.

What is missing is a *continuous* source. Everything that reads transcripts today
does so on demand — `harness logs`, the TUI's activity view — and the one
streaming primitive, agent-trace's `tail.Watcher`, parks marks until a tool call
arrives to carry them. During a provider outage that is exactly backwards: the
agent produces error marks and no tool calls, so the Watcher emits nothing at the
moment an operator most needs to see something. ADR-0020 exists because a quota
outage ran twenty hours green; a log export built on the Watcher would have been
silent through the same twenty hours.

The daemon-side agent-event observer (`internal/observe`) closes
that gap: it polls each supervised harness's sessions, attributes every item to a
harness with the runtrace rules, and delivers tool events and marks on their own,
in per-session order, at most once, with no history replay. This ADR decides how
Harness turns that stream into telemetry an operator can ship.

## Decision Drivers

* **Consent.** Reading a transcript locally is not consent to publish it.
  Nothing leaves the process, and nothing is written to a new file, unless
  the operator named a destination *and* the harness is opted in.
* **Secrets never reach config or the wire.** A collector credential lives in
  the environment or an env file, never in `harness.toml` (ADR-0008). Transcript
  text carries credentials agents typed; every byte is redacted before it leaves
  the process or reaches disk.
* **Never block the supervisor.** A slow or dead collector must cost dropped
  telemetry, counted, and nothing else (ADR-0007's backpressure rule).
* **Speak the operator's existing stack.** OTLP is what collectors, Loki, Tempo,
  Honeycomb and Grafana Cloud ingest; the standard `OTEL_EXPORTER_OTLP_*`
  variables are how operators already configure exporters.
* **A zero-network path.** Many operators ship logs by tailing files (Vector,
  Fluent Bit, Promtail/Alloy) and will not open an egress for one daemon.
* **Laptop users pay nothing.** With no telemetry configured, the feature costs
  no network, no files and no measurable CPU.

## Considered Options

### Where the data comes from

* **Post-hoc harvest when a run finishes** (the `cairnexport` shape). Rejected: a
  resident harness never finishes, so a production worker would export nothing
  until it was restarted, and nothing at all while it was failing.
* **agent-trace's `tail.Watcher` directly.** Rejected: it parks marks until a
  tool event arrives, so a provider outage — error marks, no tool calls — is
  invisible. That is the failure this work most needs to show.
* **Each agent CLI's native telemetry** (Claude Code, for one, can emit its own
  OTel). Rejected as the mechanism: per-agent, inconsistent across adapters,
  absent for most of them, and not attributed to a *harness*. Operators who want
  it can still configure it per harness in `env_file`; it is complementary.
* **The daemon-side observer (`internal/observe`).** Chosen. One continuous,
  harness-attributed stream of tool events *and* marks, shared with the metrics
  work (ADR-0020) so both read the same facts.

### Transport

* **The OpenTelemetry Go SDK (`go.opentelemetry.io/otel` + OTLP exporters).**
  Correct by construction, but a large dependency tree (gRPC, protobuf, the SDK's
  own batching and resource detection) for a daemon that already has its own
  span model from agent-trace and would use a fraction of the SDK. The logs SDK
  has also been the least stable part of the Go project. Rejected.
* **OTLP/gRPC.** Adds the gRPC stack for no capability OTLP/HTTP lacks; every
  collector serves both. Rejected; out of scope.
* **Hand-rolled OTLP/HTTP JSON, extending `internal/otlpexport`.** Chosen. The
  OTLP JSON encoding is a stable part of the specification (a direct protobuf
  mapping with fixed field names), `otlpexport` already implements the trace
  half with tests, and the logs half is the same envelope with `resourceLogs` in
  place of `resourceSpans`. The cost is that batching, retry and backoff are ours
  to write and test — which we would have had to configure and test anyway to
  meet the never-block driver.

### How an operator "slurps" the stream

* **OTLP only.** Excludes the file-tailing operators.
* **A local file only.** Excludes operators with a collector, and gives up trace
  structure.
* **OTLP logs + OTLP traces + an optional local JSONL file.** Chosen. Logs carry
  every item as a searchable record; traces carry the session's shape; the JSONL
  file is the zero-network path and carries exactly the log attributes, so a file
  shipper and a collector see the same fields.

### Where configuration and credentials live

* **Environment variables only.** Standard, but an inherited
  `OTEL_EXPORTER_OTLP_ENDPOINT` — set in a login shell for some other tool —
  would silently start publishing transcripts. Consent cannot be implicit.
* **`harness.toml` only.** Puts the collector credential in the config file,
  against ADR-0008.
* **A `[telemetry]` table that grants consent and sets policy; the standard OTel
  variables supply endpoints and headers.** Chosen. Signals are enabled only in
  `harness.toml`; endpoints may come from either place, env winning; headers come
  only from the environment or a `[telemetry] env_file`. `harness.toml` rejects a
  `headers` key outright rather than trying to tell a tenant ID from a token.

### Which harnesses contribute

* **Reuse `harvest_trajectory`.** Rejected: that key consents to the local MCP
  facade reading a transcript — another agent on the same machine. Publication
  to an external collector is a different audience, and publication consent is precisely the
  rule that one does not imply the other.
* **Fleet-wide only.** Cannot exclude the one harness that handles something
  sensitive.
* **Per-harness only.** Tedious for a forty-harness production box.
* **Per-harness `export_telemetry`, plus a fleet-wide `[telemetry] export_all`.**
  Chosen. Both are explicit. An explicit per-harness `false` beats `export_all`,
  so an operator can export everything but one harness. A project file may opt
  its harnesses *out* but not *in*: a cloned repository does not get to decide
  that its transcripts are published to the operator's collector.

### The dead `[daemon] otel_endpoint`

* **Alias it to the traces endpoint, with a deprecation warning.** Rejected. The
  key never did anything and the docs said so; configs carry it (the test
  fixtures point it at a public Cairn host). Aliasing would turn an inert,
  documented-as-inert line into live publication of agent transcripts on
  upgrade — the one outcome the consent driver forbids.
* **Reject it with a migration error.** Chosen, per the delete-not-deprecate
  convention in `internal/config/config.go` ("Removed keys"). The error names
  `[telemetry]` and the env variables, so the fix is one edit.

## Decision Outcome

Harness exports the observer's stream as three independent, opt-in signals:

1. **OTLP logs.** Every observed item — tool event or mark — becomes one
   LogRecord, POSTed as OTLP/HTTP JSON to the logs endpoint. The body is the
   item's human summary, redacted and length-capped. Attributes use OTel semantic
   conventions where they exist (`service.name`, `service.version`, `host.name`
   as resource attributes) and a documented `harness.*` / `agent.*` namespace
   otherwise, sharing agent-trace's span attribute names so a log query and a
   trace query use one vocabulary. Error marks are `ERROR`; a tool call that
   returned an error is `WARN` (a failing test run is the agent working, not the
   agent broken); everything else is `INFO`. Each record carries the trace ID of
   its session and, where the item maps to a span, that span's ID.
2. **OTLP traces.** Per session, agent-trace's `otel.BuildTrace` builds the span
   tree and `internal/otlpexport` sends it, when the session goes idle, when its
   harness exits, and at daemon shutdown. Span IDs are keyed to the *item* (its
   session-positional sequence) rather than to agent-trace's per-build counter,
   because the observer does not replay history: after a daemon restart the first
   delivered item would otherwise be span zero again and collide with a span the
   previous daemon already sent. With item-keyed IDs, a session that resumes after
   an idle export sends only spans it has not sent, and a restart cannot produce
   a duplicate.
3. **A local JSONL events file.** One JSON object per observed item, carrying the
   log record's fields under the same names, rotated by size. Zero network.

All three sit behind the same gate: a destination configured in `[telemetry]`
and a harness opted in by `export_telemetry = true` or `export_all = true`. All
three drain bounded queues fed by non-blocking observer subscriptions; overflow
drops the oldest item and counts it. The OTLP senders batch by size and interval,
retry 429/502/503/504 and network errors with exponential backoff, full jitter and
`Retry-After`, and give a batch up after a bounded time. Their counters are
exposed through a `Stats()` API so the metrics endpoint (ADR-0020) can publish
`harness_telemetry_*` series once both land.

User-message notes are prompt text. They are exported by default — redacted and
capped — because for an operator the prompt is what makes a run legible, and the
destination is the operator's own. `[telemetry] omit_prompts = true` replaces
them (and the prompt-derived session title) with a placeholder for operators
whose prompts carry customer data.

`[daemon] otel_endpoint` is removed and rejected with a migration error.

SPEC-0015 defines the configuration surface, the record and span mapping, the
queueing and retry behaviour, and the self-telemetry contract.

### Consequences

* Good: an operator's existing log and trace stack sees every harness's agent
  activity, attributed to the harness, with no Harness-specific tooling.
* Good: a provider outage is a burst of `ERROR` records per affected harness in
  the log backend — the shape ADR-0020 makes alertable as a metric, now also
  searchable with the provider's own message. This holds for adapters that
  surface API errors as marks: Crush does; Claude Code does not at the pinned
  agent-trace version, where the outage shows instead as its records stopping.
* Good: consent stays explicit at both levels, and the zero-config case — a
  laptop — is unchanged: no sockets, no files.
* Good: `internal/otlpexport` and the observer each gain a real caller; the dead
  config key stops misleading people.
* Bad: batching, retry, backoff and partial-success handling are ours to own
  and test, where an SDK would have supplied them.
* Bad: span IDs deliberately diverge from agent-trace's counter-derived IDs
  until agent-trace keys them by item itself. A second system correlating by
  agent-trace's own IDs (a Cairn trace of the same session) joins on trace ID,
  not span ID.
* Bad: removing `otel_endpoint` fails daemon startup for anyone who set it. The
  error says exactly what to do, and the alternative was worse.
* Neutral: a span's end time is frozen when it is first exported; a turn that
  resumes after an idle export keeps its first-export end time. The idle gap is
  real, and a zero-length final span is more honest than one stretched across it.
* Neutral: supervisor lifecycle events (state changes, restarts, give-up) are not
  in this stream; the daemon's own log already carries them to journald or
  `HARNESS_LOG_FILE`, which shippers already collect.

## More Information

* SPEC-0015 (telemetry export) and its design notes.
* The daemon-side agent-event observer (`internal/observe`) this builds on.
* ADR-0020 / SPEC-0013 — the metrics endpoint that publishes the self-telemetry.
* ADR-0008 — secrets stay out of `harness.toml` and out of our output.
* The OTLP specification's JSON encoding and exporter environment variables are
  the external contracts SPEC-0015 follows.

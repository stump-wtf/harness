---
status: draft
date: 2026-09-21
implements: [adr-0021]
related: [adr-0007, adr-0008, adr-0011, adr-0020]
---

# SPEC-0014: Telemetry Export

## Overview

The Harness daemon exports what its supervised agents do — every tool call and
every mark (user message, compaction, subagent, error) that the daemon-side
observer attributes to a harness — as three independent, opt-in signals:

* **OTLP logs**: one LogRecord per observed item, POSTed as OTLP/HTTP JSON.
* **OTLP traces**: one trace per agent session, built by agent-trace's
  `otel.BuildTrace` and POSTed as OTLP/HTTP JSON.
* **A local JSONL events file**: one JSON object per observed item, for
  file-tailing shippers. No network.

The source is the observer in `internal/observe` (issue #390). This spec
consumes its `Event` stream and its guarantees — marks delivered on their own,
per-session sequence order, at most once per daemon lifetime, no history
replay, ambiguous sessions withheld — and does not restate them.

The audience is an operator running Harness as a production service who already
has a collector, a log store or a trace backend. The zero-configuration case is
the opposite audience, and it MUST stay exactly as it is today.

## Requirements

### REQ-1: Nothing is exported without two explicit consents

Export requires both a **destination** and a **contributing harness**. Neither
implies the other, and reading a transcript locally (`harness logs`, the TUI,
`harvest_trajectory`) implies neither (issue #94).

A destination is configured when, in the global `harness.toml`, at least one of
`[telemetry] logs = true`, `[telemetry] traces = true`, or a non-empty
`[telemetry] events_file` is set. Environment variables alone MUST NOT enable any
signal: an `OTEL_EXPORTER_OTLP_ENDPOINT` inherited from a login shell for some
other tool is not consent to publish agent transcripts. When `OTEL_EXPORTER_OTLP_*`
variables are present but no OTLP signal is enabled, the daemon SHOULD log once at
info level that they were ignored and which key enables export.

With no destination configured, the daemon MUST NOT subscribe to the observer
for telemetry, MUST NOT start the observer on telemetry's account, MUST NOT open
or create any telemetry file, and MUST NOT make any network connection for
telemetry.

A harness contributes when its effective opt-in is true, resolved in this order:

| per-harness `export_telemetry` | `[telemetry] export_all` | contributes |
|---|---|---|
| `false` | any | no |
| `true` | any | yes |
| unset | `true` | yes |
| unset | `false` (default) | no |

An explicit per-harness `false` MUST beat `export_all`, so an operator can export
the fleet but one harness. A harness with no definition entry for the key —
including an ephemeral scratchpad harness (ADR-0017) — follows `export_all`.

The gate MUST be evaluated per item, at the moment the item is taken from the
observer, against the harness's *current* definition. A `harness reload` that
changes `export_telemetry` therefore takes effect for every item delivered after
the reload, without a restart. An item from a non-contributing harness MUST be
discarded before redaction, queueing or any sink sees it, and MUST NOT be counted
as dropped.

The gate applies identically to all three signals. The events file is a new
persistent copy of transcript content that exists to be shipped; it is a
destination, not a local read.

### REQ-2: The `[telemetry]` table

Configuration lives in a `[telemetry]` table in the **global** `harness.toml`.

```toml
[telemetry]
# Consent: nothing is exported unless at least one destination is on.
logs   = false                 # OTLP logs signal
traces = false                 # OTLP traces signal
events_file = ""               # local JSONL sink; empty = off

# Scope: which harnesses contribute (see also per-harness export_telemetry).
export_all   = false
omit_prompts = false           # replace prompt text with a placeholder

# Destination. Endpoints and headers may also come from the OTel env vars.
endpoint    = ""               # OTLP/HTTP base URL; /v1/logs and /v1/traces are appended
env_file    = ""               # file supplying OTEL_EXPORTER_OTLP_* (credentials go here)
compression = "none"           # "none" | "gzip"
timeout     = "10s"            # per request

# Tuning. Defaults are sized for a single host; most operators change none of them.
queue_size     = 2048          # per signal, in that signal's unit (records, spans, lines)
batch_size     = 512           # max records/spans per request, lines per write
batch_interval = "5s"          # flush a partial batch after this long
idle_flush     = "5m"          # export a session's trace after this long without items
shutdown_timeout = "5s"        # total budget for the shutdown flush

events_file_max_mb = 100       # rotate the events file at this size
events_file_keep   = 5         # rotated files kept (events.jsonl.1 … .5)
```

And per harness:

```toml
[harness.worker]
export_telemetry = true        # unset = follow [telemetry] export_all
```

Validation, all at config load, all errors citing the offending key and line:

* Durations MUST parse with `time.ParseDuration` and MUST be positive.
  `idle_flush` MUST be at least `batch_interval`.
* `queue_size`, `batch_size`, `events_file_max_mb` and `events_file_keep` MUST be
  positive integers, and `batch_size` MUST NOT exceed `queue_size`.
* `compression` MUST be `none` or `gzip`.
* `endpoint`, when set, MUST be an absolute `http` or `https` URL with a host and
  MUST NOT carry userinfo. `https://user:token@collector` is a credential in
  `harness.toml`; it MUST be rejected with an error pointing at `env_file`.
* A `headers` key in `[telemetry]` MUST be rejected with an error directing the
  operator to `OTEL_EXPORTER_OTLP_HEADERS` or `env_file`. Headers are how OTLP
  carries credentials; rather than guess which header values are secret (a
  tenant ID is not, an API key is), `harness.toml` carries none.
* `events_file` and `env_file` expand a leading `~` and resolve a relative path
  against the config file's own directory, as other config paths do.

Scope rules:

* A project `harness.toml` MUST reject a `[telemetry]` table, as it rejects
  `[server]` and `[profile.*]` — a fleet-wide concern.
* A `harness_d` drop-in file MUST reject a `[telemetry]` table (drop-ins carry
  `[harness.*]` only) and MAY set `export_telemetry` either way.
* A project file MAY set `export_telemetry = false` and MUST reject
  `export_telemetry = true` with an error saying the opt-in belongs in the
  daemon's global config. Opting out narrows publication and is always safe;
  opting in widens it, and a cloned repository does not get to decide that its
  transcripts reach the operator's collector.

Changes to the `[telemetry]` table MUST take effect only on daemon restart, and
the config reference MUST say so: the exporters hold connections, queues and an
open file. A `harness reload` that detects a changed `[telemetry]` table MUST log
a warning that the change is pending a restart. Per-harness `export_telemetry`
changes take effect on reload (REQ-1).

### REQ-3: Endpoints, headers and the OTel environment

The daemon MUST honour the standard OTLP exporter variables, read from the
daemon's process environment and from `[telemetry] env_file`:

| Variable | Meaning |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | base URL; `/v1/logs` and `/v1/traces` are appended |
| `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT` | full logs URL, used as-is |
| `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | full traces URL, used as-is |
| `OTEL_EXPORTER_OTLP_HEADERS` | `k=v,k2=v2`, values URL-decoded |
| `OTEL_EXPORTER_OTLP_LOGS_HEADERS`, `OTEL_EXPORTER_OTLP_TRACES_HEADERS` | per-signal headers |
| `OTEL_EXPORTER_OTLP_COMPRESSION` (+ `_LOGS_` / `_TRACES_`) | `none` or `gzip` |
| `OTEL_EXPORTER_OTLP_TIMEOUT` (+ `_LOGS_` / `_TRACES_`) | milliseconds |
| `OTEL_RESOURCE_ATTRIBUTES` | extra resource attributes (SHOULD) |
| `OTEL_SDK_DISABLED` | `true` disables both OTLP signals (SHOULD) |

Precedence for each OTLP setting, per signal, highest first:

```
signal-specific env var  >  generic env var  >  harness.toml [telemetry]  >  default
```

where "env var" means the process environment first, then `env_file`. This
matches the OTel exporter convention (signal-specific beats generic) and puts the
deployment system — a systemd `EnvironmentFile`, a container secret — above the
checked-in file. An exported-but-empty variable MUST be treated as unset, as
`HARNESS_*` variables are.

Headers MUST be merged: generic headers first, then signal-specific headers
overriding any key of the same name. Headers come **only** from the environment
or `env_file` (REQ-2).

`[telemetry] env_file` MUST be read with the same parser as a harness `env_file`.
Only keys named in the table above are honoured from it; every other key MUST be
ignored, and none MUST be exported into the daemon's process environment — so a
credential supplied this way is never inherited by a supervised harness. The
config reference SHOULD recommend `env_file` over the process environment for
exactly that reason. A missing or unreadable `env_file` while an OTLP signal is
enabled MUST fail startup; a group- or world-readable one SHOULD produce a
warning, as for harness env files (ADR-0008).

`OTEL_EXPORTER_OTLP_PROTOCOL` set to anything other than `http/json` MUST produce
a startup warning that Harness speaks only OTLP/HTTP JSON and that the value is
ignored. It MUST NOT fail startup: the variable is commonly inherited from other
tooling. gRPC is out of scope.

If an OTLP signal is enabled and no endpoint resolves for it from any source, the
daemon MUST refuse to start with an error naming the signal and every place an
endpoint may be set. A signal that is consented to and cannot be delivered is a
configuration mistake, better refused than discovered.

Header values and any credential MUST NOT appear in any log line, error, status
output or telemetry. Where a header needs to be identified (REQ-14), the daemon
MUST report its name and a truncated SHA-256 fingerprint of its value, never the
value. An `http://` endpoint on a non-loopback host with headers configured
SHOULD produce a startup warning that credentials will travel in cleartext.

### REQ-4: Content, redaction and caps

Every string that any sink emits — a log body, an attribute value, a span name,
a span status message, a span attribute value, a JSONL field — MUST pass through
`internal/redact.String` before it is queued. Redaction happens once, at
conversion, so no unredacted copy sits in a queue, a retry buffer or a file.

Only what agent-trace's classifier retains is exported: tool name, action,
summary, targets, result byte count, error flag, and mark type and note. Tool
*output* content and file contents are never read into an item and MUST NOT be
exported by any path.

Caps, applied after redaction, truncating on a UTF-8 boundary and appending `…`:

* a log body or JSONL `body`: 4 KiB;
* a span name: 256 bytes;
* any other string attribute: 1 KiB;
* `agent.targets`: at most 32 entries, each capped at 512 bytes.

User-message notes and the session title are prompt text. They MUST be exported
by default, redacted and capped. When `[telemetry] omit_prompts = true`, every
user-message note MUST be replaced by the literal `[prompt omitted]` in log
bodies, JSONL `body`, and user-message span names; `agent.session.title` MUST be
omitted; and redaction still runs on everything else. The default is export
because the prompt is what makes a run legible to the operator whose destination
it is; the switch exists for operators whose prompts carry customer data.

### REQ-5: The item vocabulary

Every observed item is described by one set of fields, used as OTLP log
attributes (REQ-6) and as JSONL keys (REQ-8). Names follow OTel semantic
conventions where one exists, and otherwise reuse agent-trace's span attribute
names, so a log query and a trace query share one vocabulary.

Resource attributes (one resource per daemon):

| Attribute | Value |
|---|---|
| `service.name` | `harness` |
| `service.version` | the daemon's build version |
| `host.name` | the host name |

Additional resource attributes from `OTEL_RESOURCE_ATTRIBUTES` SHOULD be added;
they MUST NOT override `service.name`.

Item attributes:

| Attribute | Type | Present |
|---|---|---|
| `harness.name` | string | always |
| `agent.adapter` | string | always (e.g. `crush`, `claude-code`) |
| `agent.session.id` | string | always |
| `agent.session.title` | string | when known and prompts are not omitted |
| `agent.session.cwd` | string | when known |
| `agent.model` | string | when known |
| `agent.item.kind` | string | always: `tool` or `mark` |
| `agent.item.seq` | int | always: the item's session-positional sequence |
| `agent.item.id` | string | always: 16 lowercase hex (REQ-7) |
| `agent.tool.name` | string | tool items |
| `agent.tool.action` | string | tool items: `search`, `read`, `edit`, `exec`, `verify`, `other` |
| `agent.tool.is_error` | bool | tool items |
| `agent.result.bytes` | int | tool items |
| `agent.targets` | string array | tool items with targets |
| `agent.outside_count` | int | tool items touching paths outside the workdir |
| `agent.mark.type` | string | mark items, passed through verbatim (`user-message`, `compaction`, `subagent`, `error`, or any type agent-trace adds) |

`agent.item.id` is the item's idempotency key. The observer delivers an item at
most once per daemon lifetime, but a restart within the observer's no-replay
slack can deliver an item a previous daemon also delivered; consumers that care
MUST be able to deduplicate on `agent.item.id` alone.

### REQ-6: The logs signal

When `logs = true`, every contributing item MUST become exactly one OTLP
LogRecord, batched into `ExportLogsServiceRequest` bodies and POSTed as
`application/json` to the resolved logs URL.

| LogRecord field | Value |
|---|---|
| `timeUnixNano` | the item's `Time` |
| `observedTimeUnixNano` | the item's `ObservedAt` |
| `severityNumber` / `severityText` | per the table below |
| `body` | string: the tool event's `Summary`, or the mark's `Note` (REQ-4); for a mark with an empty note, the mark type |
| `attributes` | REQ-5 item attributes |
| `traceId` | the session's trace ID (REQ-7) |
| `spanId` | the item's span ID when the item maps to a span (REQ-7); absent otherwise |

| Item | severityText | severityNumber |
|---|---|---|
| mark of type `error` | `ERROR` | 17 |
| tool event with `is_error` | `WARN` | 13 |
| everything else | `INFO` | 9 |

An error *mark* is the model or provider failing the turn: an operational
condition. A tool call that returned an error is the agent working — a failing
test, a missing file — and marking it `ERROR` would bury the provider outage
under a thousand failed `grep`s. An alert on `severityText = "ERROR"` MUST mean a
turn failed.

The instrumentation scope MUST be named `github.com/stump-wtf/harness/telemetry`
with the build version.

### REQ-7: The traces signal

When `traces = true`, the daemon MUST export one trace per contributing agent
session.

**Trace ID.** A session's trace ID MUST equal the one agent-trace's
`otel.BuildTrace` assigns: the first 16 bytes of `SHA-256("trace:" + session.Key)`
as 32 lowercase hex characters. Logs (REQ-6) compute it without building a trace,
and a test MUST pin the two derivations equal.

**Item and span IDs.** Every item's ID MUST be the first 8 bytes, as 16
lowercase hex characters, of

```
SHA-256("harness-item:" + traceID + ":" + kind + ":" + seq + ":" + ordinal)
```

where `kind` is `tool` or `mark`, `seq` is the item's session-positional
sequence, and `ordinal` is its index among items of the same kind sharing that
`seq`, in delivery order (almost always `0`). An item *maps to a span* exactly
when `otel.BuildTrace` produces a span for it — today tool events and marks of
type `user-message`, `compaction` and `subagent` — and that span's ID MUST be
the item's ID, with `parentSpanId` rewritten to match.

agent-trace's own span IDs are derived from a counter over the spans in one
build. That is deterministic for a complete session and wrong for this daemon:
the observer does not replay history, so after a restart the first item
delivered becomes span zero again and collides with a different span the
previous daemon already sent. Keying by item makes a span's ID a property of the
item, stable across builds, idle exports and restarts. The implementation MAY get
item-keyed IDs from an agent-trace change (preferred; Cairn traces would then
share them) or by re-keying `BuildTrace` output in Harness; if it re-keys, a test
MUST pin the one-span-per-mapped-item correspondence against the pinned
agent-trace version, so an upstream change to what maps to a span fails a test
rather than silently mis-keying spans.

Items MUST keep their session-positional `seq` from the observer, which MUST
assign it as a full parse of the session would, so the same item has the same
`seq` in every daemon lifetime.

**Timestamps.** Before building, an item with an empty or unparseable timestamp
MUST be given the observer's `Time` (RFC 3339, nanoseconds), so a span's start
matches its log record's time rather than falling back to the session start.

**Export triggers.** A session's unsent spans MUST be exported when any of:

* no item has arrived for the session for `idle_flush` (default 5m);
* the session reaches 2048 unsent spans (exported immediately, bounding memory
  for a long-running session);
* its harness exits or leaves `running` (SHOULD, promptly; the idle trigger
  covers it regardless);
* the daemon shuts down (REQ-11).

**Resumed sessions send only new spans.** The exporter MUST NOT send a span ID
it has already sent for that session. It MAY retain already-sent items as
parenting context — in particular the current turn's `user-message` mark, so
tool calls arriving after an idle export still parent under their turn — and
MUST filter the spans built from them out of the request. A span's fields are
frozen when it is first sent: a turn span exported at an idle point keeps that
end time when its turn resumes. A span that is already sent is never re-sent to
correct it.

**Bounds.** Per-session context MUST be forgotten 24 hours after the session's
last item, and the exporter MUST track at most 256 sessions, exporting and
forgetting the least recently active when a new one would exceed that. Items
arriving for a forgotten session export into the same trace (the trace ID is a
function of the session) as spans without a turn parent.

**Error marks.** `otel.BuildTrace` maps no span for an `error` mark, and this
spec MUST NOT invent one: the logs signal carries errors. A trace for a session
that only ever failed contains its turn span and nothing else.

Spans are sent through `internal/otlpexport`, extended per REQ-9 and REQ-10. A
trace larger than `batch_size` spans MUST be split across requests.

### REQ-8: The local events file

When `events_file` is set, every contributing item MUST be appended to it as one
line of JSON: a single object, UTF-8, terminated by `\n`, containing no raw
newline.

Fields are the REQ-5 item attributes under the same dotted names as flat keys,
plus:

| Key | Value |
|---|---|
| `schema` | `harness.telemetry/v1` |
| `time` | the item's `Time`, RFC 3339 with nanoseconds, UTC |
| `observed_time` | the item's `ObservedAt`, same format |
| `severity` | `INFO`, `WARN` or `ERROR` (REQ-6) |
| `body` | the log body (REQ-6) |
| `trace_id` | the session's trace ID |
| `span_id` | the item's span ID, when it maps to a span |
| `service.name`, `service.version`, `host.name` | the resource attributes |

A field that is absent from a log record MUST be absent from the line, not null.
Adding a field is compatible; renaming or removing one MUST change `schema`.

The file and any directory the daemon creates for it MUST be `0600` / `0700`.
Rotation is by rename: when appending a batch would take the file past
`events_file_max_mb`, the daemon MUST rename `events.jsonl.(n-1)` to
`events.jsonl.n` down to `events.jsonl` → `events.jsonl.1`, deleting the file
beyond `events_file_keep`, then open a fresh file. Rename rotation is what
inode-tracking tailers (Vector, Fluent Bit, Promtail/Alloy) expect; copy-truncate
loses lines. Each line MUST be written whole, so a tailer never reads half a
record followed by another record.

A write failure (disk full, permission revoked, directory removed) MUST NOT stop
the daemon or any other signal. The batch counts as `failed`, the error is
logged (rate-limited, REQ-10), and the next batch retries opening the file.

### REQ-9: Queues never block

Each enabled signal MUST take items through its own observer subscription with a
bounded buffer, so a stalled sink cannot starve another. Subscription names MUST
be `telemetry.logs`, `telemetry.traces` and `telemetry.events_file`. Observer-side
overflow is counted by the observer per subscriber; the exporter MUST surface it
as `dropped_observer` (REQ-12).

Behind each subscription, each signal MUST keep a bounded in-memory queue of at
most `queue_size` units — log records, spans, or lines. When the queue is full,
the **oldest** unit MUST be dropped and counted as `dropped_queue`; the newest
data is the most likely to explain what is happening now.

No telemetry path may block the observer, the supervisor, the Manager's lock or
the control socket. Conversion, redaction, queueing and export MUST run on
telemetry-owned goroutines, and at most one export request per OTLP signal MUST
be in flight at a time.

Batches flush when they reach `batch_size` units or when `batch_interval` has
passed since the first unit was queued, whichever comes first.

Worst-case telemetry memory MUST be bounded by configuration: `queue_size` units
per signal at their capped sizes (REQ-4), plus one in-flight batch per signal,
plus the trace exporter's per-session context bounded by REQ-7. It MUST NOT grow
with collector downtime.

### REQ-10: Delivery, retry and failure

An OTLP request MUST set `Content-Type: application/json`, the merged headers
(REQ-3), and, when compression is `gzip`, a gzip body with
`Content-Encoding: gzip`. Each request MUST be bounded by the resolved timeout.

Response handling:

| Response | Outcome |
|---|---|
| 2xx, no `partialSuccess` rejections | every unit `exported` |
| 2xx with `partialSuccess` | rejected count `rejected`, remainder `exported`; the `errorMessage` logged, redacted, rate-limited |
| 429, 502, 503, 504, or a network error or timeout | retry |
| any other status | the batch `failed`, not retried; status logged, rate-limited |

Retries MUST use exponential backoff starting at 1s, doubling, capped at 30s, with
full jitter. A `Retry-After` header (seconds or an HTTP date) MUST be honoured
when present, in place of the computed delay. A batch still undelivered 5 minutes
after its first attempt MUST be given up and counted `failed`. While a batch is
retrying, its signal's queue keeps accepting (and, when full, dropping) units per
REQ-9.

Export failures MUST be logged to the daemon log at warn level, rate-limited to
at most one line per signal per distinct failure kind per minute, carrying the
status code or error class and a running count — never a header value or a
request body. A rejected credential (401/403) is thereby visible in the daemon
log within a minute without flooding it.

### REQ-11: Shutdown flush

On daemon shutdown the telemetry pipeline MUST, within `shutdown_timeout` in
total (default 5s):

1. stop taking new items from the observer;
2. export every tracked session's unsent spans as a final trace export;
3. flush every queue with one delivery attempt per batch, no retries;
4. flush and close the events file.

Units still undelivered at the deadline MUST be counted (`dropped_queue` for
queued units, `failed` for an abandoned in-flight batch) and the totals logged in
one line. The flush MUST NOT extend shutdown past `shutdown_timeout`, and MUST
NOT delay the supervisor's own shutdown handling of harnesses.

### REQ-12: Self-telemetry

The telemetry pipeline MUST expose a `Stats()` snapshot, per signal (`logs`,
`traces`, `events_file`):

* units `exported`, `rejected`, `failed`, `dropped_queue`, `dropped_observer`;
* export requests (or file write batches) by result: `success`, `retryable`,
  `permanent`;
* current queue length;
* time of the last successful delivery (zero until the first);
* the last error, as a redacted, capped string.

Counters MUST be cumulative for the daemon's lifetime and safe to read
concurrently without blocking export.

Once SPEC-0013's `/metrics` endpoint exists, it SHOULD publish them as:

```
harness_telemetry_items_total{signal,outcome}              counter  outcome: exported|rejected|failed|dropped_queue|dropped_observer
harness_telemetry_requests_total{signal,result}            counter  result: success|retryable|permanent
harness_telemetry_queue_items{signal}                      gauge
harness_telemetry_last_success_timestamp_seconds{signal}   gauge    omitted until the first success
```

`signal` is `logs`, `traces` or `events_file`; units are records, spans and lines
respectively. As SPEC-0013 REQ-3 requires for model calls, the last-success
gauge MUST be omitted rather than zeroed before a first success. Only enabled
signals report. No series carries a harness, session or endpoint label.

### REQ-13: `[daemon] otel_endpoint` is removed

`[daemon] otel_endpoint` MUST be rejected at config load with a migration error,
following the delete-not-deprecate convention for removed keys: still decoded so
its presence can be refused, never acted on. The error MUST name `[telemetry]`,
`traces = true`, and `OTEL_EXPORTER_OTLP_ENDPOINT`/`endpoint` as the replacement.
`core.DaemonConfig.OTelEndpoint` MUST be deleted.

It MUST NOT become an alias. The key never did anything and the documentation
said so; aliasing it would make an inert line in an existing config start
publishing agent transcripts on upgrade, without the consent REQ-1 requires.

### REQ-14: Operators can see what was resolved

At startup, when any destination is configured, the daemon MUST log one line per
enabled signal with the resolved endpoint (or file path), the source of each
setting (`env`, `env_file`, `file`, `default`), the compression, and the header
names with value fingerprints (REQ-3). `harness doctor` SHOULD report the same
under a telemetry section, in its existing source-per-setting format.

## Scenarios

### Scenario: a provider quota outage, as the log backend sees it

Three Crush harnesses opted in with `export_telemetry = true`; a provider's quota
empties.

* each failed turn yields a Crush `error` mark; the observer delivers it on its
  own, with no tool call to carry it
* each becomes an `ERROR` LogRecord with `harness.name`, `agent.mark.type =
  "error"`, and the provider's message (redacted) as the body
* the log backend shows a simultaneous burst of `ERROR` records across all three
  harnesses and no `INFO` tool records after it
* a query on `severityText = "ERROR"` returns only failed turns, not the agents'
  failed test runs, which are `WARN`

For a Claude Code harness at the pinned agent-trace version, which surfaces no
API-error marks, the same outage shows as its records stopping; SPEC-0013's
`harness_last_successful_call_timestamp` is the alertable signal there.

### Scenario: a production operator with a collector

`harness.toml` sets `[telemetry] logs = true`, `traces = true`, `export_all =
true`, and `env_file = "/etc/harness/otel.env"`, which OpenBao renders with
`OTEL_EXPORTER_OTLP_ENDPOINT` and `OTEL_EXPORTER_OTLP_HEADERS=authorization=Bearer%20…`.

* the daemon resolves both endpoints from `env_file`, logs them with
  `source=env_file`, and logs `authorization` with a fingerprint, not a value
* no credential appears in `harness.toml`, in any log line, or in any supervised
  harness's environment
* records reach `/v1/logs` as items occur; each idle session's trace reaches
  `/v1/traces` five minutes after its last item
* log records and spans for the same item share `traceId` and `spanId`, so the
  trace backend links to the logs and back

### Scenario: a laptop with nothing configured

No `[telemetry]` table. `OTEL_EXPORTER_OTLP_ENDPOINT` happens to be exported in
the user's shell for another tool.

* no telemetry subscription, no socket, no file, no network connection
* the daemon logs once that the OTel variables were ignored and names the key
  that would enable export
* behaviour is otherwise byte-for-byte what it was before this spec

### Scenario: the collector is down for an hour

* each batch retries with backoff and jitter for five minutes, then counts as
  `failed`; the next batch starts its own retries
* each signal's queue fills to `queue_size` and drops its oldest units, counted
  as `dropped_queue`; memory stays at the configured bound
* the daemon log carries one warn line per signal per minute with a running
  count, not one per request
* the supervisor, the TUI and the control socket are unaffected throughout
* when the collector returns, the queued (most recent) units deliver, and
  `harness_telemetry_items_total` shows exactly how much was lost and where

### Scenario: a harness that never opted in

`[telemetry] logs = true`, `export_all = false`. `worker-a` sets
`export_telemetry = true`; `payroll-bot` sets nothing.

* `worker-a`'s items are exported
* `payroll-bot`'s items are discarded before redaction or queueing; nothing about
  them reaches the collector or the events file, and they are not counted as drops
* switching to `export_all = true` exports `payroll-bot` too, unless it sets
  `export_telemetry = false`, which wins

### Scenario: a session resumes after an idle export

A worker finishes a turn; five minutes pass; the trace for its session exports
(turn span plus five tool spans). A new tool call then arrives in the same turn.

* the next export sends one span: the new tool call, parented under the turn
  span sent earlier
* the turn span is not re-sent, and keeps the end time it had when it was first
  sent

### Scenario: the daemon restarts mid-session

* the observer does not replay items from before the restart
* the first item delivered after it keeps its session-positional `seq`, so its
  span ID differs from every span the previous daemon sent for that session
* the trace continues under the same trace ID; spans whose turn began before the
  restart export without a turn parent
* an item inside the observer's slack window may be delivered by both daemons; it
  carries the same `agent.item.id` and span ID both times

### Scenario: daemon shutdown

The daemon is stopped with two sessions mid-turn and 300 log records queued.

* both sessions' unsent spans export, and the 300 records flush, within
  `shutdown_timeout`
* if the collector is unreachable, the flush gives up at the deadline, logs one
  line with what was lost, and the daemon exits on time

### Scenario: a config that still carries `otel_endpoint`

* the daemon refuses to start with an error naming `[telemetry]` and
  `traces = true`
* nothing is exported to the old URL, before or after the edit, unless the
  operator opts in

## Out of Scope

* Token or cost accounting. The gateway owns spend.
* Cairn run-share export (`internal/cairnexport`): publication of a run as a
  shareable URL is a different consent and a different surface.
* The OTLP metrics signal. SPEC-0013's Prometheus endpoint owns metrics.
* OTLP/gRPC and OTLP/HTTP protobuf.
* A CLI command that follows the stream (`harness events --follow`); the events
  file and the collector are the consumption paths.
* Supervisor lifecycle events as OTLP records; the daemon's own log carries them.
* Span types for marks agent-trace does not map (notably `error`).

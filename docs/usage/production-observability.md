---
title: "Production observability"
sidebar_position: 8
---

# Production observability

When Harness runs as a production service — a daemon on an agent box draining
queues around the clock — the useful record of what each agent *did* lives in
the agent's own session files. The daemon reads them continuously and can ship
every tool call and every mark (user message, compaction, subagent launch,
provider error) to the stack you already run:

- **OTLP logs**: one log record per item, searchable, with the provider's error
  message as the body of an `ERROR` record.
- **OTLP traces**: one trace per agent session, a span per turn and tool call.
- **A local JSONL events file**: the same records as lines, for Vector, Fluent
  Bit or Promtail/Alloy to tail. No network.

This page is the recipe book. The key-by-key reference is
[Configuration → Telemetry export](./configuration#telemetry-export-telemetry),
and the reasoning is [ADR-0022](/decisions/adr-0022-telemetry-export).

## Two consents, always

Nothing leaves the daemon unless **both** hold:

1. a destination is on in the global `harness.toml`: `logs = true`,
   `traces = true`, or an `events_file`;
2. the harness opted in: `export_telemetry = true` on it, or `export_all = true`
   in `[telemetry]` (an explicit `export_telemetry = false` always wins).

Environment variables never turn export on by themselves, and `harvest_trajectory`
is not the switch — reading a transcript locally is not consent to publish it.

```toml
[telemetry]
logs = true
traces = true
env_file = "/etc/harness/otel.env"

[harness.worker]
harness = "crush"
workdir = "~/src/worker"
enabled = true
export_telemetry = true
```

## Credentials go in `env_file`

`harness.toml` refuses a `headers` key and an endpoint with a password in it.
Collector credentials live in the standard `OTEL_EXPORTER_OTLP_*` variables,
best kept in the file `[telemetry] env_file` names: the daemon reads only the
OTLP keys from it and never puts them into its own environment, so no
supervised agent inherits your collector's API key. Keep the file `0600` (the
daemon warns otherwise); render it from your secrets manager.

```sh
# /etc/harness/otel.env — chmod 600
OTEL_EXPORTER_OTLP_ENDPOINT=https://collector.example.com:4318
OTEL_EXPORTER_OTLP_HEADERS=authorization=Bearer%20<token>
OTEL_RESOURCE_ATTRIBUTES=deployment.environment=prod
```

Header values are percent-decoded (`%20` is a space). At startup the daemon logs
one line per signal with the resolved endpoint, where each setting came from,
and each header as a name plus a fingerprint — enough to confirm a rotated
credential landed, never the value:

```
INFO telemetry export active signal=logs endpoint=https://collector.example.com:4318/v1/logs endpoint_source=env_file compression=none compression_source=default timeout=10s timeout_source=default headers=authorization=sha256:5f2c0e9a1b7d(env_file)
```

Changing `[telemetry]` needs a daemon restart; `export_telemetry` on a harness
applies on `harness reload`.

## Recipe: an OpenTelemetry Collector on the box

The most flexible setup: Harness speaks OTLP/HTTP to a local Collector, which
fans out to whatever you use.

```toml
[telemetry]
logs = true
traces = true
endpoint = "http://127.0.0.1:4318"
export_all = true
```

```yaml
# otelcol.yaml
receivers:
  otlp:
    protocols:
      http:
        endpoint: 127.0.0.1:4318

processors:
  batch: {}

exporters:
  debug:
    verbosity: basic
  otlphttp/backend:
    endpoint: https://otlp.example.com
    headers:
      authorization: ${env:BACKEND_TOKEN}

service:
  pipelines:
    logs:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlphttp/backend]
    traces:
      receivers: [otlp]
      processors: [batch]
      exporters: [otlphttp/backend]
```

Harness speaks OTLP/HTTP with JSON bodies only; gRPC is not supported, and an
inherited `OTEL_EXPORTER_OTLP_PROTOCOL=grpc` is ignored with a warning.

## Recipe: Grafana (Loki and Tempo)

Loki 3 and Tempo both ingest OTLP/HTTP. Point each signal at its own backend
with the signal-specific variables, which are full URLs used as-is:

```sh
# /etc/harness/otel.env
OTEL_EXPORTER_OTLP_LOGS_ENDPOINT=https://loki.example.com/otlp/v1/logs
OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=https://tempo.example.com/v1/traces
OTEL_EXPORTER_OTLP_LOGS_HEADERS=authorization=Basic%20<base64 user:token>
OTEL_EXPORTER_OTLP_TRACES_HEADERS=authorization=Basic%20<base64 user:token>
```

For Grafana Cloud, use the stack's OTLP gateway as the base endpoint instead
(`OTEL_EXPORTER_OTLP_ENDPOINT=https://otlp-gateway-<region>.grafana.net/otlp`)
with its `authorization=Basic%20…` header.

Loki keeps `service.name` as the `service_name` label and the record's
attributes as structured metadata (dots become underscores), so a provider
outage across the fleet is one query:

```logql
sum by (harness_name) (
  count_over_time({service_name="harness"} | severity_text = "ERROR" [5m])
)
```

Because a failed tool call is `WARN` and only a failed *turn* is `ERROR`, that
query does not fire on an agent's failing test runs. Log records carry the
session's `traceId` and, for tool calls and turns, the span's `spanId`, so
Grafana's trace-to-logs link lands on the right records.

## Recipe: Honeycomb

```sh
# /etc/harness/otel.env
OTEL_EXPORTER_OTLP_ENDPOINT=https://api.honeycomb.io
OTEL_EXPORTER_OTLP_HEADERS=x-honeycomb-team=<ingest key>
```

Traces and logs arrive under the `harness` service. Query log records with
`severity_text = ERROR` grouped by `harness.name` for outages, and
`agent.tool.is_error = true` for tool failures.

## Recipe: the JSONL file with Vector or Fluent Bit

No egress needed: the daemon appends one JSON object per item to a local file,
with the same fields the log records carry, flattened to dotted keys.

```toml
[telemetry]
events_file = "/var/lib/harness/events.jsonl"
export_all = true
```

```json
{"schema":"harness.telemetry/v1","time":"2026-09-21T12:00:03Z","observed_time":"2026-09-21T12:00:04Z","severity":"ERROR","body":"429 quota exhausted","trace_id":"…","harness.name":"worker","agent.adapter":"crush","agent.item.kind":"mark","agent.mark.type":"error","agent.item.id":"…","service.name":"harness","host.name":"agent-box"}
```

The file is `0600` and rotates by rename (`events.jsonl` → `events.jsonl.1`, …)
at `events_file_max_mb`, which inode-following tailers handle without losing
lines. Every line is written whole.

Vector:

```toml
# vector.toml
[sources.harness]
type = "file"
include = ["/var/lib/harness/events.jsonl"]

[transforms.parse]
type = "remap"
inputs = ["harness"]
source = '. = parse_json!(.message)'

[sinks.loki]
type = "loki"
inputs = ["parse"]
endpoint = "https://loki.example.com"
encoding.codec = "json"
labels.service = "harness"
labels.harness = "{{ \"harness.name\" }}"
```

Fluent Bit:

```ini
[INPUT]
    Name    tail
    Path    /var/lib/harness/events.jsonl
    Parser  json
    Tag     harness

[OUTPUT]
    Name    stdout
    Match   harness
```

`agent.item.id` is stable per item, so if a daemon restart ever delivers an item
twice (it can, inside a few seconds of slack), deduplicate on it.

## What is in a record, and what is not

Every string is redacted with the rules `harness logs` uses before it is queued
(URL userinfo, `Authorization` headers, secret-named assignments, vendor token
shapes), then capped: a body at 4 KiB, a span name at 256 bytes, other
attributes at 1 KiB, at most 32 targets. Tool **output** and file contents are
never read, so they are never exported. User prompts are exported by default,
redacted — set `omit_prompts = true` if yours carry customer data, and they
become `[prompt omitted]`.

| Attribute | Present on |
|---|---|
| `harness.name`, `agent.adapter`, `agent.session.id` | every record |
| `agent.session.title`, `agent.session.cwd`, `agent.model` | when known (title not under `omit_prompts`) |
| `agent.item.kind` (`tool`/`mark`), `agent.item.seq`, `agent.item.id` | every record |
| `agent.tool.name`, `agent.tool.action`, `agent.tool.is_error`, `agent.result.bytes`, `agent.targets`, `agent.outside_count` | tool calls |
| `agent.mark.type` | marks |

The resource is `service.name=harness`, `service.version`, `host.name`, plus
anything in `OTEL_RESOURCE_ATTRIBUTES`.

A caveat on outages: Crush records a provider failure as an error mark, so it
shows up as `ERROR` records. Claude Code does not surface API errors in its
transcript at the pinned agent-trace version; there an outage shows as that
harness's records stopping.

## When the collector is down

A dead collector costs counted telemetry and nothing else. Each signal has a
bounded queue (`queue_size`) that drops its oldest units when full; a failed
batch retries with backoff (429/502/503/504 and network errors, honouring
`Retry-After`) for up to five minutes, then is counted failed; the supervisor,
the TUI and the control socket are never held up. The daemon log carries at most
one warning per signal per failure kind per minute, with a running count — a
rejected credential (401/403) is visible within a minute without flooding the
log. On shutdown the daemon spends up to `shutdown_timeout` flushing, and logs
in one line what, if anything, was lost.

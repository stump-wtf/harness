---
status: draft
date: 2026-09-21
implements: [ADR-0021]
extends: [SPEC-0008]
requires: [SPEC-0002, SPEC-0006, SPEC-0012, SPEC-0013]
---

# SPEC-0014: Event-Triggered One-Shot Runs

## Overview

A prompt harness (SPEC-0006) can be fired by an external event as well as by a
clock. Two kinds of **trigger source** are defined:

* A **channel source** (`[channel.<name>]`). The daemon holds one listen-only
  MCP Streamable HTTP session to a server that advertises the Claude Code
  Channels capability. Each `notifications/claude/channel` it receives is a
  firing.
* A **webhook source** (`[webhook.<name>]`). It is an authenticated route,
  `POST /hooks/<name>`, on an opt-in HTTP listener. Each verified delivery that
  passes the source's filters is a firing.

A harness binds sources with a `triggers` key. A harness that carries
`schedule`, `triggers`, or both is a **triggered harness**. Every firing enters
the run machinery SPEC-0008 defines for scheduled runs, including run history,
per-run logs, `timeout`, `on_overlap`, `keep_runs` and `trigger`. The event
reaches the run as a private file named in its environment. It never becomes
prompt text or argv.

See ADR-0021 for the decision, the alternatives, and what is deferred. Harness
is a trigger, not a queue: nothing in this spec buffers events durably, and an
event whose loss matters belongs in a durable queue in front of Harness.

This spec extends SPEC-0008 (the run machinery), and amends it where
"scheduled" becomes "triggered". It requires SPEC-0002 (protocol operations and
events), SPEC-0006 (the prompt source, and argv synthesis, which are
unchanged), SPEC-0012 (hours gating of firings), and SPEC-0013 (source
metrics).

## Requirements

### Requirement: Channel Source Table

The daemon SHALL accept `[channel.<name>]` tables in the global `harness.toml`
and in `harness_d` drop-in files. A project `harness.toml` SHALL reject them as
a parse error naming the table. `<name>` MUST match
`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`, because it appears in references, protocol
replies and metric labels. The same name MUST NOT be declared twice across the
main file and its drop-ins.

| Key | Type | Required | Meaning |
| --- | --- | --- | --- |
| `url` | string | yes | The server's MCP Streamable HTTP endpoint |
| `headers` | table of strings | no | Request headers sent on every request of the session |
| `env_file` | path | when a `${NAME}` reference is used | The file credential references resolve from (REQ "Credential Resolution") |
| `enabled` | bool | no (default `true`) | `false` keeps the table but never connects |
| `description` | string | no | Operator prose |

`url` MUST use the `https` scheme, unless its host is a loopback address
(`localhost`, `127.0.0.0/8`, `::1`), in which case `http` is also accepted.
`url` MUST NOT carry userinfo; credentials belong in `headers`. Unknown keys
SHALL be a parse error, as they are on every other table.

#### Scenario: Minimal channel source

- **WHEN** a config declares `[channel.sb]` with an `https` `url` and no other
  keys
- **THEN** the config parses, and the source is available to bind as
  `channel.sb`

#### Scenario: Plain HTTP to a remote host

- **WHEN** a `[channel.*]` table sets `url = "http://sb.example.com/mcp/x"`
- **THEN** config parsing fails, naming the source and `url`, because the
  session's credentials would cross the network in cleartext

#### Scenario: Credentials in the URL

- **WHEN** a `[channel.*]` `url` contains `user:token@`
- **THEN** config parsing fails and directs the operator to `headers`

#### Scenario: Channel table in a project file

- **WHEN** a project `harness.toml` declares `[channel.sb]`
- **THEN** the project fails to load with an error directing the operator to
  the global `harness.toml`

### Requirement: Webhook Source Table

The daemon SHALL accept `[webhook.<name>]` tables in the global `harness.toml`
and in `harness_d` drop-in files, with the same name grammar, uniqueness rule and
project-file rejection as REQ "Channel Source Table".

| Key | Type | Required | Meaning |
| --- | --- | --- | --- |
| `verify` | enum | yes | `bearer`, `hmac-sha256`, `github`, `gitea`, `gitlab` or `standard-webhooks` (REQ "Webhook Verification") |
| `secret` | string | yes | Exactly one `${NAME}` reference (REQ "Credential Resolution") |
| `env_file` | path | yes | The file `secret` resolves from |
| `events` | list of strings | no | Allowlist matched against the event name (REQ "Webhook Filtering") |
| `signature_header` | string | only for `hmac-sha256` | Header carrying the signature |
| `signature_prefix` | string | no (`hmac-sha256` only, default `""`) | Prefix stripped before decoding, e.g. `sha256=` |
| `event_header` | string | no (`bearer`/`hmac-sha256` only) | Header naming the event type |
| `delivery_header` | string | no (`bearer`/`hmac-sha256` only) | Header carrying a unique delivery ID |
| `max_body` | size | no (default `"1MiB"`) | Largest accepted body. `B`/`KiB`/`MiB` suffix, at most `"25MiB"` |
| `rate_limit` | string | no (default `"60/m"`) | `<n>/<s\|m\|h>`, or `"0"` for no limit (REQ "Webhook Rate Limit") |
| `enabled` | bool | no (default `true`) | `false` keeps the table, and its route answers `404` |
| `description` | string | no | Operator prose |

The presets `github`, `gitea`, `gitlab` and `standard-webhooks` fix their
headers. Setting `signature_header`, `signature_prefix`, `event_header` or
`delivery_header` alongside a preset SHALL be a parse error. `events` with
`bearer` or `hmac-sha256` SHALL require `event_header`. A `standard-webhooks`
source SHALL also require its resolved secret to be in the Standard Webhooks
secret format (REQ "Standard Webhooks Verification").

#### Scenario: GitHub preset

- **WHEN** a config declares `[webhook.gh]` with `verify = "github"`,
  `env_file`, and `secret = "${GH_HOOK_SECRET}"`
- **THEN** the config parses, and the route `POST /hooks/gh` verifies
  `X-Hub-Signature-256`

#### Scenario: Standard Webhooks preset

- **WHEN** a config declares `[webhook.switchboard]` with
  `verify = "standard-webhooks"`, `env_file`, and
  `secret = "${SB_NOTIFY_SECRET}"`, and the file defines `SB_NOTIFY_SECRET` as a
  `whsec_` value
- **THEN** the config parses, and the route `POST /hooks/switchboard` verifies
  `webhook-signature` over `webhook-id`, `webhook-timestamp` and the body

#### Scenario: Preset with an overridden header

- **WHEN** a `verify = "gitea"` table also sets `signature_header`
- **THEN** config parsing fails, naming the source and `signature_header`

#### Scenario: Standard Webhooks with an event header

- **WHEN** a `verify = "standard-webhooks"` table sets `event_header`
- **THEN** config parsing fails, naming the source and `event_header`, because
  the scheme reads its event name from the body (REQ "Standard Webhooks
  Verification")

#### Scenario: Event filter without an event header

- **WHEN** a `verify = "bearer"` table sets `events` but not `event_header`
- **THEN** config parsing fails, naming the source and `events`

#### Scenario: Oversized limit

- **WHEN** a table sets `max_body = "100MiB"`
- **THEN** config parsing fails, naming the source and the 25 MiB ceiling

### Requirement: Triggers Key

The daemon SHALL accept an optional `triggers` key on a `[harness.*]` table. Its
value SHALL be a non-empty list of references of the form `channel.<name>` or
`webhook.<name>`, each naming a source declared in the same config view.

A reference to an undeclared source, a reference of any other form, and the
same reference listed twice SHALL each be a parse error naming the harness and
the reference. A harness carrying `schedule`, `triggers`, or both SHALL be
referred to as a *triggered harness*. `schedule` and `triggers` MAY be combined.

#### Scenario: Binding a webhook

- **WHEN** `[harness.pr-review]` sets `prompt_file` and
  `triggers = ["webhook.gitea-pr"]`, and `[webhook.gitea-pr]` is declared
- **THEN** the config parses, and `pr-review` is a triggered harness

#### Scenario: Undeclared source

- **WHEN** a harness sets `triggers = ["channel.nope"]` and no `[channel.nope]`
  exists
- **THEN** config parsing fails, naming the harness and `channel.nope`

#### Scenario: Schedule and triggers together

- **WHEN** a harness sets `schedule = "@every 1h"` and
  `triggers = ["channel.sb"]`
- **THEN** the config parses, and the harness fires both hourly and on each
  notification from `channel.sb`

### Requirement: Triggered Harness Exclusions

The daemon SHALL apply SPEC-0008 REQ "Schedule Exclusions" to every triggered
harness, and SHALL reject each combination below as a parse error naming the
harness and the offending key:

| Rejected combination | Rationale |
| --- | --- |
| `triggers` without `prompt` or `prompt_file` | A triggered unit is a one-shot agent run |
| `triggers` with `enabled = true` | Autostart intent and on-demand firing are distinct |
| A harness with `triggers` that is a `[profile.*]` member | Profile autostart would fire it with no event |
| `triggers` with `restart = "always"` or `"unless-stopped"` | A respawn would re-run the one-shot with no event |
| `triggers` in a project `harness.toml` | Project harnesses never enter the daemon's config view |
| `timeout`, `on_overlap` or `keep_runs` on a harness that is not triggered | Run keys shape runs that only a triggered harness has |
| `catch_up` on a harness with no `schedule`, no channel trigger, and no `operating_hours` | Nothing else can miss a firing (REQ "Channel Catch-Up", REQ "Operating Hours On Triggered Harnesses") |
| `hours_shutdown` or `hours_shutdown_timeout` on a triggered harness | They close a resident session; a triggered run is bounded by `timeout` |

#### Scenario: Triggers on a cmd harness

- **WHEN** a harness sets `cmd` and `triggers`
- **THEN** config parsing fails, naming the harness and the missing prompt
  source

#### Scenario: Run keys on a webhook harness

- **WHEN** a harness with `triggers = ["webhook.gh"]` and no `schedule` sets
  `timeout = "20m"` and `keep_runs = 50`
- **THEN** the config parses, and both keys apply to its runs

#### Scenario: Catch-up with nothing to miss

- **WHEN** a harness whose only trigger is `webhook.gh`, with no `schedule` and
  no `operating_hours`, sets `catch_up = true`
- **THEN** config parsing fails, naming the harness and `catch_up`

### Requirement: Overlap Default For Triggered Harnesses

`on_overlap` SHALL default to `queue` on a harness that sets `triggers`, and to
`skip` on a harness that sets only `schedule`. An explicit value SHALL always
win. SPEC-0008 REQ "Overlap Policy" otherwise applies unchanged.

#### Scenario: Default on an event harness

- **WHEN** a harness sets `triggers` and no `on_overlap`, and a firing arrives
  while a run is in flight
- **THEN** the firing is held, and it starts when the run ends

#### Scenario: Default on a schedule-only harness

- **WHEN** a harness sets only `schedule` and no `on_overlap`
- **THEN** its overlap policy is `skip`, as SPEC-0008 specifies

### Requirement: Firing

An event from a source SHALL fan out to every harness whose `triggers` lists
that source, in config order. For each harness it SHALL call the same run entry
point a schedule firing uses (SPEC-0008 REQ "Firing And Overlap") with:

* trigger `channel` or `webhook`, according to the source's kind;
* the source reference, for example `webhook.gitea-pr`;
* the event (REQ "Event Delivery To The Run").

The overlap decision SHALL be made per harness. A failure to start one harness
SHALL NOT prevent or delay the others. A firing SHALL never stack a second
concurrent process for the same harness. A firing while the harness is
`stopping` SHALL be recorded `skipped` with reason `stopping`. A firing from
`failed` SHALL clear the failed latch through the ordinary start path.

#### Scenario: Fan-out to two harnesses

- **WHEN** `pr-review` and `pr-labels` both list `webhook.gh` and one verified
  delivery arrives
- **THEN** each harness receives its own firing and overlap decision, and each
  started run has its own record and its own event file

#### Scenario: One harness fails to spawn

- **WHEN** a delivery fans out to two harnesses and the first one's agent binary
  is missing
- **THEN** the first run is recorded `failed`, and the second still starts

### Requirement: Overlap Skip Coalescing

While a run is in flight, successive `skipped` decisions for the same harness
that share a trigger, source and reason SHALL coalesce into one run record. The
record SHALL carry a `coalesced` count of the firings it covers, which starts at
1 and increments once per further skip. The first skip SHALL create the record,
and emit `job_run_finished` as SPEC-0008 REQ "Lifecycle Events" specifies.
Later increments SHALL update the stored record and emit no further event. When
the run in flight ends, the next skip SHALL create a new record.

This amends SPEC-0008 REQ "Overlap Policy", whose "further firing is recorded
`skipped`" becomes "further firings are recorded in one coalesced `skipped`
record per in-flight run". Coalescing applies to schedule firings too.

#### Scenario: A burst during one run

- **WHEN** 200 webhook firings arrive for a `queue` harness while one run is in
  flight
- **THEN** one firing is held, and the history gains exactly one `skipped`
  record with `coalesced` = 199 and reason `overlap`

#### Scenario: A new run opens a new record

- **WHEN** a skip is coalesced during run 7, run 7 ends, run 8 starts, and
  another firing is skipped
- **THEN** the second skip creates a new `skipped` record rather than
  incrementing the first

### Requirement: Run Record Fields

Run records (SPEC-0008 REQ "Run History") SHALL gain these fields. The trigger
values `channel` and `webhook` SHALL join `schedule`, `manual` and `catch_up`.

| Field | Present when | Meaning |
| --- | --- | --- |
| `source` | A source caused the decision | The source reference, e.g. `channel.sb` |
| `event_id` | The decision carried an event | The event's ID (REQ "Event Delivery To The Run") |
| `reason` | Outcome is `skipped` | `overlap`, `stopping` or `outside_hours` |
| `coalesced` | Outcome is `skipped` | Firings the record covers (REQ "Overlap Skip Coalescing") |

A record SHALL NOT carry any byte of an event payload, a header value, or a
credential (ADR-0008).

#### Scenario: A webhook run's record

- **WHEN** a delivery with `X-Gitea-Delivery: abc` starts run 12 of `pr-review`
- **THEN** run 12's record carries trigger `webhook`, source `webhook.gitea-pr`,
  and `event_id` `abc`, and no field containing the body

### Requirement: Event Delivery To The Run

For a run started by an event, the daemon SHALL write the event, before the
process spawns, to `<jobs dir>/<harness>/<run_id>.event.json`. That is the
directory that holds the run's log (SPEC-0008 REQ "Per-Run Logs"). The file
SHALL be created with mode `0600`, and it SHALL be pruned together with the
run's record and log. A held firing's event SHALL be written when the held run
starts. A harness without a per-run log directory SHALL get no event file.

The file SHALL hold one JSON object, the *event envelope*:

| Field | Meaning |
| --- | --- |
| `version` | `1` |
| `kind` | `channel` or `webhook` |
| `source` | The source reference |
| `event_id` | The delivery ID from the scheme's delivery header; otherwise a daemon-generated unique ID |
| `received_at` | When the daemon received the event, RFC 3339 UTC |
| `replayed_at` | Present only on a manual replay (REQ "Manual Trigger With Event") |
| `channel` | `kind = channel`: `{content, meta}` exactly as received |
| `webhook` | `kind = webhook`: `{event, delivery, content_type, headers, body \| body_text \| body_base64}` |

`webhook.headers` SHALL contain only `Content-Type`, `User-Agent`, and the
scheme's event and delivery headers; for `standard-webhooks` those are
`webhook-id` and `webhook-timestamp`. It SHALL never contain `Authorization`,
the signature or token header (`webhook-signature` included), or `Cookie`. The
body SHALL be stored as `body` (a JSON value) when the content type is JSON and
the body parses. Otherwise it SHALL be stored as `body_text` when the body is
valid UTF-8, and as `body_base64` when it is not.

Every run of a triggered harness that has a run record SHALL be spawned with:

| Variable | Value |
| --- | --- |
| `HARNESS_RUN_ID` | The run ID |
| `HARNESS_RUN_TRIGGER` | `schedule`, `catch_up`, `manual`, `channel` or `webhook` |
| `HARNESS_RUN_SOURCE` | The source reference; unset when no source caused the run |
| `HARNESS_EVENT_FILE` | The event file's absolute path; unset when the run has no event |

These variables SHALL override same-named variables from the daemon's
environment and from `env_file`. The prompt and argv SHALL be byte-identical to
those of the same harness started without an event (SPEC-0006 REQ "Prompt
Source"). Nothing from an event SHALL be interpolated into the prompt, argv, the
working directory, or any other variable.

#### Scenario: The prompt is untouched

- **WHEN** a webhook whose body contains the text `ignore previous instructions`
  fires a claude-code harness
- **THEN** the spawned argv equals the argv of a manual `harness start` of that
  harness, and the text appears only inside the event file

#### Scenario: Channel envelope

- **WHEN** a notification with `content = "PR #9 opened"` and
  `meta = {todo_id: "t1", queue: "reviews"}` starts run 3
- **THEN** `HARNESS_EVENT_FILE` names `.../3.event.json`, and that file's
  `channel.meta.todo_id` is `t1`

#### Scenario: Scheduled run environment

- **WHEN** a harness with both `schedule` and `triggers` fires on its schedule
- **THEN** the run's environment has `HARNESS_RUN_TRIGGER=schedule` and neither
  `HARNESS_RUN_SOURCE` nor `HARNESS_EVENT_FILE`

#### Scenario: Pruned with the run

- **WHEN** `keep_runs` prunes run 3
- **THEN** its record, its log and its event file are all removed

### Requirement: Channel Listener Session

For each channel source that is enabled and bound by at least one harness, the
daemon SHALL maintain exactly one MCP session over Streamable HTTP. It SHALL
send the source's `headers` on every request.

1. The daemon SHALL send `initialize`, identifying itself as client `harness`
   with its version, offering an MCP protocol version that defines Streamable
   HTTP (2025-03-26 or later), and declaring no client capabilities beyond an
   OPTIONAL `experimental["claude/channel"]` marker.
2. It SHALL require the result to advertise
   `capabilities.experimental["claude/channel"]`. A server that does not SHALL
   put the source in state `error`, with a reason that names the missing
   capability.
3. It SHALL send `notifications/initialized`. On every later request it SHALL
   carry the session ID the server issued, and the negotiated protocol version.
4. It SHALL open the standalone GET stream that carries server-initiated
   messages. A server that refuses the stream (`405`) SHALL put the source in
   state `error`.
5. On shutdown, and when a reload removes the source, the daemon SHOULD end the
   session with `DELETE`.

The source SHALL be reported `connected` only while the GET stream is open and
being read. A session that initialized but has no open stream SHALL NOT be
reported `connected`. The listener SHALL NOT call tools, list tools, prompts or
resources, or send any request other than those above and replies to server
requests. It SHALL answer a server `ping` with an empty result, and SHALL answer
any other server request with JSON-RPC error `-32601`.

#### Scenario: Server without the channel capability

- **WHEN** a source's server answers `initialize` without
  `capabilities.experimental["claude/channel"]`
- **THEN** the source is `error`, `harness triggers` shows why, and no GET
  stream is opened

#### Scenario: Initialized, but no stream

- **WHEN** `initialize` succeeds but the GET stream fails to open
- **THEN** the source is not reported `connected`

#### Scenario: Unbound source

- **WHEN** a `[channel.sb]` table exists but no harness lists `channel.sb`
- **THEN** the daemon opens no session to it, and it reports state `unbound`

### Requirement: Channel Notification Handling

Each `notifications/claude/channel` message on a source's stream SHALL be one
firing (REQ "Firing"). Its `params.content` MUST be a string and its
`params.meta`, if present, MUST be an object of string values. A message that
violates this, or whose serialized `params` exceed 64 KiB, SHALL be dropped,
logged with the source's name, and counted as `invalid`. It SHALL NOT fire.

The daemon SHALL treat `content` and `meta` as opaque. It SHALL NOT interpret,
route on, or rewrite them, apart from storing them in the envelope. Other
notifications SHALL be ignored.

#### Scenario: A doorbell fires the bound harness

- **WHEN** a connected source receives one `notifications/claude/channel`
- **THEN** each bound harness gets one firing with trigger `channel`

#### Scenario: Malformed notification

- **WHEN** a notification's `params.meta.todo_id` is a number
- **THEN** it fires nothing, and it is logged and counted as `invalid`

### Requirement: Channel Reconnection

When a session or its stream fails, the daemon SHALL reconnect with jittered
exponential backoff: an initial delay of 1 second, doubling to a ceiling of 5
minutes. The delay SHALL reset once a stream has stayed open for 5 minutes.

* When a stream drops within a live session, the daemon SHOULD reopen it with
  `Last-Event-ID`, if the server issued event IDs.
* When the server reports the session unknown (`404` on a request carrying the
  session ID), the daemon SHALL re-initialize a new session.
* An authentication failure (`401`/`403`), a missing channel capability, or a
  protocol violation SHALL put the source in state `error`, and it SHALL be
  retried at the backoff ceiling.
* Any other failure SHALL put the source in state `backoff`.

A source's failures SHALL NOT affect any other source, the webhook listener, or
the daemon.

#### Scenario: Server restart

- **WHEN** the server restarts and the stream closes
- **THEN** the source goes to `backoff`, re-initializes, and returns to
  `connected` without operator action

#### Scenario: Revoked token

- **WHEN** the server starts answering `401`
- **THEN** the source is `error`, and the daemon retries no more often than once
  every 5 minutes

### Requirement: Channel Catch-Up

For each harness that sets `catch_up = true` and lists a channel source, the
daemon SHALL start one run with trigger `catch_up` and that source when the
source becomes `connected`:

* the first time after the daemon starts; and
* after any disconnection that lasted longer than the scheduler's late grace
  (one minute, SPEC-0008 REQ "Missed Window Handling").

A reconnection after a shorter outage SHALL NOT start a catch-up run. A catch-up
run SHALL carry no event file, and SHALL be subject to `on_overlap` like any
other firing.

#### Scenario: Daemon restart

- **WHEN** the daemon restarts, and a `catch_up` harness's channel source
  connects
- **THEN** one `catch_up` run starts, whose `HARNESS_RUN_SOURCE` names the
  source

#### Scenario: A short blip

- **WHEN** a connected source's stream drops and reconnects 10 seconds later
- **THEN** no catch-up run starts

#### Scenario: A long outage

- **WHEN** a source is in `backoff` for 20 minutes and then connects
- **THEN** exactly one `catch_up` run starts for each bound `catch_up` harness

### Requirement: One Consumer Per Endpoint

Two `[channel.*]` tables whose `url` values are equal after normalization SHALL
be a parse error naming both sources. Normalization lowercases the scheme and
host, elides the scheme's default port, and removes a trailing `/`. A channel
server may ring only one of the sessions connected to an endpoint, so a second
session in the same daemon would swallow the doorbells the first one should
hear.

#### Scenario: Same endpoint twice

- **WHEN** `[channel.a]` and `[channel.b]` both set
  `url = "https://sb.example.com/mcp/x"`
- **THEN** config parsing fails, naming both sources

### Requirement: Webhook Listener

The daemon SHALL start an HTTP listener when `[server] webhook_listen` names an
address, or when `harness daemon --webhook-listen ADDR` or
`HARNESS_WEBHOOK_LISTEN` does (SPEC-0010 REQ "Precedence Order"). With none of
them set, no listener SHALL exist. The listener SHALL be a server of its own,
separate from the SSH server and the metrics listener.

* When `[server] webhook_tls_cert_file` and `webhook_tls_key_file` are both set,
  the listener SHALL serve HTTPS only. Setting exactly one of them SHALL be a
  parse error.
* A non-loopback bind without TLS SHALL start, SHALL log a warning at startup,
  and SHALL be flagged by `harness doctor`.
* The listener SHALL bound a slow client: a header read timeout of 10 seconds, a
  request read timeout of 30 seconds, a write timeout of 30 seconds, an idle
  timeout of 60 seconds, and 64 KiB of request headers.
* It SHALL handle at most 64 requests at a time, and SHALL answer the excess with
  `503`.
* A reload that changes the bind address or TLS files SHALL log that a daemon
  restart is required, and SHALL keep serving on the old settings.
* On shutdown the listener SHALL stop accepting connections, and SHALL give
  in-flight requests up to 5 seconds to finish.

A `[webhook.*]` table with no listener configured is valid. Its state SHALL be
`no_listener`, and `harness doctor` SHALL flag it.

#### Scenario: Off by default

- **WHEN** a config declares `[webhook.gh]` and sets no `webhook_listen`
- **THEN** the daemon opens no HTTP port, and `harness triggers` shows
  `webhook.gh` as `no_listener`

#### Scenario: Half-configured TLS

- **WHEN** `[server]` sets `webhook_tls_cert_file` without
  `webhook_tls_key_file`
- **THEN** config parsing fails, naming `webhook_tls_key_file`

#### Scenario: Bind change on reload

- **WHEN** a reload changes `webhook_listen`
- **THEN** the listener keeps its old address, and the daemon logs that a
  restart is required

### Requirement: Webhook Routes

The listener SHALL serve exactly these routes:

| Method | Path | Behavior |
| --- | --- | --- |
| `POST` | `/hooks/<name>` | A delivery to `[webhook.<name>]` |
| `GET` | `/healthz` | `200` with body `ok`, revealing nothing about sources |

A path under `/hooks/` naming no enabled source that a harness binds SHALL be
answered `404`. So SHALL any other path. The response SHALL be identical for an
unknown, disabled or unbound name, so routes cannot be enumerated. Any method
other than `POST` on `/hooks/<name>` SHALL be answered `405`. The listener SHALL
NOT redirect, serve files, or expose any other surface.

#### Scenario: Disabled route

- **WHEN** `[webhook.gh]` sets `enabled = false`, and a correctly signed
  delivery arrives
- **THEN** the response is `404`, byte-identical to the response for a name that
  was never declared

#### Scenario: GET on a hook

- **WHEN** a client sends `GET /hooks/gh`
- **THEN** the response is `405`

### Requirement: Webhook Verification

Every delivery SHALL be verified before it is parsed, filtered, or recorded
anywhere other than a counter. The daemon SHALL read the body within `max_body`
(answering `413` beyond it), and then check it:

| `verify` | Signature or token | Event header | Delivery header |
| --- | --- | --- | --- |
| `bearer` | `Authorization: Bearer <secret>` (scheme matched case-insensitively) | `event_header` | `delivery_header` |
| `hmac-sha256` | hex HMAC-SHA256 of the raw body, keyed by the secret, in `signature_header` after `signature_prefix` | `event_header` | `delivery_header` |
| `github` | `X-Hub-Signature-256: sha256=<hex HMAC-SHA256>` | `X-GitHub-Event` | `X-GitHub-Delivery` |
| `gitea` | `X-Gitea-Signature: <hex HMAC-SHA256>` | `X-Gitea-Event` | `X-Gitea-Delivery` |
| `gitlab` | `X-Gitlab-Token: <secret>` | `X-Gitlab-Event` | `X-Gitlab-Event-UUID` |
| `standard-webhooks` | `webhook-signature: v1,<base64 HMAC-SHA256>` over `<webhook-id>.<webhook-timestamp>.<body>`, with a timestamp tolerance (REQ "Standard Webhooks Verification") | none; the body's `type` | `webhook-id` |

Comparisons SHALL be constant-time. A missing, malformed or wrong signature or
token SHALL be answered `401` with the same body in every case. It SHALL be
logged with the route name and peer address, and never with the value it
presented.

#### Scenario: Valid GitHub signature

- **WHEN** a delivery to a `github` route carries a correct
  `X-Hub-Signature-256` for its body
- **THEN** it passes verification

#### Scenario: Body altered in transit

- **WHEN** one byte of the body differs from the one that was signed
- **THEN** the response is `401`, and nothing fires

#### Scenario: Oversized body

- **WHEN** a delivery's body exceeds the route's `max_body`
- **THEN** the response is `413`, and the signature is not evaluated

### Requirement: Standard Webhooks Verification

The `standard-webhooks` scheme SHALL implement the symmetric (`v1`) signature of
the [Standard Webhooks](https://www.standardwebhooks.com/) specification. It is
the scheme Switchboard signs its notify hooks with (Switchboard ADR-0029), and
any sender that follows the specification can use it.

1. **Secret format.** The value `secret` resolves to SHALL be `whsec_`
   followed by the standard, padded base64 encoding (RFC 4648 §4) of between 24
   and 64 bytes. The HMAC key SHALL be the decoded bytes, never the string. A
   value without the prefix, one whose remainder does not decode, and one that
   decodes to fewer than 24 or more than 64 bytes SHALL each be a parse error
   naming the source and the reference, never the value (REQ "Credential
   Resolution").
2. **Headers.** A delivery SHALL carry `webhook-id`, `webhook-timestamp` and
   `webhook-signature`, matched case-insensitively as HTTP header names are.
   `webhook-id` MUST be 1 to 256 bytes of visible ASCII (`0x21`–`0x7E`).
   `webhook-timestamp` MUST be a decimal integer count of seconds since the
   Unix epoch.
3. **Timestamp tolerance.** `webhook-timestamp` MUST lie within 5 minutes
   (300 seconds) of the daemon's wall clock, in either direction. The check
   SHALL use the time the request was received.
4. **Signed content.** The expected signature SHALL be the HMAC-SHA256, keyed as
   in item 1, of the `webhook-id` value, a `.`, the `webhook-timestamp` value,
   a `.`, and the raw body, all byte-for-byte as received.
5. **Signature list.** `webhook-signature` is a list of
   `<version>,<base64 signature>` entries separated by single spaces. The
   delivery SHALL pass when at least one `v1` entry's decoded signature equals
   the expected signature under a constant-time comparison. Every `v1` entry
   SHALL be compared, so a sender that signs with both its old and its new
   secret while it rotates verifies against a route holding either one. An
   entry with any other version (for example `v1a`, the asymmetric variant)
   SHALL be ignored, not rejected. An entry whose signature does not decode
   SHALL be skipped. A header with no `v1` entry SHALL fail.
6. **Delivery ID and event name.** `webhook-id` SHALL be the delivery ID: the
   de-duplication key (REQ "Webhook Filtering"), the envelope's `event_id`, and
   its `webhook.delivery`. Because the ID is signed, altering it to slip past
   de-duplication SHALL fail verification. The event name SHALL be the body's
   top-level `type` member, read only after verification passes, when the body
   is a JSON object whose `type` is a string. Otherwise the delivery has no
   event name.

A failure at any item from 2 to 5 SHALL be answered `401` with the same body as
every other verification failure (REQ "Webhook Verification") and counted as
`unauthorized` (REQ "Trigger Metrics"). The log line SHALL carry the route, the
peer address, and one reason (`missing_header`, `malformed_header`,
`timestamp_out_of_tolerance` or `bad_signature`), and SHALL NOT carry any
header value.

#### Scenario: A Switchboard notify hook verifies

- **WHEN** a `standard-webhooks` route whose secret resolves to a valid
  `whsec_` value receives a delivery with `webhook-id: msg_1`, the current time
  in `webhook-timestamp`, and a `v1` signature of `msg_1.<timestamp>.<body>`
  under that secret
- **THEN** it passes verification, and a run it starts has `event_id` `msg_1`

#### Scenario: Secret rotation on the sender

- **WHEN** a delivery's `webhook-signature` is
  `v1,<signature under the old secret> v1,<signature under the new secret>`
- **THEN** it passes verification on a route holding the new secret, and on a
  route still holding the old one

#### Scenario: Stale timestamp

- **WHEN** a correctly signed delivery's `webhook-timestamp` is 6 minutes in
  the past
- **THEN** the response is `401`, nothing fires, and the log line carries reason
  `timestamp_out_of_tolerance` and no header value

#### Scenario: Timestamp from the future

- **WHEN** a correctly signed delivery's `webhook-timestamp` is 6 minutes ahead
  of the daemon's clock
- **THEN** the response is `401`, and nothing fires

#### Scenario: Replay inside the tolerance

- **WHEN** a verified delivery with `webhook-id: msg_1` fires, and the identical
  request is sent again 30 seconds later
- **THEN** the second response is `202` with decision `duplicate`, and nothing
  fires

#### Scenario: Altered delivery ID

- **WHEN** a captured delivery is resent with its `webhook-id` changed and its
  signature unchanged
- **THEN** the response is `401`, and nothing fires

#### Scenario: Only an asymmetric signature

- **WHEN** a delivery's `webhook-signature` carries only `v1a,…` entries
- **THEN** the response is `401`

#### Scenario: Secret in the wrong format

- **WHEN** a `standard-webhooks` source's secret resolves to a value without the
  `whsec_` prefix, such as a bare hex string
- **THEN** config parsing fails, naming the source and the reference, and the
  error does not echo the value

#### Scenario: Event name from the body

- **WHEN** a `standard-webhooks` route sets `events = ["todo.ready"]`, and a
  verified delivery's body is `{"type": "todo.created", …}`
- **THEN** the response is `202` with decision `ignored`, and nothing fires

### Requirement: Webhook Filtering

After verification, the daemon SHALL apply, in order:

1. **Events.** When the source sets `events`, a delivery whose event name is
   not in the list SHALL be answered `202` with decision `ignored`. It SHALL
   fire nothing and SHALL make no record. The event name is the value of the
   scheme's event header, or for `standard-webhooks` the body's `type` (REQ
   "Standard Webhooks Verification"). A delivery with no event name is not in
   the list.
2. **De-duplication.** When the scheme has a delivery header and the delivery
   carries one, a delivery ID seen on this route in the last 24 hours, among
   the last 1024 IDs kept, SHALL be answered `202` with decision `duplicate`. It
   SHALL fire nothing and SHALL make no record. The set SHALL be held in memory,
   and it is NOT REQUIRED to survive a restart.
3. **Rate limit** (REQ "Webhook Rate Limit").

A delivery that passes all three SHALL be a firing.

#### Scenario: GitHub ping

- **WHEN** a `github` route with `events = ["pull_request"]` receives a verified
  `ping` delivery
- **THEN** the response is `202` with decision `ignored`, and the run history
  is unchanged

#### Scenario: Redelivery

- **WHEN** a sender redelivers `X-Gitea-Delivery: abc` an hour after it fired
- **THEN** the response is `202` with decision `duplicate`, and nothing fires

### Requirement: Webhook Rate Limit

Each route SHALL enforce `rate_limit` as a token bucket, with capacity `n`,
refilling `n` tokens per unit. Only deliveries that passed verification and
filtering SHALL consume a token, so unauthenticated traffic cannot exhaust a
route's budget. A delivery with no token SHALL be answered `429` with a
`Retry-After` header. It SHALL NOT be added to the de-duplication set, so the
sender's retry is not treated as a duplicate. `"0"` SHALL disable the limit.

#### Scenario: A burst over the limit

- **WHEN** a route with `rate_limit = "2/m"` receives three verified deliveries
  within a second
- **THEN** two fire, and the third is answered `429` with `Retry-After`

#### Scenario: Forged traffic does not spend the budget

- **WHEN** a route receives 1000 deliveries with bad signatures, and then one
  good one
- **THEN** the good one fires

### Requirement: Webhook Responses

Responses SHALL be JSON (`application/json`) and SHALL name only the route and
its decisions:

| Status | Body |
| --- | --- |
| `202` | `{"webhook", "event_id", "decision": "fired", "firings": [{"harness", "decision": "started"\|"queued"\|"skipped", "run_id"?}]}` |
| `202` | `{"webhook", "decision": "ignored"}` or `{"webhook", "event_id", "decision": "duplicate"}` |
| `401` / `404` / `405` / `413` / `429` / `503` | `{"error": "unauthorized" \| "not_found" \| "method_not_allowed" \| "payload_too_large" \| "rate_limited" \| "unavailable"}` |

`run_id` SHALL be present for `started`, and for `skipped` (the coalesced
record's ID). It SHALL be absent for `queued`. The response SHALL NOT wait for a
run to finish, and SHALL carry no run output, environment or configuration.

#### Scenario: A queued firing

- **WHEN** a verified delivery fans out to one harness with a run in flight
  under `queue`
- **THEN** the response is `202` with that harness's decision `queued` and no
  `run_id`

### Requirement: Credential Resolution

A source's `env_file` SHALL be resolved like `prompt_file` (SPEC-0006 REQ
"Prompt Source"): `~` expands, and a relative path anchors on the directory of
the file that declared the source. The file SHALL be read as `KEY=VALUE` lines,
in the format `env_file` already uses.

* `${NAME}` references SHALL be expanded only in `[webhook.*] secret` and in
  `[channel.*] headers` values, and only from the source's own `env_file`. They
  SHALL never be expanded from the daemon's environment, so the CLI and the
  daemon resolve identically.
* `secret` MUST consist of exactly one `${NAME}` reference. A literal secret
  SHALL be a parse error, because `harness.toml` is routinely committed to a
  dotfiles repository.
* A reference to a missing file, or to a name the file does not define, SHALL be
  a parse error naming the source and the reference, but never the value.
* An `env_file` readable by group or other SHALL produce a warning, and
  `harness doctor` SHALL flag it.

Resolved values SHALL be read at load and on every reload. They SHALL NOT be
written to `state.json`, run records, logs, protocol frames, or `describe`
output. `describe` and `triggers` MAY show header names, and SHALL NOT show
header values.

#### Scenario: Literal secret

- **WHEN** a `[webhook.*]` table sets `secret = "hunter2"`
- **THEN** config parsing fails, telling the operator to reference a variable in
  `env_file`, and the error does not echo the value

#### Scenario: Header value reference

- **WHEN** `[channel.sb]` sets
  `headers = { Authorization = "Bearer ${SB_TOKEN}" }` and its `env_file`
  defines `SB_TOKEN`
- **THEN** every session request carries the expanded header, and no daemon
  output contains the token

### Requirement: Source Reconciliation On Reload

After every successful reload the daemon SHALL reconcile sources incrementally.
A failed reload SHALL change nothing.

* A channel source whose normalized `url`, resolved-header fingerprint and
  `enabled` are unchanged, and which is still bound, SHALL keep its session and
  stream untouched. This holds however its set of bound harnesses changed.
* A channel source whose identity changed SHALL end its session and connect
  anew. A reconnection caused by a reload SHALL NOT count as an outage for REQ
  "Channel Catch-Up".
* A channel source that was removed, disabled or unbound SHALL end its session.
* The webhook route table SHALL be replaced atomically. A request in flight
  SHALL complete against the table it started with. A route whose name and
  `rate_limit` are unchanged SHALL keep its token bucket and de-duplication set.
* Changes to the set of harnesses bound to a source SHALL apply from the next
  event.

The requirement is load-bearing for the same reason SPEC-0008 REQ "Schedule
Reconciliation On Reload" is: external tooling rewrites the config periodically,
and a listener that reconnected on every rewrite would drop doorbells and could
never hold a stream.

#### Scenario: No-change rewrite

- **WHEN** a config is rewritten with identical content while `channel.sb` is
  connected
- **THEN** the session ID and the open stream are unchanged, and no catch-up
  run starts

#### Scenario: Token rotation

- **WHEN** the value `SB_TOKEN` resolves to changes, and the config is reloaded
- **THEN** `channel.sb` reconnects with the new header

### Requirement: Operating Hours On Triggered Harnesses

`operating_hours` SHALL be accepted on a harness that sets `triggers` and no
`schedule`. The exclusion against `schedule` is unchanged (SPEC-0012 REQ
"Operating Hours Exclusions"). On such a harness, the gate (SPEC-0012 REQ "Gate
Evaluation") SHALL gate **firings**, not processes:

* A channel or webhook firing that arrives out of hours SHALL be recorded
  `skipped` with reason `outside_hours`, coalesced per REQ "Overlap Skip
  Coalescing". The webhook response SHALL report that harness's decision as
  `skipped`.
* A run already in flight when the window closes SHALL continue, bounded by its
  `timeout`.
* With `catch_up = true`, the first in-hours gate evaluation after one or more
  `outside_hours` skips SHALL start one run with trigger `catch_up`.
* A manual `trigger` SHALL NOT be gated.

#### Scenario: Doorbell at night

- **WHEN** a harness with `operating_hours = "Mon-Fri 09:00-18:00"` and a
  channel trigger receives a doorbell on Saturday
- **THEN** no run starts, and the history gains a `skipped` record with reason
  `outside_hours`

#### Scenario: Catching up at opening

- **WHEN** that harness also sets `catch_up = true`, and Monday 09:00 arrives
- **THEN** exactly one `catch_up` run starts

### Requirement: Manual Trigger With Event

`trigger` (SPEC-0008 REQ "Manual Trigger") SHALL accept any triggered harness.
It SHALL answer `not_scheduled` only for a harness that has neither `schedule`
nor `triggers`.

`trigger` SHALL accept an optional event envelope. The CLI supplies it as
`harness trigger <name> --event FILE`. The envelope SHALL match the schema of
REQ "Event Delivery To The Run", so a copy of an earlier run's event file is a
valid input. Its `source` MUST name a source that the harness binds, and its
size MUST NOT exceed the largest `max_body` among the harness's webhook sources,
or 1 MiB when it has none. Otherwise the reply SHALL be the error
`invalid_event`.

The run SHALL be recorded with trigger `manual`, the envelope's `source` and
`event_id`, and an event file identical to the envelope except for an added
`replayed_at`. `--wait` SHALL behave as SPEC-0008 specifies.

#### Scenario: Replaying a past delivery

- **WHEN** an operator runs
  `harness trigger pr-review --event ~/.local/state/harness/jobs/pr-review/41.event.json --wait`
- **THEN** a run starts with trigger `manual` and source `webhook.gitea-pr`, its
  event file equals run 41's plus `replayed_at`, and the command exits with the
  run's exit code

#### Scenario: Envelope for an unbound source

- **WHEN** `--event` names an envelope whose `source` the harness does not list
- **THEN** the reply is `invalid_event`, and nothing runs

### Requirement: Trigger Visibility

The daemon SHALL add a `triggers` control operation (SPEC-0002 REQ "Control
Operations"), with the CLI verb `harness triggers`. For each declared source,
the reply SHALL carry:

* `source` and `kind`;
* `state`: `disabled`, `unbound`, `connecting`, `connected`, `backoff`, `error`,
  `listening` or `no_listener`;
* the time of the last state change, and the time of the last event;
* the last error, with no credential in it;
* the bound harnesses;
* counters since daemon start, per outcome (REQ "Trigger Metrics");
* for a channel, its `url` with the query removed;
* for a webhook, its path, `verify` and `events`.

Also:

* `jobs` SHALL include every triggered harness. Each entry SHALL carry its
  triggers with their source states, and SHALL omit the next window when the
  harness has no `schedule`.
* The harness projection returned by `list` and `describe` SHALL carry
  `triggers`, and every listing surface SHALL distinguish a triggered harness
  from a disabled one, as SPEC-0008 REQ "Schedule Visibility" requires for
  schedules.
* The daemon SHALL emit `trigger_source_changed { source, kind, state, error? }`
  on every state transition (SPEC-0002 REQ "Event Subscription").
* Run replies and `job_run_*` events SHALL carry `source`.
* The protocol minor version SHALL be bumped for these additions, and the bump
  documented beside the constant.

#### Scenario: A dead listener is visible

- **WHEN** a channel source's server has been unreachable for ten minutes
- **THEN** `harness triggers` shows it in `backoff` with its last error and the
  time it left `connected`

#### Scenario: An event harness in a listing

- **WHEN** a harness with `triggers` and no `schedule` is listed
- **THEN** it is shown as triggered, with its sources, and not as disabled

### Requirement: Trigger Metrics

When the metrics endpoint of SPEC-0013 is enabled, the daemon SHALL export:

```
harness_trigger_source_up{source,kind}             gauge    1 while connected or listening, else 0
harness_trigger_events_total{source,outcome}       counter
harness_trigger_last_event_timestamp{source}       gauge    unix seconds
harness_trigger_reconnects_total{source}           counter  channel sources only
```

`outcome` SHALL be one of `fired`, `ignored`, `duplicate`, `unauthorized`,
`too_large`, `rate_limited` or `invalid`. Every declared source SHALL report
every `outcome`, including zeros. Source names are bounded by the config, which
keeps label cardinality within SPEC-0013's cap.

#### Scenario: Alerting on a dead listener

- **WHEN** a channel source leaves `connected`
- **THEN** `harness_trigger_source_up{source="channel.sb"}` reads 0 on the next
  scrape

### Requirement: Triggers Round-Trip Through Config Writers

Any surface that rewrites a `[harness.*]` table SHALL preserve `triggers`, as
SPEC-0008 REQ "Schedule Round-Trip Through Config Writers" requires for the
schedule keys, and SHALL validate REQ "Triggered Harness Exclusions" before
writing. A surface that rewrites a harness table SHALL NOT rewrite, reorder, or
drop `[channel.*]` or `[webhook.*]` tables.

#### Scenario: Editing a triggered harness in the TUI

- **WHEN** an operator edits only the description of a triggered harness and
  saves
- **THEN** the rewritten table still carries `triggers`, and every source table
  is byte-identical

### Requirement: Error Handling Standards

All error-producing operations SHALL follow structured error handling:

* Errors SHALL be wrapped with context at each layer boundary. For example: a
  failure to connect `channel.sb`, because `initialize` failed, because the
  connection was refused.
* Distinguishable failure modes SHALL be defined as sentinel errors that callers
  can test for, including: a missing channel capability, a refused stream, an
  expired session, an authentication failure, an invalid event, and an unknown
  source reference.
* No error SHALL be silently swallowed. Each one SHALL be returned, logged with
  context, or handled with a documented reason for suppression. Dropped
  notifications and rejected deliveries SHALL each produce a log line and a
  counter increment.
* Logging SHALL be structured (key-value pairs), and SHALL never contain a
  credential, a header value, a signature, or a payload byte.
* Every rejected source or `triggers` combination SHALL produce a config error
  naming the file, the line of the offending table, the source or harness, and
  the specific reason.

#### Scenario: Located source error

- **WHEN** a `[webhook.*]` table omits `verify`
- **THEN** the error names the file, the table's line, the source and `verify`

### Requirement: Concurrency Safety

All concurrent operations SHALL follow safe concurrency patterns:

* Cancellation and timeout SHALL be propagated through a context across every
  source session, stream reader, request handler and firing.
* Each channel session and the webhook listener SHALL have explicit startup and
  graceful shutdown. Shutdown SHALL wait for firings in progress to reach the
  run entry point, or be abandoned before they reach it.
* Shared mutable state (source states, token buckets, de-duplication sets, the
  route table, coalescing records) SHALL be protected by synchronization, or
  owned by a single goroutine that receives messages. The overlap decision
  remains atomic on the harness's actor loop (SPEC-0008 REQ "Overlap Policy").
* A panic while handling one event SHALL be recovered and logged. It SHALL NOT
  terminate the daemon or its source.
* Tests covering this spec SHALL run with race detection enabled in CI.

#### Scenario: Concurrent deliveries to one harness

- **WHEN** 50 verified deliveries for one harness arrive concurrently
- **THEN** at most one process runs and at most one firing is held, as SPEC-0008
  specifies

#### Scenario: Panic in a firing

- **WHEN** handling one notification panics
- **THEN** the daemon keeps running, and the source's next notification still
  fires

## Security Requirements

This spec adds an inbound HTTP surface that is often exposed to the internet
through a reverse proxy, and an outbound session that carries a bearer
credential. ADR-0008's model applies: the surface is opt-in, every state-changing
route is authenticated, and no secret is held in the config or persisted.

### Authentication

| Endpoint | Auth | Justification |
| --- | --- | --- |
| `POST /hooks/<name>` | Required | A per-route signature or token (REQ "Webhook Verification"). No `verify = "none"` exists |
| `GET /healthz` | Public | A liveness probe for a reverse proxy or load balancer. It returns a constant `ok` and reveals no sources, harnesses or versions |

Channel sessions authenticate *to* the server with `headers` resolved from
`env_file`. Every session travels over TLS, except on loopback (REQ "Channel
Source Table").

### Rate Limiting

Each route has a token bucket, `rate_limit`, defaulting to `60/m`, spent only by
verified, filtered deliveries (REQ "Webhook Rate Limit"). The listener as a
whole is bounded to 64 concurrent requests and to the slow-client timeouts in
REQ "Webhook Listener". Firings are further bounded per harness by the overlap
policy, which holds at most one pending run.

### Replay Protection

A captured delivery is a valid request until something refuses it. For the
`bearer`, `hmac-sha256`, `github`, `gitea` and `gitlab` schemes, the only
defense is the in-memory de-duplication set (REQ "Webhook Filtering"). It
covers 24 hours and 1024 IDs, it is empty after a restart, and it needs the
sender to supply a delivery ID. The `standard-webhooks` scheme signs a
timestamp as well as the delivery ID, so a captured delivery is refused once it
is 5 minutes old, restart or not. Inside those 5 minutes, de-duplication
refuses it unless the daemon restarted in between (REQ "Standard Webhooks
Verification"). Operators who can choose a scheme for an internet-reachable
route SHOULD prefer `standard-webhooks`.

### Security Headers

Every listener response SHALL include:

- `Content-Security-Policy: default-src 'none'; frame-ancestors 'none'`. The
  listener serves JSON and `ok`, never a document.
- `X-Frame-Options: DENY`
- `X-Content-Type-Options: nosniff`
- `Referrer-Policy: strict-origin-when-cross-origin`
- `Cache-Control: no-store`
- `Strict-Transport-Security: max-age=31536000`, only when the listener serves
  TLS itself

### Request Body Size Limits

Every body SHALL be read through a bounded reader. The default limit is 1 MiB
per route, `max_body` can raise it to at most 25 MiB, and the excess is answered
`413` before verification. Request headers are bounded to 64 KiB. A channel
notification's `params` are bounded to 64 KiB (REQ "Channel Notification
Handling"). A manual event envelope is bounded as REQ "Manual Trigger With
Event" specifies.

### CSRF Protection

`POST /hooks/<name>` is state-changing: it can start a run. CSRF does not apply
to it, because authentication is a per-request signature or bearer header that a
browser never attaches on its own. The listener sets no cookies and honors no
ambient credential. So a cross-site request cannot carry a valid signature, and
no CSRF token is required. Cookies and session authentication SHALL NOT be added
to this listener.

### Redirect Validation

The listener SHALL NOT issue redirects. The channel listener SHALL NOT follow a
redirect to a different scheme or host than `url`, so a redirect cannot move the
bearer header to another origin.

---
status: approved
date: 2026-09-22
implements: [ADR-0025]
extends: [SPEC-0014, SPEC-0008]
requires: [SPEC-0002, SPEC-0006, SPEC-0012, SPEC-0013]
---

# SPEC-0019: Relay Attempts With Supervisor-Held Leases

## Overview

A triggered harness (SPEC-0014) may name a **lease source**: a Switchboard
endpoint the daemon claims work on. Such a harness is a **leased harness**, and
each of its runs is an **attempt**. For every admitted firing the daemon claims
one todo with the lease source's own credentials, spawns a fresh one-shot with
the todo and the earlier attempts in a private file, heartbeats the lease while
the process lives, runs an operator-authored success check when the process
ends, and reports `complete` or `fail` with a bounded summary and an optional
Cairn handle. Switchboard owns what happens next: backoff and re-queue below the
attempt cap, a dead letter at it.

See ADR-0025 for the decision, the alternatives, and **the part of ADR-0021 it
deliberately reverses**: for a leased harness the daemon calls Switchboard's
drain verbs. The ADR-0021 channel listener still calls no tools, and a harness
without `lease` is unchanged.

This spec extends SPEC-0014 (triggered harnesses, firing, event files) and
SPEC-0008 (the run machinery). It requires SPEC-0002 (protocol operations and
events), SPEC-0006 (prompt source and argv, which are unchanged), SPEC-0012
(hours gating before the claim) and SPEC-0013 (metrics).

The Switchboard side of the contract is Switchboard SPEC-0034 (attempt history,
written in parallel). Relay requires it: REQ-19 refuses a lease source whose
server predates it, with no degraded mode. Harness budgets (Harness SPEC-0021,
in flight) plug into REQ-4 as admission checks.

Terms:

* **Lease source**: a `[queue.<name>]` table (REQ-1).
* **Attempt**: one run of a leased harness that claimed a todo.
* **Lease token**: the opaque capability Switchboard returns with a fenced
  claim, presented on every later call for that attempt.
* **Lease deadline**: the time of the last successful claim or heartbeat plus
  `lease_ttl`.
* **Verdict**: success or failure of an attempt, decided by REQ-9.

## Requirements

### REQ-1: Lease Source Table

The daemon SHALL accept `[queue.<name>]` tables in the global `harness.toml` and
in `harness_d` drop-in files. A project `harness.toml` SHALL reject them as a
parse error naming the table. `<name>` MUST match
`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`, and the same name MUST NOT be declared twice
across the main file and its drop-ins.

| Key | Type | Required | Meaning |
| --- | --- | --- | --- |
| `url` | string | yes | The Switchboard endpoint's MCP Streamable HTTP URL |
| `headers` | table of strings | no | Request headers sent on every request of the lease client's session |
| `env_file` | path | when a `${NAME}` reference is used | The file credential references resolve from (REQ-18) |
| `queue` | string | no | Restrict `claim_next` to one granted queue; unset means every granted queue |
| `lease_ttl` | duration | no (default `"5m"`) | Lease TTL requested on claim and heartbeat, between `"1m"` and `"1h"` |
| `heartbeat_interval` | duration | no (default `lease_ttl / 3`) | Time between heartbeats; MUST be at most `lease_ttl / 2` |
| `enabled` | bool | no (default `true`) | `false` keeps the table; a leased harness bound to it records every firing `skipped` with reason `lease_disabled` |
| `description` | string | no | Operator prose |

`url` MUST use `https` unless its host is a loopback address, and MUST NOT carry
userinfo, exactly as SPEC-0014 REQ "Channel Source Table" requires of channel
sources. A lease source MAY share its `url` with a `[channel.*]` table; SPEC-0014
REQ "One Consumer Per Endpoint" SHALL NOT count a lease source as a consumer,
because the lease client never opens a notification stream (REQ-17). Two
`[queue.*]` tables MAY share a `url`.

#### Scenario: Lease source beside its channel

- **WHEN** a config declares `[channel.sb]` and `[queue.sb]` with the same `url`
- **THEN** the config parses, and both sources are available

#### Scenario: Heartbeat slower than half the lease

- **WHEN** `[queue.ci]` sets `lease_ttl = "5m"` and `heartbeat_interval = "4m"`
- **THEN** config parsing fails, naming the source and both keys

#### Scenario: Lease source in a project file

- **WHEN** a project `harness.toml` declares `[queue.ci]`
- **THEN** the project fails to load with an error directing the operator to
  the global `harness.toml`

### REQ-2: Lease Key and Exclusions

A `[harness.*]` table MAY set `lease` to exactly one reference of the form
`queue.<name>`. The following SHALL be parse errors, each naming the harness and
the offending key:

| Rejected | Because |
| --- | --- |
| `lease` on a harness with neither `schedule` nor `triggers` | An attempt is a run of a triggered harness |
| `lease` naming an undeclared `[queue.*]` table | A typo must fail the load, not silently never claim |
| `lease` in a project `harness.toml` | Project harnesses are never triggered (SPEC-0014) |
| `success_check`, `success_check_timeout`, `success_check_interval`, `success_check_before` or `attempt_receipt` without `lease` | They describe an attempt |
| `attempt_receipt` naming an undeclared `[cairn.*]` table | As for `lease` |

A leased harness SHALL keep every SPEC-0014 and SPEC-0008 run key
(`timeout`, `on_overlap`, `keep_runs`, `catch_up`) with its existing meaning.

#### Scenario: Lease on a resident harness

- **WHEN** `[harness.fixer]` sets `lease = "queue.ci"`, `enabled = true`, and
  neither `schedule` nor `triggers`
- **THEN** config parsing fails, stating that `lease` requires a triggered harness

#### Scenario: Check without a lease

- **WHEN** a triggered harness sets `success_check` and no `lease`
- **THEN** config parsing fails, naming `success_check`

### REQ-3: Success Check Keys

A leased harness MAY set:

| Key | Type | Default | Meaning |
| --- | --- | --- | --- |
| `success_check` | list of strings | unset | argv of the check. The first element is resolved like `prompt_file` when it contains a `/`, otherwise through `PATH`. No shell is involved |
| `success_check_timeout` | duration | `"10m"` | Total time the check may take, across re-runs (REQ-9) |
| `success_check_interval` | duration | `"30s"` | Wait before re-running a check that exited 75 |
| `success_check_before` | bool | `false` | Also run the check after the claim and before the spawn (REQ-9) |

`success_check` MUST be non-empty when set, and MUST NOT contain a `${NAME}`
reference or a `{placeholder}`. It SHALL NOT be expanded from any event, todo or
payload. `success_check_interval` MUST be less than `success_check_timeout`.

#### Scenario: Empty argv

- **WHEN** a leased harness sets `success_check = []`
- **THEN** config parsing fails, naming the key

### REQ-4: Admission Before Claim

For a leased harness, every admission decision SHALL be made before any
Switchboard call: `on_overlap` (SPEC-0008, SPEC-0014), operating hours
(SPEC-0012, SPEC-0014 REQ "Operating Hours On Triggered Harnesses"), and any
budget or usage-limit gate a later spec adds. A firing refused by any of them
SHALL be recorded exactly as it is today, and SHALL NOT cause a claim, a
heartbeat or any other call to the lease source. A held firing (`on_overlap =
"queue"`) SHALL claim only when it starts.

#### Scenario: Firing during a run

- **GIVEN** a leased harness with `on_overlap = "skip"` and an attempt in flight
- **WHEN** a doorbell fires it
- **THEN** a `skipped` record with reason `overlap` is written, and the fake
  Switchboard receives no call

#### Scenario: Outside hours

- **WHEN** a leased harness with `operating_hours` is fired outside its window
- **THEN** it records `skipped` / `outside_hours`, and no claim is made

### REQ-5: Claim

An admitted firing of a leased harness SHALL claim before it spawns. The daemon
SHALL call `claim_next` on the lease source with:

* `queue`, when the lease source sets it;
* `lease_ttl_seconds` from `lease_ttl`;
* `require_fence: true`;
* `claimant`: `harness/<host>/<harness>/run-<run_id>`, truncated to 128 bytes.

The claim SHALL use `claim_next` for every trigger, including channel and
webhook firings whose event names a todo. The daemon SHALL NOT parse an event
payload to choose a todo (ADR-0025, option 4A).

* If the response is `empty: true`, the daemon SHALL record the run `skipped`
  with reason `no_work`, SHALL spawn nothing, and SHALL write no context file.
* If the call fails (transport error, `forbidden`, or any error code), the daemon
  SHALL record the run `skipped` with reason `claim_failed` and the error code,
  and SHALL spawn nothing.
* On success the daemon SHALL hold the todo id, the lease token, the attempt
  number and `max_attempts`, and SHALL proceed to REQ-6. A success response
  without a lease token SHALL be treated as a failed claim (`claim_failed`,
  code `no_fence`): the daemon SHALL spawn nothing and SHALL let the lease
  lapse.

The run SHALL count as in flight for `on_overlap` from the moment the claim is
sent.

#### Scenario: Claimed and spawned

- **WHEN** a doorbell fires a leased harness and `claim_next` returns todo `td_1`
  at attempt 1 of 5
- **THEN** exactly one `claim_next` is sent, with `require_fence: true` and the
  harness's claimant label, and the process is spawned after it returns

#### Scenario: Nothing to claim

- **WHEN** a catch-up firing's `claim_next` returns `empty: true`
- **THEN** the run is recorded `skipped` / `no_work`, and no process starts

#### Scenario: Endpoint lacks the verb

- **WHEN** the lease source's endpoint answers `claim_next` with `forbidden`
- **THEN** the run is recorded `skipped` / `claim_failed` with code `forbidden`,
  and `harness doctor` reports the missing verb (REQ-21)

### REQ-6: Relay Context File and Environment

Before spawning an attempt, the daemon SHALL write
`<jobs dir>/<harness>/<run_id>.relay.json` with mode `0600`, beside the run log,
pruned with the run's record and log. It SHALL hold one JSON object:

| Field | Meaning |
| --- | --- |
| `version` | `1` |
| `todo` | The claimed todo as Switchboard returned it: `id`, `queue`, `title`, `kind`, `source`, `payload`, `work_order`, `created_at` |
| `attempt` | `{seq, number, max, claimed_at, lease_ttl_seconds}` |
| `prior_attempts` | The claim response's prior attempts, newest first, each `{seq, number, outcome, died, claimant, claimed_at, ended_at, summary, artifact}`; an empty array on a first attempt |
| `untrusted` | The list of JSON paths holding text the daemon did not author: at least `todo.title`, `todo.payload`, `todo.work_order` and `prior_attempts[].summary` |
| `summary_file` | The path of the attempt summary file (REQ-11) |

The lease token SHALL NOT appear in the file. The run SHALL be spawned with the
SPEC-0014 run-context variables plus:

| Variable | Value |
| --- | --- |
| `HARNESS_TODO_ID` | The claimed todo's id |
| `HARNESS_ATTEMPT` | The attempt number (the todo's `attempt` after the claim) |
| `HARNESS_MAX_ATTEMPTS` | The todo's `max_attempts` |
| `HARNESS_RELAY_FILE` | The context file's absolute path |
| `HARNESS_ATTEMPT_SUMMARY_FILE` | An absolute path, not yet created, where the agent MAY write its summary |

These variables SHALL override same-named variables from the daemon's
environment and from `env_file`. The prompt and argv SHALL be byte-identical to
those of the same harness run without a lease. When the firing also carries an
event, the event file SHALL still be written (SPEC-0014 REQ "Event Delivery To
The Run"); the context file is authoritative for which todo the attempt is for.

#### Scenario: Second attempt sees the first

- **GIVEN** attempt 1 of todo `td_1` failed with summary `tests still red`
- **WHEN** attempt 2 is claimed
- **THEN** its context file's `prior_attempts[0]` has `number` 1, `outcome`
  `failed`, `died` false and summary `tests still red`, and the argv equals an
  unleased run's argv

#### Scenario: The token stays out

- **WHEN** a claim returns lease token `T`
- **THEN** no byte of `T` appears in the context file, the run log, the event
  file or the run's environment

### REQ-7: Lease Heartbeat

While an attempt is in flight (claimed and not yet reported), the daemon SHALL
call `heartbeat` on the lease source every `heartbeat_interval`, passing the todo
id, `lease_ttl_seconds` and the lease token. Each successful
heartbeat SHALL move the lease deadline to its response time plus `lease_ttl`.
A heartbeat that fails with a transport error SHALL be retried with jittered
backoff, and retries SHALL NOT be spaced further apart than
`heartbeat_interval`. Heartbeats SHALL continue through the spawn, the process,
the success check and the report, and SHALL stop only when the attempt ends.

#### Scenario: A long run keeps its lease

- **WHEN** an attempt runs for 40 minutes with `lease_ttl = "5m"` and
  `heartbeat_interval = "1m"`
- **THEN** the fake Switchboard receives about 40 heartbeats, each carrying the
  lease token, and never observes the lease lapse

### REQ-8: Lease Loss

The daemon SHALL treat the lease as lost when a heartbeat is answered
`conflict` or `not_found`, or when the lease deadline passes without a
successful heartbeat. On loss the daemon SHALL:

1. stop the process, as SPEC-0008 stops a timed-out run (SIGTERM, then SIGKILL
   after the grace period), when it is still running;
2. skip the success check if it has not started, and abandon it if it has;
3. send no `complete`, `fail` or `release` for that attempt;
4. record the run's outcome as `cancelled` with `relay_outcome = lease_lost`.

When the lease deadline has passed, the stop SHALL begin no later than one
second after the deadline.

#### Scenario: Taken over

- **WHEN** a heartbeat for an in-flight attempt returns `conflict`
- **THEN** the process is stopped within one `heartbeat_interval`, no
  `complete` or `fail` is sent, and the run records `lease_lost`

#### Scenario: Switchboard unreachable

- **GIVEN** `lease_ttl = "5m"` and a last successful heartbeat at 12:00:00
- **WHEN** every later heartbeat fails with a transport error
- **THEN** the process keeps running until 12:05:00 and is stopped by 12:05:01

### REQ-9: Attempt Verdict

When an attempt's process exits, or SPEC-0008's `timeout` stops it, the daemon
SHALL decide the verdict:

* **No `success_check`:** exit code 0 is success, and anything else, including a
  timeout, is failure.
* **With `success_check`:** the daemon SHALL run the check, whatever the agent's
  exit code or timeout, as a child process outside the PTY, in the harness's
  `workdir`, with the harness's `env_file` and the REQ-6 variables plus
  `HARNESS_AGENT_EXIT` (the exit code, or `-1` when signalled) and
  `HARNESS_AGENT_OUTCOME` (`success`, `failed` or `timed_out`). Exit 0 SHALL be
  success. Exit 75 SHALL mean "not conclusive yet": the daemon SHALL re-run the
  check after `success_check_interval` until `success_check_timeout` has elapsed
  since the first run, and SHALL then treat the check as failed with reason
  `check_inconclusive`. Any other exit, a check that cannot be started, or a
  check still running at `success_check_timeout` (which the daemon SHALL kill)
  SHALL be failure. The check's output SHALL be appended to the run log under a
  delimiter line naming the check.

With `success_check_before = true`, the daemon SHALL also run the check after a
successful claim and before writing the context file. If it exits 0, the daemon
SHALL report `complete` with the summary `already satisfied: success_check
passed before spawn`, SHALL spawn nothing, and SHALL record `relay_outcome =
completed` with no exit code. Any other result SHALL proceed to REQ-6.

#### Scenario: The world disagrees with the agent

- **WHEN** the agent exits 0 and `success_check` exits 1
- **THEN** the verdict is failure

#### Scenario: A timed-out agent whose fix landed

- **WHEN** `timeout` stops the agent and `success_check` then exits 0
- **THEN** the verdict is success

#### Scenario: CI still running

- **GIVEN** `success_check_interval = "30s"` and `success_check_timeout = "10m"`
- **WHEN** the check exits 75 three times and then 0
- **THEN** the check runs four times about 30 seconds apart, and the verdict is
  success

#### Scenario: Already green

- **GIVEN** `success_check_before = true`
- **WHEN** a claimed todo's check exits 0 before the spawn
- **THEN** `complete` is sent and no process is spawned

### REQ-10: Report

After the verdict, the daemon SHALL call `complete` (success) or `fail`
(failure) on the lease source with the todo id, the lease token, the
REQ-11 `summary`, the REQ-12 `artifact` when one exists, and a `result` object:

```json
{"harness": {"host": "…", "harness": "…", "run_id": 42, "attempt": 2,
             "agent_exit": 0, "agent_outcome": "success", "duration_s": 724,
             "check_exit": 1, "check_runs": 1, "reason": "check_failed"}}
```

The summary and artifact SHALL be sent only as the top-level `summary` and
`artifact` arguments that Switchboard SPEC-0034 defines. They SHALL NOT be
duplicated inside `result`.

A report that fails with a transport error SHALL be retried with jittered
backoff until it is accepted or the lease deadline passes, with REQ-7
heartbeats continuing meanwhile. A report answered `conflict` or `not_found`
SHALL be treated as REQ-8 lease loss, except that there is no process to stop.
A report not accepted by the lease deadline SHALL record `relay_outcome =
report_lost`. The run SHALL end, and a held firing MAY start, only when the
report is accepted or lost.

#### Scenario: Report retried through a blip

- **WHEN** the first `fail` call times out and the second succeeds within the
  lease
- **THEN** exactly one `fail` is accepted, and the run records `failed`

### REQ-11: Attempt Summary

The daemon SHALL compose the attempt summary from:

1. a first line it authors:
   `attempt <n>/<max> · agent <outcome> (exit <code>) after <duration> · check exit <code> after <duration>`,
   omitting the check clause when there is no check;
2. the agent's summary, when `HARNESS_ATTEMPT_SUMMARY_FILE` exists after the
   process ends: the daemon SHALL read at most 16 KiB of it. When the content
   parses as a JSON object with a string `summary`, that string is the agent's
   summary and a string `artifact` is its artifact (REQ-12); otherwise the whole
   content is the agent's summary;
3. the last lines of the check's output, when a check ran.

The daemon SHALL apply its credential redactor to the whole summary, SHALL cap
it at 2048 bytes on a UTF-8 boundary, cutting parts 3 and then 2 before part 1,
and SHALL mark any cut with `…`. The summary SHALL NOT contain the lease token,
a header value or any value from an `env_file`.

#### Scenario: The agent explains itself

- **WHEN** the agent writes `{"summary": "bumped the fixture; flaky test
  remains", "artifact": "mcp://cairn/Ab12Cd34"}` to its summary file
- **THEN** the reported summary contains the daemon's first line and that text,
  and the reported artifact is `mcp://cairn/Ab12Cd34`

#### Scenario: A token in the agent's summary

- **WHEN** the agent's summary file contains a string shaped like a GitHub token
- **THEN** the reported summary contains the redacted form and not the token

### REQ-12: Attempt Artifact Handle

The daemon SHALL choose at most one artifact handle for a report, in this order:

1. the agent's `artifact` from REQ-11, when it is a string of at most 512 bytes
   that is an `mcp://cairn/<id>` handle or an `https` URL;
2. the handle of the REQ-13 attempt receipt, when the harness sets
   `attempt_receipt` and the upload succeeded;
3. none.

The daemon SHALL NOT fetch or dereference an agent-supplied handle. A handle that
fails the shape check SHALL be dropped, with a warning in the run log.

#### Scenario: Malformed handle

- **WHEN** the agent's summary file names `artifact = "file:///etc/passwd"`
- **THEN** the report carries no artifact from the agent, and the run log warns

### REQ-13: Attempt Receipt Upload

The daemon SHALL accept `[cairn.<name>]` tables in the global `harness.toml` and
drop-ins, with keys `url` (the Cairn base URL; `https` unless loopback),
`headers`, `env_file`, `ttl` (default `"7d"`), `enabled` and `description`, and
the same name, credential and project-file rules as REQ-1.

When a leased harness sets `attempt_receipt = "cairn.<name>"` and the agent did
not supply an artifact, the daemon SHALL, after the verdict and before the
report, create one Markdown artifact on that Cairn source holding the REQ-11
summary and the last 200 lines of the run log, ANSI-folded and passed through
the credential redactor. It SHALL be tagged `receipt`, `harness` and `relay`.
The daemon SHALL NOT probe for Cairn's receipt metadata (Cairn ADR-0027) or
switch shape by server capability: when receipt metadata ships, adopting it
replaces the tags outright, requires that Cairn release, and carries an upgrade
note. The upload SHALL be bounded by a 30-second timeout. A failed upload SHALL be logged
and SHALL NOT delay the report beyond that timeout or change the verdict.

#### Scenario: Receipt on failure

- **WHEN** an attempt with `attempt_receipt = "cairn.main"` fails and the agent
  wrote no artifact
- **THEN** one artifact is created on `cairn.main`, and the `fail` call carries
  its handle

#### Scenario: Cairn down

- **WHEN** the receipt upload times out
- **THEN** `fail` is sent without an artifact within 30 seconds of the verdict

### REQ-14: Aborted Attempts

When an attempt is ended by the daemon for a reason that is not a verdict on the
work (the daemon is stopping, the operator runs `harness stop`, a reload removes
the harness or its lease source, or an agent adapter reports a usage limit), the
daemon SHALL stop the process as SPEC-0008 does, SHALL skip the success check,
and SHALL call `release` with a summary naming the reason when the endpoint
advertises `release`, or `fail` with `result.harness.reason` set to that reason
when it does not. The run SHALL record `relay_outcome = released` or `failed`
accordingly. A reload that changes a leased harness's other keys SHALL NOT abort
an attempt in flight; it continues under the config it started with.

#### Scenario: Daemon shutdown with release granted

- **WHEN** the daemon shuts down during an attempt and the endpoint advertises
  `release`
- **THEN** the process is stopped, `release` is sent with a summary containing
  `daemon stopping`, and no success check runs

### REQ-15: Daemon Restart Mid-Attempt

The daemon SHALL NOT persist a lease token, a pending verdict or a todo payload
to `state.json` or any other file except the REQ-6 context file. After a crash
or restart, a run left `running` SHALL be reconciled as `interrupted`, as
SPEC-0008 already does, and the daemon SHALL make no Switchboard call for it.
The attempt's lease SHALL be left to lapse, so Switchboard records it as having
died (Switchboard SPEC-0034).

#### Scenario: Crash mid-attempt

- **WHEN** the daemon is killed during an attempt and restarted
- **THEN** that run is recorded `interrupted`, no `fail` or `release` is sent for
  it, and a later firing claims afresh

### REQ-16: Dead Letter

When a `fail` response reports `dead_letter: true`, the daemon SHALL record
`relay_outcome = dead_lettered`, SHALL emit a `relay_reported` event carrying
that outcome, and SHALL count it (REQ-21). The daemon SHALL NOT send a
notification of its own; notifying a human is Switchboard's. The daemon SHALL
read the dead letter only from `dead_letter`, and SHALL NOT infer one from
`state` or attempt counts.

#### Scenario: Last attempt fails

- **WHEN** attempt 5 of 5 is reported `fail` and the response has
  `dead_letter: true`
- **THEN** the run records `dead_lettered`, and the daemon makes no outbound
  call other than to the lease source

### REQ-17: Lease Client Session

For each lease source that a leased harness binds, the daemon SHALL hold an MCP
Streamable HTTP client session used only for `tools/list` and `tools/call`. The
session SHALL send `initialize` and `notifications/initialized`, and SHALL NOT
open the standalone GET notification stream, so the server never treats it as a
doorbell target. It SHALL NOT advertise the `claude/channel` capability. It
SHALL re-initialize when the server expires the session, and SHALL reconcile on
reload as SPEC-0014 REQ "Source Reconciliation On Reload" reconciles channel
sources. The ADR-0021 channel listener SHALL continue to call no tools.

#### Scenario: Never a ring target

- **WHEN** a lease source and a channel source share a `url`
- **THEN** the fake server observes one GET stream, belonging to the channel
  listener, and the lease client's session never opens one

### REQ-18: Credential and Token Handling

A lease source's and a Cairn source's `headers` SHALL resolve `${NAME}` only
from the source's own `env_file`, as SPEC-0014 REQ "Credential Resolution"
specifies. Resolved values and lease tokens SHALL NOT be written to
`state.json`, run records, logs, protocol frames, context files or `describe`
output. A lease token SHALL be held in memory only, for the life of its attempt,
and SHALL be discarded when the attempt ends. Error messages from the lease
client SHALL NOT include request headers or arguments containing a token.

#### Scenario: Doctor output

- **WHEN** an operator runs `harness describe fixer` during an attempt
- **THEN** it shows the lease source's name, URL, header names and the attempt's
  todo id, and no header value or token

### REQ-19: Required Switchboard Capabilities

Relay SHALL require a lease source whose server implements Switchboard
SPEC-0034's attempt history. There SHALL be no degraded mode for a server that
predates it, because the fence is what stops an agent holding the same endpoint
credential from closing its own attempt.

When the lease client session initializes (REQ-17), and again on every
re-initialize, the daemon SHALL read each drain verb's input schema from
`tools/list` and SHALL check that:

* `claim_next` declares `require_fence` and `claimant`;
* `heartbeat` declares `lease_token`;
* `complete` and `fail` declare `lease_token`, `summary` and `artifact`.

`release` is optional, because it is a verb an endpoint may or may not be
granted (REQ-14). When `release` is advertised, its schema SHALL declare
`lease_token` and `summary`.

When any check fails, the lease source SHALL be marked unsupported. While it is,
the daemon SHALL NOT call `claim_next` on it. Each firing of a harness bound to
it SHALL be recorded `skipped` with reason `lease_source_unsupported`, and SHALL
spawn nothing. `harness doctor` SHALL show a `fail` row for the source, naming
every missing verb argument and stating that relay needs a Switchboard release
that carries SPEC-0034. The daemon SHALL log one ERROR per source each time the
session initializes unsupported. It SHALL NOT fall back to unfenced claims,
SHALL NOT send `summary` or `artifact` anywhere but their SPEC-0034 arguments,
and SHALL NOT infer a dead letter (REQ-16).

Because Switchboard's tool schemas forbid unknown properties, reading the
schemas is an exact statement of what the server accepts. The daemon SHALL NOT
read the server's version string for this check.

#### Scenario: A server that predates attempt history

- **WHEN** a lease source's server advertises `claim_next` without
  `require_fence` or `claimant` in its input schema
- **THEN** a firing of a harness bound to it makes no `claim_next` call, the
  run is recorded `skipped` / `lease_source_unsupported`, no process starts,
  and `harness doctor` shows a `fail` row naming `claim_next.require_fence` and
  `claim_next.claimant`

#### Scenario: The server is upgraded

- **GIVEN** a lease source marked unsupported
- **WHEN** the server is upgraded and the session re-initializes with the
  SPEC-0034 schemas
- **THEN** the next firing claims with `require_fence: true`, and the doctor row
  clears

#### Scenario: Schema declares the fence

- **WHEN** the server's `heartbeat` schema declares `lease_token`
- **THEN** every heartbeat for the attempt carries the token

### REQ-20: Run Record Fields and Events

Run records (SPEC-0008) of a leased harness SHALL gain:

| Field | Present when | Meaning |
| --- | --- | --- |
| `lease` | Always | The lease source reference |
| `todo_id` | A claim succeeded | The claimed todo |
| `attempt` | A claim succeeded | The attempt number |
| `relay_outcome` | The attempt ended | `completed`, `failed`, `released`, `lease_lost`, `report_lost`, `dead_lettered` or `no_work` |
| `check_exit` | A check ran | The last check exit code |
| `artifact` | A handle was reported | The reported handle |

`skipped` reasons SHALL gain `no_work`, `claim_failed`, `lease_disabled` and
`lease_source_unsupported`.
The daemon SHALL publish `relay_claimed` (todo id, attempt), `relay_reported`
(relay outcome, dead letter) and `relay_lease_lost` events. The additions are
additive, and `ProtoMinor` SHALL be bumped once. A record SHALL NOT carry a
summary, a payload byte, a header value or a token.

#### Scenario: A failed attempt's record

- **WHEN** run 7 of `fixer` claims `td_1` at attempt 2 and fails its check with
  exit 1
- **THEN** run 7's record has `lease = "queue.ci"`, `todo_id = "td_1"`,
  `attempt = 2`, `relay_outcome = "failed"` and `check_exit = 1`

### REQ-21: Visibility

* `harness describe` SHALL show, for a leased harness, its lease source, the
  source's session state, and the in-flight attempt's todo id, attempt number
  and lease deadline.
* `harness doctor` SHALL call `tools/list` on each bound lease source and SHALL
  flag: a missing `claim_next`, `heartbeat`, `complete` or `fail`; a missing
  `release` (as a notice); a server without the REQ-19 capabilities (as a
  `fail`, naming each missing argument); and a lease
  source bound to a harness whose channel trigger points at a different `url`
  (as a warning, because a doorbell for one endpoint cannot be claimed on
  another).
* When SPEC-0013's endpoint is enabled, the daemon SHALL export
  `harness_relay_attempts_total{harness,outcome}` (outcome from
  `relay_outcome`) and `harness_relay_heartbeat_failures_total{harness}`.

#### Scenario: Doctor on a read-only endpoint

- **WHEN** a lease source's endpoint advertises only `list_todos`
- **THEN** `harness doctor` reports that `claim_next`, `heartbeat`, `complete`
  and `fail` are missing

### REQ-22: Pools

Several leased harnesses MAY bind the same lease source. Each SHALL claim
independently, as REQ-5 specifies, and SHALL rely on Switchboard's claim
semantics to receive distinct todos. The daemon SHALL NOT coordinate claims
between them.

#### Scenario: Three workers, two todos

- **GIVEN** three leased harnesses bound to one channel and one lease source
- **WHEN** one doorbell fires all three while two todos are pending
- **THEN** two attempts start on different todos, and the third records
  `skipped` / `no_work`

### REQ-23: Concurrency Safety

Lease client calls, heartbeats, success checks and receipt uploads SHALL run
off the supervisor's actor loop, and SHALL deliver their results to it as
messages. Every such operation SHALL take a context that is cancelled when its
attempt ends, and every goroutine an attempt starts SHALL have exited by the
time the attempt's run record is closed. Shared state between the heartbeat
loop and the actor SHALL be passed by message, not shared memory. The relay's
tests SHALL run under the race detector in CI.

#### Scenario: No leaked heartbeat

- **WHEN** 100 attempts run to completion in a test
- **THEN** the goroutine count returns to its baseline after the last run record
  closes

### REQ-24: Error Handling Standards

Errors from the lease client, the success check and the receipt upload SHALL be
wrapped with the lease source or harness name and the operation. Switchboard
error codes (`conflict`, `not_found`, `forbidden`, `invalid`) SHALL be mapped to
distinct sentinel errors so the attempt logic can branch on them. No error SHALL
be silently dropped: each SHALL change the attempt's outcome, be retried under
REQ-7 or REQ-10, or be logged with structured key-value context that contains
no credential.

#### Scenario: A conflict is not a transport error

- **WHEN** a heartbeat returns `conflict`
- **THEN** the attempt follows REQ-8 at once, and does not enter the transport
  retry path

---
status: approved
date: 2026-09-22
implements: [ADR-0028]
extends: [SPEC-0008]
requires: [SPEC-0002, SPEC-0003, SPEC-0013, SPEC-0014]
---

# SPEC-0022: Run History Ledger

## Overview

The daemon keeps a durable, append-only **run ledger**: one record per run, for
every harness. A one-shot's run is one firing (SPEC-0008, SPEC-0014); a resident
harness's run is one process lifetime, from spawn to exit. Each record carries
the trigger, the times, the exit code, an outcome class, and, once the observer
has seen the run's agent activity, the model that served it, its tokens and
cost, its error counts, its sessions and a link to its trace. See ADR-0028.

The ledger is the one source of run facts. Metrics (SPEC-0013), budgets
(SPEC-0021), `harness jobs` and `harness runs` all read it, and no counter reads
run outcomes from the lifecycle bus. The observer remains the source of agent
activity; this spec folds that activity into run records through a usage
accumulator.

This spec amends, by reference and without editing them:

* **SPEC-0008 REQ "Run History"**: records exist for resident harnesses too, the
  outcome set gains `budget_exceeded`, `quota_parked`, `model_mismatch` and
  `model_unattested`, and
  `cancelled` covers any deliberate stop with a `reason` (REQ-5). The bounded
  `state.json` history is removed; the ledger replaces it (REQ-13).
* **SPEC-0008 REQ "Protocol Operations"** and **SPEC-0002 REQ "Control
  Operations"**: the `runs` op gains a query and NAME becomes optional (REQ-15).
* **SPEC-0013 REQ-4**: `harness_scheduled_runs_total` is counted from committed
  ledger records instead of the lifecycle bus, and new run series are added
  (REQ-11).

The key words MUST, MUST NOT, SHALL, SHALL NOT, SHOULD and MAY are used as in
RFC 2119.

## Requirements

### REQ-1: Ledger location and format

The ledger SHALL live in `$XDG_STATE_HOME/harness/ledger/` (the directory of
`state.json`, plus `ledger/`), created with mode 0700. It SHALL consist of one
file per UTC calendar day named `YYYY-MM-DD.jsonl`, each created with mode 0600.
Each line SHALL be one JSON object terminated by `\n` and no longer than 16 KiB.
Every line SHALL carry `v` (the schema version, `1`), `seq` (a ledger-wide
integer that increases by one per line and never repeats, including across
restarts), `type`, `at` (RFC 3339 with nanoseconds, UTC), `harness` and
`run_id`. A line is written to the file of the UTC day of its `at`.

#### Scenario: A first run creates the ledger

- **GIVEN** a daemon with no `ledger/` directory
- **WHEN** a harness starts
- **THEN** `ledger/` exists with mode 0700, today's file exists with mode 0600, and its first line is an `opened` line with `seq` 1

#### Scenario: Sequence survives a restart

- **GIVEN** a ledger whose last line has `seq` 812
- **WHEN** the daemon restarts and writes a line
- **THEN** that line has `seq` 813

#### Scenario: Another user cannot read it

- **WHEN** a different local user tries to read a ledger file
- **THEN** the operating system refuses, because the directory is 0700 and the file 0600

### REQ-2: Line types and folding

A line's `type` SHALL be one of:

| `type` | Written when | Carries |
| --- | --- | --- |
| `opened` | a run is admitted, before its process spawns | the record's identity and start fields |
| `updated` | a usage or link checkpoint | only the fields that changed |
| `closed` | a run ends, by any path | end fields, final usage |
| `decided` | a record that started no process (`skipped`, `missed`) | the whole record |

A run's record SHALL be the fold, in `seq` order, of every line with its
`(harness, run_id)`: later values replace earlier ones field by field. A reader
SHALL ignore fields it does not know, SHALL skip a line that does not parse
(counting it), and SHALL treat a `closed` or `updated` line with no `opened`
(pruned) as a partial record.

#### Scenario: A run's three lines fold into one record

- **GIVEN** an `opened` line, two `updated` lines with growing token counts, and a `closed` line for `pr-review` run 7
- **WHEN** `harness runs pr-review --json` reads them
- **THEN** it returns one record for run 7 with the last token counts and the closed outcome

#### Scenario: A torn last line

- **GIVEN** a daemon killed mid-write, leaving half a JSON object at the end of today's file
- **WHEN** the daemon boots
- **THEN** the torn line is skipped and counted, the next append starts on a new line, and every complete line still folds

### REQ-3: Every run gets a record

The daemon SHALL open a ledger record for every process spawn of every harness,
and close it at every way the process ends: natural exit, spawn failure, timeout,
replace, operator stop or restart, operating-hours close, budget stop, quota park,
reload removal and daemon shutdown. For a one-shot, the record is the SPEC-0008
run record and uses its `run_id`. For a resident, each spawn SHALL allocate the
next `run_id` from the same per-harness sequence and SHALL carry a trigger of
`autostart`, `restart` (the supervisor restarting after an exit), `release` (a
hold cleared: hours opened, a park expired, the budget day rolled), `lease`, or
`manual`. Records with no process (`skipped`, `missed`) SHALL be written as
`decided` lines.

#### Scenario: A resident crash loop

- **GIVEN** an enabled resident harness that exits 1 three seconds after every start
- **WHEN** it has started four times
- **THEN** the ledger holds four closed records with exit code 1 and outcome `failed`, with triggers `autostart`, `restart`, `restart`, `restart`, and consecutive run ids

#### Scenario: An operating-hours close

- **GIVEN** a resident harness with `operating_hours` that is closed gracefully at 13:00
- **WHEN** its process stops
- **THEN** its record closes with outcome `cancelled` and reason `hours`, and the next window's start opens a new record with trigger `release`

#### Scenario: A spawn failure

- **GIVEN** a harness whose executable does not exist
- **WHEN** it is started
- **THEN** its record closes with outcome `failed`, no exit code, and reason `spawn`

### REQ-4: Record fields

A folded record SHALL expose these fields, each omitted when it does not apply:

| Field | Type | Meaning |
| --- | --- | --- |
| `harness`, `run_id` | string, integer | identity |
| `kind` | `oneshot` or `resident` | |
| `trigger` | string | REQ-3 and SPEC-0008/SPEC-0014 values |
| `source`, `event_id` | string | SPEC-0014 REQ "Run Record Fields" |
| `todo_id`, `attempt` | string, integer | a Switchboard todo, from a channel notification's `meta.todo_id` (SPEC-0014) or a supervisor-held lease |
| `started_at`, `ended_at`, `duration_ms` | RFC 3339, integer | as SPEC-0008 |
| `exit_code` | integer | set only when a process was reaped; -1 if signalled |
| `outcome`, `reason` | string | REQ-5 |
| `model` | string | the served model with the most output tokens |
| `models` | list of `{model, provider, output_tokens}` | every served model and provider |
| `tokens` | `{input, output, cache_read, cache_write}` | integers |
| `cost_usd`, `cost_source` | decimal, `recorded`/`priced`/`unknown` | SPEC-0021 REQ-8 |
| `model_calls` | integer | successful model calls (tool calls, as SPEC-0013 counts them) |
| `errors` | map of SPEC-0013 class to integer | classified model errors |
| `usage_complete` | boolean | false if the observer dropped items for this harness during the run |
| `sessions` | list of `{id, adapter, trace_id}` | attributed agent sessions |
| `trace_url` | string | REQ-9 |
| `log` | string | the per-run log path (one-shot) or the durable log path (resident) |
| `log_pruned` | boolean | the per-run log was deleted by `keep_runs` |
| `override` | boolean | admitted with `--over-budget` (SPEC-0021 REQ-15) |
| `mismatch` | `{kind, served_model, served_provider, at}` | the first mismatching call of a `model_mismatch` record (SPEC-0020); `kind` is `model` or `provider` |
| `window`, `first_window`, `windows`, `coalesced` | as SPEC-0008 and SPEC-0014 | |

Every string field SHALL be capped at 256 bytes, `models` and `sessions` at 16
entries each.

#### Scenario: A webhook run on a todo

- **GIVEN** a channel notification with `meta = {todo_id: "t1"}` fires run 3 of `sb-drain`
- **WHEN** run 3 is recorded
- **THEN** its record has trigger `channel`, the channel's `source`, the notification's `event_id` and `todo_id` `t1`

#### Scenario: An oversized todo id

- **WHEN** a notification's `meta.todo_id` is 4 KiB long
- **THEN** the record stores its first 256 bytes and a doctor-visible truncation count increases

### REQ-5: Outcome classes

A closed record's `outcome` SHALL be one of SPEC-0008's `success`, `failed`,
`timed_out`, `skipped`, `replaced`, `missed`, `cancelled` or `interrupted`, or
one of:

| Outcome | Set by | Means |
| --- | --- | --- |
| `budget_exceeded` | SPEC-0021 REQ-9, REQ-10 | stopped for crossing a per-run or daily cap |
| `quota_parked` | SPEC-0021 REQ-13 | ended by a provider quota refusal that parked the harness |
| `model_mismatch` | SPEC-0020 (model pinning) | the served model or provider did not match the harness's pin |
| `model_unattested` | SPEC-0020 (model pinning) | a full-attestation pin found no model or provider evidence for a call, or its final check timed out |

`running` SHALL mark a record in flight. `cancelled` SHALL mean a run ended by a
deliberate stop, with `reason` `operator`, `hours` or `reload`. A resident whose
process exits on its own SHALL read `success` for exit 0 and `failed` otherwise.
`reason` SHALL be set for `skipped` (SPEC-0014's `overlap`, `stopping` and
`outside_hours`; SPEC-0021's `quota_parked`, `budget` and `concurrency`; and
SPEC-0020's `model_hold`),
`cancelled`, `budget_exceeded` (the cap), `failed` when the process never spawned
(`spawn`), and `interrupted` (`shutdown` or `daemon_crash`).

For SPEC-0013's `harness_scheduled_runs_total{outcome}` and for SPEC-0008's
consecutive-failure count, `success` SHALL count as success; `failed`,
`timed_out`, `budget_exceeded`, `model_mismatch` and `model_unattested` SHALL
count as failure; and
every other outcome, `quota_parked` included, SHALL count as neither.

#### Scenario: A model mismatch counts as a failure

- **GIVEN** a scheduled harness whose last run closed `model_mismatch`
- **WHEN** `harness jobs` renders it
- **THEN** its consecutive failures include that run, and `harness_scheduled_runs_total{outcome="failure"}` counted it

#### Scenario: A quota park is not a verdict

- **WHEN** a scheduled run closes `quota_parked`
- **THEN** it neither increments nor resets the harness's consecutive failures

### REQ-6: Write ordering and durability

`opened`, `closed` and `decided` lines SHALL be written with `O_APPEND` and synced
to stable storage before the caller proceeds: an `opened` line before the process
spawns, and a `closed` or `decided` line before the record is published to the run
feed (REQ-10), to lifecycle events, or to the control protocol. `updated` lines
MAY be buffered and SHALL be synced at least every 30 seconds while any are
pending, and before the run's `closed` line. The single writer SHALL be the
Manager's run journal; no other component SHALL write the ledger.

A sync SHALL be bounded by 2 seconds. If a write or sync fails or times out, the
daemon SHALL log the failure with the harness and `seq`, count it
(`harness_ledger_append_errors_total`), keep the line queued, and retry it in
order. SPEC-0021 REQ-4 decides whether admission proceeds. Supervision of a
harness subject to no budget SHALL NOT wait on the retry.

#### Scenario: The opened line is on disk before the spawn

- **GIVEN** a one-shot fired at 09:00
- **WHEN** the daemon is killed with SIGKILL the instant after the process spawns
- **THEN** the ledger holds that run's `opened` line

#### Scenario: A read-only disk

- **GIVEN** the ledger's filesystem becomes read-only
- **WHEN** an unbudgeted resident harness exits and restarts
- **THEN** it restarts without waiting, the append error is logged and counted, `harness doctor` reports it, and the queued lines are written in order when the filesystem recovers

### REQ-7: Crash reconciliation

On boot, before admitting any start, the daemon SHALL find every record with an
`opened` line and no `closed` line, and append a `closed` line for it with
outcome `interrupted`, reason `daemon_crash`, and no `ended_at`. It SHALL append
a line saying so to the run's log when the run has one, as SPEC-0008 REQ "Run
History" requires of `state.json` records. A clean daemon shutdown SHALL close
every open record with outcome `interrupted`, reason `shutdown` and an
`ended_at`.

#### Scenario: A daemon killed mid-run

- **GIVEN** run 4 of `nightly` is in flight when the daemon is killed with SIGKILL
- **WHEN** the daemon boots
- **THEN** run 4's record reads `interrupted` with reason `daemon_crash` and no end time, and the next run is run 5

### REQ-8: Usage accumulation

The daemon SHALL run a usage accumulator that subscribes to the observer and,
for each item attributed to a harness with an open run, folds it into that run:

* a usage item (stump.wtf/agent-trace#105) adds its tokens, adds its cost as
  SPEC-0021 REQ-8 resolves it, and adds its `(model, provider)` to `models`;
  a cumulative item (crush session totals) SHALL be differenced against the last
  total seen for that session, and a total lower than the last one SHALL be
  treated as a new baseline, not a negative delta;
* a tool event increments `model_calls`;
* an error mark increments `errors[class]` using SPEC-0013 REQ-3's classifier;
* a new session adds `{id, adapter, trace_id}` to `sessions`.

An item SHALL be attributed to the run open for its harness at the item's time.
An item with no open run SHALL be dropped and counted. If the observer reports a
drop for the accumulator's subscription while a run is open, the run's
`usage_complete` SHALL be false. The accumulator SHALL write `updated` lines no
more often than every 30 seconds per run, and SHALL fold its final totals into
the `closed` line. Before agent-trace#105 is available, records SHALL carry
`model_calls`, `errors` and `sessions`, and no `tokens`, `cost_usd` or `models`.

#### Scenario: A crush resident resumed across restarts

- **GIVEN** a crush session whose totals read 10,000 prompt tokens when run 5 starts and 14,000 when it ends
- **WHEN** run 5 closes
- **THEN** run 5's `tokens.input` is 4,000, not 14,000

#### Scenario: The observer drops items

- **GIVEN** the accumulator's subscriber buffer overflows during run 9
- **WHEN** run 9 closes
- **THEN** its record has `usage_complete: false`, and `harness doctor` reports the drops

### REQ-9: Trace and todo links

Each `sessions` entry SHALL carry the trace id that SPEC-0015's traces signal
(ADR-0022) assigns to that session, or the id agent-trace's
`otel.BuildTrace` would assign when SPEC-0015 is not implemented. When
`[ledger] trace_url` is set, the record's `trace_url` SHALL be that template with
`{trace_id}` replaced by the first session's trace id; the template SHALL NOT
support any other substitution. An exporter that returns a URL for a run MAY
record it with an `updated` line, which SHALL take precedence over the template.
`todo_id` SHALL be taken from a channel notification's `meta.todo_id`, or from a
supervisor-held lease (ADR-0025) when that lease claimed the todo; it SHALL be
stored as an opaque string and never interpreted.

#### Scenario: A Grafana link

- **GIVEN** `trace_url = "https://grafana.example.com/explore?traceId={trace_id}"` and a run whose session's trace id is `4bf92f3577b34da6a3ce929d0e0e4736`
- **WHEN** the run is listed with `--json`
- **THEN** `trace_url` is `https://grafana.example.com/explore?traceId=4bf92f3577b34da6a3ce929d0e0e4736`

#### Scenario: A template with another placeholder

- **WHEN** `trace_url = "https://x.example/{harness}/{trace_id}"`
- **THEN** the config load fails, naming the unsupported placeholder

### REQ-10: The run feed

After a `closed` or `decided` line is synced (and after an `opened` line, for
consumers that track runs in flight), the journal SHALL publish the folded record
to every subscriber of the run feed. Publishing SHALL NOT block: a subscriber
whose buffer is full SHALL miss that record, and the miss SHALL be counted per
subscriber. Each published record SHALL carry its `seq`. The journal SHALL offer
a replay, `Since(seq)`, returning every line after `seq` in order from memory or
from the files, so that a consumer that must not miss a record (ADR-0025's lease
completion, a sweep) can resume from the last `seq` it processed. No metric or
budget counter SHALL count run outcomes from the lifecycle bus; the bus's run
events SHALL remain for clients and the TUI.

#### Scenario: A slow subscriber does not stall supervision

- **GIVEN** a feed subscriber that never reads
- **WHEN** 1,000 runs close
- **THEN** no run's close is delayed, that subscriber's miss count reads 1,000, and every other subscriber received every record

#### Scenario: A lossless consumer resumes

- **GIVEN** a consumer that processed through `seq` 500 before the daemon restarted
- **WHEN** it calls `Since(500)` after the restart
- **THEN** it receives every line from `seq` 501 on, in order, including those written while it was down

### REQ-11: Metrics from the ledger

When SPEC-0013's endpoint is enabled, the daemon SHALL export, counted from the
run feed:

```
harness_runs_total{harness,kind,outcome}          counter
harness_run_duration_seconds{harness,kind}        histogram  closed runs with an ended_at
harness_run_tokens_total{harness,type}            counter    type: input|output|cache_read|cache_write
harness_run_cost_usd_total{harness,source}        counter    source: recorded|priced
harness_ledger_append_errors_total                counter
harness_ledger_bytes                              gauge
harness_run_feed_dropped_total{subscriber}        counter
```

`harness_scheduled_runs_total` (SPEC-0013 REQ-4) SHALL be counted from the run
feed with REQ-5's success and failure mapping, and SHALL NOT read the lifecycle
bus. Every declared harness SHALL report every `kind`/`outcome` pair it can
produce, including zeros. The metrics subscriber's buffer SHALL be sized so that
it does not drop under the load the acceptance test applies, and its drops SHALL
be visible in `harness_run_feed_dropped_total{subscriber="metrics"}`. Series SHALL
follow SPEC-0013 REQ-5's cardinality cap.

#### Scenario: The CLI and the endpoint agree

- **GIVEN** 12 runs of `pr-review` closed today, 3 of them `failed`
- **WHEN** `/metrics` is scraped and `harness runs pr-review --since 24h --outcome failed --json` is run
- **THEN** the increase of `harness_runs_total{harness="pr-review",outcome="failed"}` since the daemon started equals the number of records the CLI returns for the same period

### REQ-12: Retention

The global `harness.toml` MAY set `[ledger] retention` (a duration of at least
`1d`, default `90d`) and `[ledger] max_mb` (an integer of at least 16, default
256). At boot and after each UTC day rollover, the daemon SHALL delete day files
whose day ended more than `retention` ago, then the oldest remaining files until
the total is at most `max_mb`, never deleting today's file. Before deleting a file
that holds the `opened` line of a run still open, it SHALL write a fresh
`opened` line for that run into today's file. `keep_runs` SHALL continue to bound
per-run logs only; a record whose per-run log was deleted SHALL read
`log_pruned: true`.

#### Scenario: Old files go

- **GIVEN** `retention = "30d"` and day files going back 45 days
- **WHEN** the daemon boots
- **THEN** the files older than 30 days are gone and the rest are intact

#### Scenario: A resident open across the prune

- **GIVEN** a resident run opened 100 days ago and still running, with `retention = "90d"`
- **WHEN** the prune runs
- **THEN** today's file gains an `opened` line for that run before the old file is deleted, and the run still folds into a record

#### Scenario: A record outlives its log

- **GIVEN** `keep_runs = 5` and a one-shot's run 1 whose log was pruned
- **WHEN** `harness runs NAME` lists run 1
- **THEN** it is listed with `log pruned`, and `harness logs NAME --run 1` says the log was pruned

### REQ-13: Run history leaves `state.json`

The ledger SHALL be the only run history. The daemon SHALL NOT write SPEC-0008
run records to `state.json`; each harness's `state.json` entry SHALL keep only
`last_run_id`, the run id allocator. `jobs`, `trigger --wait`, `logs --run` and
the `runs` op SHALL read run records from the ledger, and SHALL behave as
SPEC-0008 specifies for the fields they show. There SHALL be no projection, no
dual write and no compatibility path for a daemon that predates this spec.

On the first boot with no `ledger/` directory, the daemon SHALL import each
harness's existing `state.json` history into the ledger as `decided`/`closed`
lines marked `imported: true`, exactly once, and SHALL then drop the record list
from `state.json`. The implementing change SHALL carry an upgrade note in the
release notes saying that run history now lives only in `ledger/` and that
`keep_runs` bounds only per-run logs.

#### Scenario: Upgrading a daemon with history

- **GIVEN** a `state.json` holding 20 runs for `nightly`, written before this spec
- **WHEN** the upgraded daemon boots
- **THEN** the ledger holds those 20 runs marked `imported`, `harness runs nightly` lists them, `state.json` holds no run records for `nightly` but keeps its `last_run_id`, and a second boot imports nothing

#### Scenario: jobs reads the ledger

- **GIVEN** `nightly`'s last run failed and `state.json` holds no run records
- **WHEN** the operator runs `harness jobs`
- **THEN** `nightly`'s row shows the failed run's id, time and outcome, read from the ledger

#### Scenario: Run ids continue

- **GIVEN** an upgraded daemon whose `state.json` records `last_run_id = 20` for `nightly`
- **WHEN** `nightly` next fires
- **THEN** its run id is 21

### REQ-14: `harness runs`

The CLI SHALL accept:

```
harness runs [NAME...] [--harness NAME]... [--since DUR|TIME] [--until TIME]
             [--outcome O[,O...]] [--trigger T[,T...]] [--limit N] [--wide] [--json]
```

* With a single NAME and no other filter, it SHALL behave as SPEC-0008 specifies
  (newest first, default `--limit 20`), with the new columns.
* With no NAME and no `--harness`, it SHALL list every harness's records from the
  last 24 hours, newest first, default `--limit 50`.
* `--since` SHALL accept a duration (`7d`, `36h`) or an RFC 3339 instant;
  `--until` an instant. `--outcome` and `--trigger` SHALL reject unknown values
  with an error listing the valid ones.
* The table SHALL fit 80 columns with HARNESS, RUN, STARTED, TOOK, TRIGGER,
  OUTCOME and EXIT; `--wide` SHALL add MODEL, TOKENS, COST and TODO.
* `--json` SHALL print a JSON array of REQ-4 records, newest first.
* It SHALL exit 0 when the query succeeded, including with no records, and
  non-zero only when it could not read the ledger.

#### Scenario: A morning sweep

- **WHEN** a sweep runs `harness runs --since 24h --outcome failed,timed_out,interrupted --json`
- **THEN** it receives every such record across every harness, each with its `todo_id` where known

#### Scenario: An unknown outcome

- **WHEN** the operator runs `harness runs --outcome timeout`
- **THEN** it fails, listing the valid outcomes including `timed_out`

#### Scenario: Back-compat

- **GIVEN** a script that runs `harness runs nightly --limit 5 --json`
- **WHEN** it runs against the new daemon
- **THEN** it receives the same five records as before, with additional fields only

### REQ-15: The `runs` op

The `runs` control op SHALL accept an optional query: `names`, `since`, `until`,
`outcomes`, `triggers`, `limit` (1–1000) and `before_seq` for paging, and SHALL
return records newest first with the `seq` of the oldest returned record. A
request with only `name` and `limit` SHALL behave as SPEC-0008 specifies. The
daemon SHALL serve records of the last 7 days from memory and older ones by
reading day files in range. An old client's request SHALL keep working.

#### Scenario: Paging

- **GIVEN** 2,500 records in range
- **WHEN** a client requests `limit 1000` three times, passing the returned `seq` as `before_seq`
- **THEN** it receives 1,000, 1,000 and 500 records with no duplicates and no gaps

### REQ-16: Offline read

When the daemon is unreachable, `harness runs` SHALL read the ledger files
directly, read-only, apply the same filters and fold, print a warning on stderr
saying it read the ledger from disk, and SHALL NOT modify any file (no
reconciliation, no pruning). An open record SHALL be shown as `running?`, since
the CLI cannot tell a live run from one a crashed daemon left.

#### Scenario: After a crash

- **GIVEN** the daemon crashed during run 4 and has not restarted
- **WHEN** the operator runs `harness runs nightly`
- **THEN** it lists run 4 as `running?`, warns that the daemon is not running, and leaves the ledger unchanged

### REQ-17: Privacy

The ledger SHALL NOT contain environment values, `env_file` contents, prompt
text, agent output, event payloads, header values or credentials (ADR-0008).
Error counts SHALL be counts by class, never error text.

#### Scenario: A harness with secrets in its env file

- **GIVEN** a harness whose `env_file` holds an API key and whose prompt contains a token
- **WHEN** it runs and its record is written
- **THEN** no ledger line contains either value

### REQ-18: Doctor

`harness doctor` SHALL report the ledger directory and its permissions, its size
against `max_mb`, the oldest day kept, append errors and queued lines, skipped
(unparseable) lines, truncated fields, feed drops per subscriber, accumulator
drops, and whether the first-boot import ran. Each warning SHALL be shown firing
in a test.

#### Scenario: A ledger with wrong permissions

- **GIVEN** the ledger directory's mode is 0755
- **WHEN** the operator runs `harness doctor`
- **THEN** it warns that the ledger is readable by other users and names the path

### REQ-19: Configuration and reload

`[ledger]` SHALL be accepted only in the global `harness.toml`; a project file or
drop-in carrying it SHALL be rejected. A reload SHALL apply `retention` and
`max_mb` at the next prune and `trace_url` to records closed after the reload,
without reopening the ledger or restarting any harness.

#### Scenario: A project file with a ledger table

- **WHEN** a project `harness.toml` carries `[ledger]`
- **THEN** it is rejected with an error saying the ledger is configured globally

### REQ-20: Error handling and concurrency safety

The journal, the accumulator and the query path SHALL:

* wrap errors with the harness, `run_id` and `seq` at each boundary, and define
  sentinel errors `ErrLedgerUnavailable` and `ErrLedgerCorrupt`;
* never swallow an error: every failure is returned, logged with structured
  fields, or explicitly handled with a comment saying why;
* keep one writer goroutine for the ledger, with every other component sending
  it work, and never block the supervisor's actor loop beyond the bounded sync
  REQ-6 allows;
* stop cleanly on daemon shutdown, draining queued lines within the shutdown
  timeout, and be tested under the race detector in CI.

#### Scenario: Shutdown drains the queue

- **GIVEN** three `updated` lines buffered when the daemon receives SIGTERM
- **WHEN** it shuts down
- **THEN** all three, and the `closed` lines for every open run, are synced before it exits

## Out of Scope

* Per-run log files for resident harnesses.
* Shipping the ledger anywhere beyond the run feed, telemetry export and `--json`.

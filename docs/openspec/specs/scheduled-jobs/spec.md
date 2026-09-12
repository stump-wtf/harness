---
status: active
date: 2026-07-26
implements: [ADR-0013]
extends: [SPEC-0003]
requires: [SPEC-0002]
---

# SPEC-0008: Scheduled One-Shot Runs

## Overview

Recurring, non-interactive work — nightly sweeps, weekly grooming, daily syncs —
runs under the same daemon that supervises resident harnesses, on a schedule the
daemon itself owns. A **scheduled harness** is an ADR-0011 prompt one-shot
carrying a `schedule` cron expression: the daemon fires it on a cadence, and the
run exiting is terminal for that firing.

Scheduling is expressed as a key on the existing `[harness.*]` table rather
than as a distinct table kind. See ADR-0013 for that decision and for the list
of capability still deferred. Suspend-safe evaluation, missed-window handling,
`catch_up`, and time zones were added by #117; run history, per-run logs, run
timeouts, and the overlap policy by #119; the protocol and CLI surface that
reads and triggers them by #120.

This spec does **not** amend SPEC-0003. The restart-policy axis ADR-0013
originally called for shipped independently as the `restart` key, and SPEC-0003
REQ "Restart On Exit" is already conditional on it.

## Requirements

### Requirement: Schedule Key

The daemon SHALL accept an optional `schedule` key on a `[harness.*]` table in
the global `harness.toml`. Its value SHALL be a 5-field cron expression
(`min hour dom mon dow`), one of the `@daily`/`@hourly`/`@weekly`/`@monthly`/
`@yearly` descriptors, or `@every <duration>`, optionally preceded by a zone
prefix as specified in REQ "Schedule Time Zone". The daemon SHALL also accept an
optional boolean `catch_up` key, specified in REQ "Missed Window Handling", and
the optional run keys `timeout` (REQ "Run Timeout"), `on_overlap` (REQ "Overlap
Policy") and `keep_runs` (REQ "Run History").

The daemon SHALL validate the expression at config-parse time using the same
parser the scheduler uses at registration time, so a value accepted by the parser
cannot be rejected by the scheduler. A harness whose `schedule` is present SHALL
be referred to as a *scheduled harness*.

#### Scenario: Minimal valid scheduled harness

- **WHEN** a config declares `[harness.sweep]` with `prompt` and
  `schedule = "0 */6 * * *"` set
- **THEN** the daemon registers the harness and arms its schedule

#### Scenario: Zone-prefixed schedule

- **WHEN** a `[harness.*]` table sets `schedule = "CRON_TZ=UTC 0 9 * * *"`
- **THEN** the config parses and the harness carries the value verbatim

#### Scenario: Invalid cron expression

- **WHEN** a `[harness.*]` table sets `schedule` to an unparseable expression
- **THEN** config parsing fails with an error naming the harness and quoting the
  offending value

#### Scenario: Blank schedule

- **WHEN** a `[harness.*]` table sets `schedule` to a whitespace-only string
- **THEN** config parsing fails with an error naming the harness

### Requirement: Schedule Exclusions

Because `schedule` marks a lifecycle that is incompatible with several existing
keys, the daemon SHALL reject each combination below as a config-parse error
naming the harness and the offending key. These exclusions are the mechanism by
which a key on `[harness.*]` remains unambiguous (ADR-0013).

| Rejected combination | Rationale |
| --- | --- |
| `schedule` without `prompt` or `prompt_file` | A scheduled unit is a one-shot agent run, not an always-on `cmd` |
| `schedule` with `enabled = true` | Autostart intent and schedule are distinct concerns |
| `schedule` on a harness that is a `[profile.*]` member | Profile autostart would fire the one-shot off-schedule |
| `schedule` with `restart = "always"` or `"unless-stopped"` | A respawning policy restarts the one-shot after a clean exit |
| `schedule` in a project `harness.toml` | Project harnesses never enter the daemon's config view, so the schedule could never fire |
| `catch_up` without `schedule`, or in a project `harness.toml` | A missed-window policy with no schedule to apply to does nothing |
| `timeout`, `on_overlap` or `keep_runs` without `schedule`, or in a project `harness.toml` | Run keys shape scheduled runs; without a schedule there are none |

`enabled` SHALL retain its SPEC-0003 meaning of *autostart intent*; this spec
SHALL NOT redefine it to mean *armed*.

#### Scenario: Schedule without prompt

- **WHEN** a `[harness.*]` table sets `schedule` alongside `cmd` rather than
  `prompt` or `prompt_file`
- **THEN** config parsing fails with an error naming the harness and the missing
  prompt source

#### Scenario: Schedule with a prompt file

- **WHEN** a `[harness.*]` table sets `schedule` alongside `prompt_file`
- **THEN** the config parses, because either prompt source makes the harness a
  one-shot agent run (SPEC-0006 REQ "Prompt Source")

#### Scenario: Schedule with autostart intent

- **WHEN** a `[harness.*]` table sets both `schedule` and `enabled = true`
- **THEN** config parsing fails with a mutual-exclusion error

#### Scenario: Schedule with explicit disable

- **WHEN** a `[harness.*]` table sets `schedule` and `enabled = false`
- **THEN** the config parses successfully and the schedule is armed

#### Scenario: Scheduled harness in a profile

- **WHEN** a `[profile.*]` table lists a harness that carries a `schedule`
- **THEN** config parsing fails with an error naming the profile and the harness

#### Scenario: Respawning restart policy

- **WHEN** a `[harness.*]` table sets `schedule` with `restart = "always"` or
  `restart = "unless-stopped"`
- **THEN** config parsing fails with an error naming the rejected policy

#### Scenario: On-failure restart policy

- **WHEN** a `[harness.*]` table sets `schedule` with `restart = "on-failure"`
- **THEN** the config parses successfully; the policy applies only to abnormal
  exit

#### Scenario: Schedule in a project file

- **WHEN** a project `harness.toml` declares a harness with `schedule`
- **THEN** parsing fails with an error directing the operator to the daemon's
  global `harness.toml`

#### Scenario: Catch-up without a schedule

- **WHEN** a `[harness.*]` table sets `catch_up`, true or false, without
  `schedule`
- **THEN** config parsing fails with an error naming the harness and stating
  that `catch_up` requires `schedule`

### Requirement: Firing And Overlap

At each firing the daemon SHALL start the scheduled harness if no run is in
flight. A firing that arrives while a run is in flight SHALL be handled by the
harness's overlap policy (REQ "Overlap Policy"). A firing arriving during a
graceful stop SHALL be recorded `skipped` and SHALL NOT resurrect the harness or
restore its enabled intent.

The daemon SHALL fire when the harness is `stopped`, `failed`, or `restarting`; a
firing from `failed` SHALL clear the failed latch through the ordinary start
path.

A firing SHALL never stack a second concurrent process for the same harness,
under any overlap policy.

A firing naming a harness the daemon does not know SHALL be logged and otherwise
be a no-op.

#### Scenario: Firing while a run is in flight

- **WHEN** a schedule fires while the harness is `running` under the default
  `on_overlap = "skip"`
- **THEN** the daemon records the firing `skipped` and does not spawn a second
  process

#### Scenario: Firing during a graceful stop

- **WHEN** a schedule fires while the harness is `stopping`
- **THEN** the daemon skips the firing and the stop completes normally

#### Scenario: Firing after a failed run

- **WHEN** a schedule fires while the harness is `failed`
- **THEN** the daemon starts a fresh run and the failed latch is cleared

### Requirement: Suspend-Safe Schedule Evaluation

The daemon SHALL decide whether a window is due by comparing the wall clock
against each armed schedule's next window on a fixed short tick, and SHALL NOT
hold a timer longer than that tick. Comparisons SHALL use wall-clock time only,
never a monotonic clock reading, because a monotonic clock does not advance
while the host is suspended. Startup SHALL evaluate immediately, through the
same path as every later tick.

Starting a run SHALL NOT block evaluation: a slow or hung start for one harness
SHALL NOT delay evaluating, or firing, any other.

Every read of the current time and every timer the scheduler holds SHALL go
through an injectable clock, so each scenario in this spec is testable without
waiting in real time.

#### Scenario: On-time firing

- **WHEN** a window comes due while the daemon is running and the host is awake
- **THEN** the harness starts within one tick of the window

#### Scenario: Repeated interval

- **WHEN** a harness sets `schedule = "@every 1s"` and ten seconds elapse
- **THEN** it fires once per elapsed window, and a tick in which no new window
  elapsed fires nothing

#### Scenario: Slow start

- **WHEN** one harness's start blocks indefinitely
- **THEN** other harnesses due on the same or later ticks still start on time

#### Scenario: No long timers

- **WHEN** the scheduler is running
- **THEN** the longest timer it holds is the tick interval

### Requirement: Missed Window Handling

A window first evaluated more than a one-minute grace after it was due is
*missed*: the host was suspended, the daemon was down, or the wall clock jumped
forward. The `catch_up` key on a scheduled harness (default `false`) decides
what happens to missed windows:

- With `catch_up = true`, the daemon SHALL start the harness exactly once,
  however many windows were missed.
- With `catch_up = false`, the daemon SHALL NOT start the harness, and SHALL
  record exactly one missed entry naming the first and last missed window and
  how many there were.

If the most recent elapsed window is still inside the grace, the daemon SHALL
fire it as an ordinary on-time firing; only the older windows are missed, and
under `catch_up = true` that one run covers them. Several windows elapsing
within the grace — a tick landing a few seconds late — SHALL coalesce into one
on-time firing and SHALL NOT be recorded as missed.

The daemon SHALL persist, per scheduled harness, the expression and the last
window it decided (fired, caught up, or recorded missed), and SHALL write that
record durably before starting the run it accounts for. On startup the daemon
SHALL resume each schedule from its record, so windows that elapsed while it
was down are handled exactly as a wake handles them. A record taken under a
different expression SHALL be discarded. A schedule with no usable record SHALL
arm from the current time and persist that.

The daemon SHALL NOT decide the same window twice — including after a crash and
restart, and after a backwards step of the wall clock.

A schedule that a reload removes SHALL NOT be evaluated again, even if its
window elapsed before the reload was applied, and its record SHALL be
discarded.

A missed entry SHALL be recorded in the harness's run history with outcome
`missed` (REQ "Run History"), and SHALL also be logged as a warning in the
daemon log.

#### Scenario: Suspend across several windows with catch-up

- **WHEN** a harness with `schedule = "0 * * * *"` and `catch_up = true` is
  suspended from 01:30 until 06:30
- **THEN** on wake it starts exactly once, and the 07:00 window fires normally

#### Scenario: Suspend across several windows without catch-up

- **WHEN** the same harness has `catch_up = false`
- **THEN** on wake it starts nothing and records one missed entry covering the
  five windows from 02:00 to 06:00

#### Scenario: Daemon started after an outage

- **WHEN** the daemon was down across those windows and starts at 06:30
- **THEN** it reaches exactly the decision a wake at 06:30 would have

#### Scenario: Waking just after a window

- **WHEN** the host wakes 20 seconds after 03:00, having slept through 02:00
- **THEN** 03:00 fires as an on-time firing, and 02:00 is recorded missed
  unless `catch_up = true`

#### Scenario: Crash right after a firing

- **WHEN** the daemon crashes just after starting the 03:00 run and restarts at
  03:00:05
- **THEN** it does not start the 03:00 run again

#### Scenario: Wall clock steps backwards

- **WHEN** the wall clock steps back an hour after the 03:00 run
- **THEN** passing 03:00 again does not start it again

#### Scenario: Expression changed while down

- **WHEN** a harness's expression changed while the daemon was down
- **THEN** no missed entry is recorded for the new expression's windows

#### Scenario: Disarmed before evaluation

- **WHEN** a reload removes a harness's `schedule` after its window elapsed but
  before a tick evaluated it
- **THEN** nothing starts and nothing is recorded

### Requirement: Schedule Time Zone

A `schedule` expression MAY begin with a `CRON_TZ=<zone>` or `TZ=<zone>` prefix
naming an IANA time zone. The daemon SHALL evaluate a prefixed expression in
that zone regardless of its own local zone, and SHALL evaluate an unprefixed
expression in its local zone. An unknown zone SHALL fail config parsing like any
other invalid expression, and zone names SHALL resolve identically on every
host, including one with no system zoneinfo database.

An expression that fires in every hour of the day, and any `@every` interval,
SHALL be a cadence in real time: a DST transition SHALL neither add nor drop a
run. An expression restricted to particular hours names wall-clock times, and
each of its wall-clock windows SHALL run exactly once: a window a spring-forward
transition skips SHALL run at the instant the offset in force before the
transition names, and a window a fall-back transition repeats SHALL run only at
its first occurrence.

Where a surface renders a cadence label for a prefixed expression, a label that
names a time of day or a calendar boundary SHALL carry the zone name; a label
that is a pure cadence SHALL NOT.

#### Scenario: Prefixed expression on a daemon in another zone

- **WHEN** a harness sets `schedule = "CRON_TZ=UTC 0 9 * * *"` and the daemon's
  local zone is `America/Los_Angeles`
- **THEN** its next firing is 09:00 UTC

#### Scenario: Unprefixed expression

- **WHEN** a harness sets `schedule = "0 9 * * *"`
- **THEN** it fires at 09:00 in the daemon's local zone

#### Scenario: Unknown zone

- **WHEN** a harness sets `schedule = "CRON_TZ=Mars/Olympus_Mons 0 9 * * *"`
- **THEN** config parsing fails with an error naming the harness and quoting the
  value

#### Scenario: Window inside a spring-forward gap

- **WHEN** a harness scheduled `30 2 * * *` in `America/New_York` crosses the
  day 02:00 jumps to 03:00
- **THEN** it runs exactly once that day, at 03:30 EDT, and at 02:30 the next day

#### Scenario: Window in a repeated hour

- **WHEN** a harness scheduled `30 1 * * *` in `America/New_York` crosses the
  day 02:00 falls back to 01:00
- **THEN** it runs exactly once that day, at the first 01:30

#### Scenario: Hourly cadence across a transition

- **WHEN** a harness scheduled `0 * * * *` crosses either transition
- **THEN** it runs once per real hour, with no hour skipped or doubled

#### Scenario: Zone-qualified label

- **WHEN** a surface labels `CRON_TZ=UTC 0 9 * * *` and `CRON_TZ=UTC 0 */6 * * *`
- **THEN** they read `daily 09:00 UTC` and `every 6h`

### Requirement: Run History

The daemon SHALL keep, per scheduled harness, a bounded history of run records
`{run_id, trigger, outcome, started_at, ended_at, exit_code}`, together with the
schedule window a record honors and, for a missed record, the first window and
the number of windows it covers. The history records what the daemon decided,
not only what executed:

| Outcome | Meaning | Process |
| --- | --- | --- |
| `success` | Exited 0 on its own | Yes |
| `failed` | Exited non-zero on its own, or never spawned (no exit code) | Yes, or spawn failed |
| `timed_out` | Killed for outliving `timeout` | Yes |
| `skipped` | A firing the overlap policy dropped, or one arriving mid-stop | No |
| `replaced` | Stopped so another run could start (`on_overlap = "replace"`, or an operator restart) | Yes |
| `missed` | Windows that elapsed while nobody was evaluating, with `catch_up = false` | No |
| `cancelled` | A run, or a held firing, ended by an operator stop | Yes, or held |
| `interrupted` | A run, or a held firing, the daemon went down under | Yes, or held |

A record SHALL read `running` while its run is in flight. `trigger` SHALL be
`schedule` (on time), `manual` (an operator start or restart), or `catch_up`.

`run_id` SHALL be a per-harness integer that increases monotonically and is
never reused — across daemon restarts, and even after a history is lost, for as
long as any of its per-run logs remain.

The history SHALL be bounded by the `keep_runs` key (an integer of at least 1,
default 20). The oldest finished records SHALL fall off first; a run in flight
SHALL never be dropped.

History SHALL persist in the daemon's state file and be written before the run
it records starts. On startup, a record still reading `running` belongs to a
daemon that died under it: it SHALL be reconciled to `interrupted` with its end
time left unset, and a line recording that SHALL be appended to its log. A
malformed history for one harness SHALL cost only that harness its records, and
the rest of the state file SHALL still load; a state file that does not parse
at all SHALL be copied aside before the daemon replaces it. A state file written
before run history existed SHALL load unchanged.

Records SHALL NOT contain environment values, `env_file` contents, prompt text,
or output (ADR-0008).

#### Scenario: Clean and failing exits

- **WHEN** a scheduled run exits 0, and another exits 3
- **THEN** their records read `success` with exit code 0 and `failed` with exit
  code 3, each carrying its trigger, window, start and end

#### Scenario: Run ids across a restart

- **WHEN** a harness has run twice and the daemon restarts
- **THEN** its next run is run 3

#### Scenario: History bound

- **WHEN** a harness with `keep_runs = 2` has run four times
- **THEN** its history holds runs 3 and 4, and only their logs remain

#### Scenario: Crash mid-run

- **WHEN** the daemon crashes during run 4 and starts again
- **THEN** run 4 reads `interrupted` with no end time, its log says so, and the
  next run is run 5

#### Scenario: Malformed history

- **WHEN** one harness's history in the state file does not decode
- **THEN** that harness starts with no records, and every other harness, the
  active profile, and all persisted intent load normally

#### Scenario: No secrets in history

- **WHEN** a scheduled harness with an `env_file` runs
- **THEN** neither its records nor the state file contain any `env_file` value

### Requirement: Per-Run Logs

Each run that starts a process SHALL write its sanitized output history and its
lifecycle lines to `$XDG_STATE_HOME/harness/jobs/<harness>/<run_id>.log`, in
addition to the harness's rotating log (ADR-0007). The file SHALL be readable
only by the daemon's user. It SHALL be closed on every path a run ends by:
natural exit, spawn failure, timeout, replace, operator stop, operator restart,
and daemon shutdown.

When a record falls out of the history its log SHALL be deleted, and a run log
no record refers to SHALL be removed, so records and logs cannot drift apart. A
record with no process (`skipped`, `missed`) has no log.

#### Scenario: Output lands in the run log

- **WHEN** a run prints a line and exits
- **THEN** its run log holds that line between a run-started and a run-finished
  line

#### Scenario: Log closed on every exit path

- **WHEN** runs end by success, failure, timeout, replace, stop, and daemon
  shutdown
- **THEN** every run log that was opened has been closed

### Requirement: Run Timeout

The `timeout` key on a scheduled harness — a duration string, default `"1h"`,
`"0"` for no limit — SHALL bound each run, whatever started it. When a run
outlives it, the daemon SHALL send SIGTERM to the run's process group, SHALL
send SIGKILL if the group has not exited within the stop grace, SHALL record the
run `timed_out`, and SHALL leave the harness `failed`.

#### Scenario: Run exceeds its timeout

- **WHEN** a run with `timeout = "30m"` is still running 30 minutes after it
  started
- **THEN** its process group is sent SIGTERM, the run is recorded `timed_out`,
  and the harness is `failed`

#### Scenario: Process ignores SIGTERM

- **WHEN** a timed-out run's process ignores SIGTERM
- **THEN** it is sent SIGKILL once the stop grace elapses, and is still recorded
  `timed_out`

#### Scenario: Invalid timeout

- **WHEN** a scheduled harness sets `timeout = "soon"` or a negative duration
- **THEN** config parsing fails with an error naming the harness and the value

### Requirement: Overlap Policy

The `on_overlap` key on a scheduled harness SHALL decide what a firing does
while a run is in flight:

- `skip` (default): the firing is recorded `skipped` and starts nothing.
- `queue`: the firing is held, and starts when the run in flight ends by
  success, failure, or timeout. At most one firing SHALL be held; a further
  firing is recorded `skipped`. A held firing dropped by an operator stop is
  recorded `cancelled`, and by a daemon shutdown `interrupted`.
- `replace`: the run in flight is stopped gracefully and recorded `replaced`,
  and the new run starts.

The daemon SHALL make the overlap decision atomically with starting the run, so
two firings can never both find the harness idle.

#### Scenario: Queue

- **WHEN** three firings arrive for a `queue` harness while its first run is in
  flight
- **THEN** the second is held and runs once the first ends, and the third is
  recorded `skipped`

#### Scenario: Replace

- **WHEN** a firing arrives for a `replace` harness while a run is in flight
- **THEN** that run is recorded `replaced` and the firing's run starts

#### Scenario: Stop drops the held firing

- **WHEN** an operator stops a `queue` harness that has a firing held
- **THEN** the run in flight and the held firing are both recorded `cancelled`,
  and the held firing never starts

### Requirement: Protocol Operations

The daemon SHALL expose scheduled runs through three control operations
(SPEC-0002 REQ "Control Operations"), each computed from its own records so a
client never evaluates a cron expression or guesses at a run:

| Op | Request | Reply |
| --- | --- | --- |
| `jobs` | — | Every scheduled harness: schedule, state, next window (from the live scheduler), `catch_up`, `timeout`, `on_overlap`, `keep_runs`, the run in flight, the newest finished record, and consecutive failures |
| `trigger` | `name` | The decision — `started`, `queued` or `skipped` — and the run record it made (none while queued) |
| `runs` | `name`, `limit` | The harness's run records, newest first, at most `limit` (default 20) |

`logs` SHALL accept a `run` selector naming one run by id: the raw reply is that
run's own log, and the `events` reply describes exactly that record's window.

Consecutive failures SHALL count the newest records that `failed` or
`timed_out`, back to the latest `success`; records that pass no verdict on the
job (`running`, `skipped`, `missed`, `replaced`, `cancelled`, `interrupted`)
SHALL neither count nor reset the count.

Errors SHALL be distinguishable: an unknown name is `unknown_harness`, a
`trigger` for a harness with no schedule is `not_scheduled`, and a run id the
history does not hold is `unknown_run`. Replies SHALL carry outcomes, times,
exit codes and whether a run has a log — never environment, `env_file` contents,
prompt text or output (ADR-0008).

The protocol minor version SHALL be bumped for these additions and the bump
documented alongside the constant.

#### Scenario: Next run without cron math

- **WHEN** a client requests `jobs`
- **THEN** each scheduled harness carries its next window as an RFC 3339 time
  resolved by the running scheduler, and harnesses without a schedule are absent

#### Scenario: Failure streak

- **WHEN** a scheduled harness's last two runs failed after an earlier success
- **THEN** `jobs` reports two consecutive failures

#### Scenario: Run history newest first

- **WHEN** a client requests `runs` with `limit = 1` for a harness that has run
  twice
- **THEN** the reply holds run 2 only

#### Scenario: Distinguishable errors

- **WHEN** `trigger` names a harness with no schedule, or one that does not exist
- **THEN** the replies are `not_scheduled` and `unknown_harness` respectively

### Requirement: Manual Trigger

`trigger` SHALL start a run of a scheduled harness with trigger `manual` through
the same path a schedule firing takes, so the harness's `on_overlap` policy,
`timeout`, run history and per-run log apply exactly as they would at the
scheduled time.

The CLI verb SHALL be `harness trigger <name>`, since `harness run` starts a
scratchpad (ADR-0017). `harness trigger <name> --wait` SHALL stream the run's log
and exit with the run's exit code; a run that timed out SHALL exit 124, a
trigger recorded `skipped` SHALL exit 75, and any other unsuccessful run with no
usable exit code SHALL exit 1. `harness daemon run` SHALL continue to start the
daemon.

`start` and `stop` SHALL keep their SPEC-0003 meaning on a scheduled harness:
`start` runs it now and `stop` ends the run in flight.

#### Scenario: Trigger during a run

- **WHEN** `trigger` is issued for a harness with the default `on_overlap` while
  a run is in flight
- **THEN** the reply's decision is `skipped` and the history gains a `skipped`
  record, exactly as a schedule firing would

#### Scenario: Waiting on a run

- **WHEN** an operator runs `harness trigger nightly --wait` and the run exits 3
- **THEN** the run's log is streamed and the command exits 3

### Requirement: Lifecycle Events

The daemon SHALL push three events to subscribed clients (SPEC-0002 REQ "Event
Subscription"):

- `job_run_started { name, run_id, trigger }` when a run starts a process;
- `job_run_finished { name, run_id, trigger, outcome, exit_code, duration_ms }`
  when a record becomes final — including a decision that started no process
  (`skipped`, `missed`), and a run whose process failed to spawn;
- `job_schedule_changed { name, next_run_at }` whenever a harness's next window
  moves: armed, re-armed by a reload, advanced past a firing, or disarmed (no
  `next_run_at`).

Events are notifications and MAY be dropped for a slow subscriber; the run
history remains the record. `exit_code` SHALL be omitted, not zero, when no
process exited.

#### Scenario: A run seen live

- **WHEN** a subscribed client is connected while a manual run exits 0
- **THEN** it receives `job_run_started` and then `job_run_finished` with outcome
  `success` and `exit_code` 0 for the same `run_id`

#### Scenario: A reload moves the next window

- **WHEN** a reload changes a harness's `schedule`
- **THEN** subscribers receive `job_schedule_changed` carrying the new next
  window

### Requirement: Run Termination

A scheduled run exiting SHALL be terminal for that firing: the supervisor SHALL
NOT respawn it, and the next run SHALL come only from a subsequent firing or an
explicit operator start. The configured restart policy SHALL apply only to
abnormal exit, and only when the operator set `on-failure`.

#### Scenario: Clean exit

- **WHEN** a scheduled run exits with code 0 under the default `restart = "no"`
- **THEN** the daemon records the exit and does not respawn the process; the
  schedule remains armed

#### Scenario: Failing exit under on-failure

- **WHEN** a scheduled run exits non-zero under `restart = "on-failure"`
- **THEN** the restart policy applies as it does for any harness

### Requirement: Run Execution

A scheduled run SHALL be an ordinary supervised spawn: the same spawn path,
`workdir`, `env_file` loading (ADR-0008), PTY allocation, and `x/vt` emulator
and scrollback ring (ADR-0003) that any prompt harness uses. A client SHALL be
able to attach to an in-flight scheduled run and observe it live.

#### Scenario: Attaching to a scheduled run

- **WHEN** an operator attaches to a scheduled harness while a run is in flight
- **THEN** the attach behaves exactly as it does for any running harness

### Requirement: Schedule Reconciliation On Reload

The daemon SHALL re-apply schedules after every successful config reload,
regardless of which path triggered it — SIGHUP, the config watcher, or the
`reload` control op. The daemon SHALL NOT re-apply schedules after a reload that
fails to parse.

Reconciliation SHALL be incremental. An entry whose harness still declares the
identical expression SHALL retain its existing registration and therefore its
phase; an entry whose expression changed SHALL be re-registered; an entry whose
harness lost its `schedule` or disappeared from the config SHALL be removed. A
change to `catch_up` alone SHALL apply in place without re-registering the
entry.

Preserving phase is REQUIRED, not an optimization: this config file is rewritten
periodically by external tooling whether or not its contents changed, and a
scheduler that rebuilt its entries on each reload would reset every `@every`
interval's countdown and could starve such a schedule indefinitely.

#### Scenario: No-change reload preserves phase

- **WHEN** a config declaring `schedule = "@every 6h"` is reloaded with identical
  content
- **THEN** the entry retains its existing registration identity and its next fire
  time is unchanged

#### Scenario: Catch-up change keeps phase

- **WHEN** a reload changes only a harness's `catch_up`
- **THEN** the entry keeps its registration and next fire time, and the new
  policy applies to the next missed window

#### Scenario: Changed expression re-registers

- **WHEN** a reload changes a harness's `schedule` expression
- **THEN** the old registration is removed and a new one is created under the new
  expression

#### Scenario: Removed schedule disarms

- **WHEN** a reload removes the `schedule` key from a harness, or removes the
  harness entirely
- **THEN** its entry is removed and no further firings occur for it

#### Scenario: Failed reload does not re-apply

- **WHEN** a reload fails to parse
- **THEN** the daemon retains its last-good config and does not re-apply
  schedules

### Requirement: Schedule Round-Trip Through Config Writers

Any surface that rewrites a `[harness.*]` table SHALL preserve the `schedule`,
`catch_up`, `timeout`, `on_overlap`, and `keep_runs` keys. The TUI harness form
rewrites the whole table on save, so it SHALL pre-fill them from the config file
(ADR-0006 file-is-truth) and SHALL re-emit every one that differs from its
default.

A config writer SHALL validate the exclusions in REQ "Schedule Exclusions"
before writing, because the file is written before the daemon parses it and an
invalid combination would leave `harness.toml` unparseable on disk.

#### Scenario: Editing an unrelated field

- **WHEN** an operator edits only the description of a scheduled harness in the
  TUI and saves
- **THEN** the rewritten table still carries the original `schedule` value and
  re-parses to an equivalent harness

#### Scenario: Writer rejects an invalid combination

- **WHEN** a config writer is asked to save a scheduled harness with
  `enabled = true`
- **THEN** it fails validation before writing rather than producing an
  unparseable file

### Requirement: Schedule Visibility

`schedule` and the resolved next-fire time SHALL be readable from a client
without reading `harness.toml`. The daemon SHALL carry both on the harness
projection returned by `list` and `describe`: the cron expression as configured,
and the next firing as an RFC 3339 timestamp taken from the live scheduler
rather than recomputed from the config, since the daemon is the only party that
knows the resolved phase.

A scheduled harness SHALL be visually distinguishable from a disabled one on
every listing surface. A scheduled harness is always `enabled = false` (REQ
"Schedule Exclusions"), so `enabled` alone cannot carry the distinction and
rendering it as merely disabled would misreport an armed cron job as one
somebody turned off. A harness waiting for its next firing SHALL be described
as **armed**, and a surface SHALL NOT report `enabled` for it as though that
were its intent: the schedule is.

Both listing surfaces — the CLI table and the cockpit dashboard — SHALL show
the cadence and the countdown to the next firing **as fields of their own**,
derived from the configured expression and the live scheduler. Neither SHALL
depend on the operator's `description` text to carry, or to highlight, either
value: a description is prose the operator owns, and a schedule stated in it is
freetext pretending to be data. Where a surface cannot paraphrase an expression
into a cadence label it SHALL render the expression verbatim rather than
nothing. A scheduled harness whose next firing the daemon has not resolved SHALL
render no countdown rather than a placeholder time.

#### Scenario: Scheduled harness on the wire

- **WHEN** a client lists or describes a harness carrying `schedule`
- **THEN** the response carries the cron expression and the next firing time as
  resolved by the running scheduler

#### Scenario: Scheduled harness in a listing

- **WHEN** a scheduled harness appears in the CLI table or the cockpit dashboard
- **THEN** it is marked as scheduled, and its cadence and time-to-next-firing
  are both on screen, in fields of their own

#### Scenario: Schedule is not carried by the description

- **WHEN** a scheduled harness's `description` says nothing about its schedule
- **THEN** its cadence and countdown are shown and styled exactly as they are
  for a harness whose description does mention them

#### Scenario: Scheduled harness is not reported as disabled

- **WHEN** a scheduled harness (necessarily `enabled = false`) is rendered
- **THEN** the surface describes it as armed rather than as disabled, and does
  not present `enabled` as its intent

#### Scenario: Unresolved next firing

- **WHEN** a harness carries `schedule` but the daemon has not resolved a next
  firing time
- **THEN** the surface shows the cadence with no countdown, rather than a
  placeholder or a zero time

#### Scenario: Harness in backoff outranks its schedule

- **WHEN** a scheduled harness is also waiting out a restart backoff
- **THEN** the surface's next-action field shows the backoff countdown, and the
  cadence remains visible alongside it

### Requirement: Scheduler Fault Isolation

A panic raised while handling a firing SHALL NOT terminate the daemon. The
scheduler SHALL recover from it, log it, and remain armed for subsequent
firings.

The scheduler SHALL be safe for concurrent use: reconciliation MAY run
concurrently with firings, and shutdown SHALL wait for in-flight firings to
finish.

#### Scenario: Panic in a firing

- **WHEN** the firing callback panics
- **THEN** the daemon continues running and later firings still occur

#### Scenario: Shutdown with a firing in flight

- **WHEN** the daemon shuts down while a firing is being handled
- **THEN** shutdown waits for that firing to finish before completing

### Requirement: Error Handling Standards

Every rejected `schedule` combination SHALL produce a config error naming the
file, the line of the offending table, the harness name, and the specific reason.
A schedule the scheduler cannot register at apply time SHALL be logged and
skipped without aborting reconciliation of the remaining harnesses.

#### Scenario: Located parse error

- **WHEN** any exclusion in REQ "Schedule Exclusions" is violated
- **THEN** the error identifies the file, line, harness, and rejected key

#### Scenario: Registration failure is contained

- **WHEN** one harness's schedule cannot be registered at apply time
- **THEN** it is logged and skipped, and the remaining harnesses are still
  reconciled

## Out Of Scope

The following were specified in this spec's 2026-07-26 draft against the
`[job.*]` design and are **not** part of this revision. ADR-0013's *Deferred*
section tracks them:

* Arming and disarming a schedule from a client. `start` and `stop` keep their
  SPEC-0003 meaning on a scheduled harness (REQ "Manual Trigger").
* Job rows in the TUI cockpit.
* A per-harness `timezone` key. Zones are expressed with a `CRON_TZ=` prefix
  instead (REQ "Schedule Time Zone").
* `scheduled` and `completed` states.
* Project-scoped schedules.

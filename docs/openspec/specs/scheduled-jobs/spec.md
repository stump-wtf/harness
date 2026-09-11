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
of deferred capability — run history, per-run logs, timeouts, and queue/replace
overlap policies are **not** part of this revision. Suspend-safe evaluation,
missed-window handling, `catch_up`, and time zones were added by #117.

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
optional boolean `catch_up` key, specified in REQ "Missed Window Handling".

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

At each firing the daemon SHALL start the scheduled harness if and only if it is
not already active. The daemon SHALL treat `starting`, `running`, `degraded`, and
`stopping` as active and SHALL skip the firing in each of those states; a firing
arriving during a graceful stop SHALL NOT resurrect the harness or restore its
enabled intent.

The daemon SHALL fire when the harness is `stopped`, `failed`, or `restarting`; a
firing from `failed` SHALL clear the failed latch through the ordinary start
path.

Overlapping firings SHALL be skipped, not queued and not stacked. Queue and
replace policies are out of scope for this revision.

A firing naming a harness the daemon does not know SHALL be logged and otherwise
be a no-op.

#### Scenario: Firing while a run is in flight

- **WHEN** a schedule fires while the harness is `running`
- **THEN** the daemon skips the firing, logs it, and does not spawn a second
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

Until run history exists, a missed entry SHALL be recorded as a warning in the
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

Any surface that rewrites a `[harness.*]` table SHALL preserve the `schedule`
and `catch_up` keys. The TUI harness form rewrites the whole table on save, so it
SHALL pre-fill both from the config file (ADR-0006 file-is-truth) and SHALL
re-emit them.

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
somebody turned off.

Both listing surfaces — the CLI table and the cockpit dashboard — SHALL show
the cadence and the countdown to the next firing. Where a surface cannot
paraphrase an expression into a cadence label it SHALL render the expression
verbatim rather than nothing. A scheduled harness whose next firing the daemon
has not resolved SHALL render no countdown rather than a placeholder time.

#### Scenario: Scheduled harness on the wire

- **WHEN** a client lists or describes a harness carrying `schedule`
- **THEN** the response carries the cron expression and the next firing time as
  resolved by the running scheduler

#### Scenario: Scheduled harness in a listing

- **WHEN** a scheduled harness appears in the CLI table or the cockpit dashboard
- **THEN** it is marked as scheduled, and its cadence and time-to-next-firing
  are both on screen

#### Scenario: Scheduled harness is not reported as disabled

- **WHEN** a scheduled harness (necessarily `enabled = false`) is rendered
- **THEN** the surface marks it scheduled rather than disabled

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

* Run history, per-run logs, and a `keep_runs` retention key — including a
  durable `missed` record; REQ "Missed Window Handling" logs misses until it
  lands.
* `timeout` and timed-out run outcomes.
* `on_overlap = "queue" | "replace"`.
* A per-harness `timezone` key. Zones are expressed with a `CRON_TZ=` prefix
  instead (REQ "Schedule Time Zone").
* `scheduled` and `completed` states, and a `consecutive_failures` counter.
* Protocol operations `jobs`, `run`, and `runs`, and `job_run_*` events.
  Exposure of `schedule` and next-fire time over the protocol is **no longer**
  deferred — it shipped as REQ "Schedule Visibility" above (issues #160, #205).
  ([#160](https://gitea.stump.rocks/stump.wtf/harness/issues/160)).
* Project-scoped schedules.

---
status: draft
date: 2026-07-26
implements: [ADR-0013]
extends: [SPEC-0003]
requires: [SPEC-0002]
---

# SPEC-0008: Scheduled One-Shot Jobs

## Overview

Recurring, non-interactive work — nightly sweeps, weekly grooming, daily syncs —
run under the same daemon that supervises resident harnesses, on a schedule the
daemon itself owns. A **job** is a supervised unit whose exits are terminal:
where a harness restarts on any exit (SPEC-0003 REQ "Restart On Exit"), a job
runs to completion and waits for its next tick. See ADR-0013 for the decision
and `design.md` for the state machine, scheduler loop, and storage layout.

This spec **extends SPEC-0003**: it introduces restart policy as an explicit
axis and, in doing so, narrows SPEC-0003 REQ "Restart On Exit" from a universal
claim to one conditional on policy `always`. No existing harness changes
behavior.

## Requirements

### Requirement: Job Definition Schema

The daemon SHALL accept `[job.*]` tables in `harness.toml` as a sibling table
kind to `[harness.*]` (ADR-0006). A job table SHALL reuse the harness field
vocabulary — `cmd` (REQUIRED), `args`, `workdir`, `env_file`, `description` —
with identical semantics, and SHALL additionally accept `schedule` (REQUIRED),
`timezone`, `timeout`, `on_overlap`, `catch_up`, `keep_runs`, and `enabled`.

The parser SHALL reject, as a validation error naming the offending key,
`restart_delay` and `backend = "tmux"` in a `[job.*]` table: a job never
auto-restarts, and no tmux session survives a completed run. A job name SHALL
NOT collide with a harness name in the same config.

#### Scenario: Minimal valid job

- **WHEN** a config declares `[job.sweep]` with `cmd` and `schedule` set
- **THEN** the daemon registers the job and applies defaults for every
  unspecified optional key

#### Scenario: Job missing a schedule

- **WHEN** a `[job.*]` table omits `schedule`
- **THEN** config parsing fails with an error naming the job and the missing key

#### Scenario: Harness-only key on a job

- **WHEN** a `[job.*]` table sets `restart_delay` or `backend = "tmux"`
- **THEN** config parsing fails with an error naming the job and the rejected key

#### Scenario: Name collision across table kinds

- **WHEN** a config declares both `[harness.sweep]` and `[job.sweep]`
- **THEN** config parsing fails with a duplicate-name error

### Requirement: Restart Policy

Every supervised unit SHALL carry a restart policy: `always` for units declared
in `[harness.*]`, `never` for units declared in `[job.*]`. The table kind
determines the policy; it SHALL NOT be settable as a config key in this
revision. SPEC-0003 REQ "Restart On Exit" SHALL apply only to units with policy
`always`. A unit with policy `never` SHALL NOT be respawned on exit by the
supervisor; only the scheduler or an explicit manual trigger starts a new run.

#### Scenario: Job exits cleanly

- **WHEN** a job's run exits with code 0
- **THEN** the daemon records the outcome and does NOT respawn the process

#### Scenario: Job exits with failure

- **WHEN** a job's run exits with a non-zero code
- **THEN** the daemon records the failure and does NOT respawn the process; the
  schedule remains armed and the next tick fires normally

#### Scenario: Harness behavior is unchanged

- **WHEN** a unit declared in `[harness.*]` exits with code 0 while enabled
- **THEN** it is restarted per SPEC-0003 REQ "Restart On Exit", unchanged by
  this spec

### Requirement: Job State Model

The daemon SHALL track each job in exactly one of seven states: `scheduled`
(`◷`), `starting` (`◌`), `running` (`●`), `stopping` (`◌`), `completed` (`✔`),
`failed` (`✖`), and `stopped` (`○`). Jobs SHALL NOT enter `degraded` or
`restarting`, which are crash-loop states meaningful only under policy `always`.

For a job, the orthogonal `enabled` boolean of SPEC-0003 REQ "State Model" SHALL
mean *the schedule is armed*, not *a process should be up*. `failed` for a job
SHALL mean *the most recent run failed* and SHALL NOT suppress future ticks —
distinct from a harness's `failed`, which means the daemon gave up.

The daemon SHALL maintain a `consecutive_failures` counter per job, incremented
on each failed or timed-out run and reset on each successful run. The daemon
SHALL NOT take automatic action based on this counter; it is surfaced for
rendering only.

#### Scenario: Resting state

- **WHEN** a job is armed and no run is in flight
- **THEN** its state is `scheduled` and its next fire time is available to
  clients

#### Scenario: Failure does not disarm

- **WHEN** a job run fails and the job remains `enabled`
- **THEN** its state is `failed`, `consecutive_failures` increments, and the
  next scheduled tick still fires

#### Scenario: Recovery clears the counter

- **WHEN** a job whose `consecutive_failures` is greater than zero completes a
  run with exit code 0
- **THEN** its state becomes `completed` and `consecutive_failures` resets to
  zero

#### Scenario: Disarmed job

- **WHEN** an operator stops a job
- **THEN** its state becomes `stopped`, `enabled` becomes false, and no tick
  fires until it is started again

### Requirement: Schedule Expression And Timezone

The `schedule` key SHALL accept a 5-field cron expression
(`minute hour day-of-month month day-of-week`), the descriptors `@yearly`,
`@annually`, `@monthly`, `@weekly`, `@daily`, `@midnight`, and `@hourly`, and
the interval form `@every <duration>`. An unparseable expression SHALL be a
config validation error naming the job.

Schedules SHALL be evaluated in the job's `timezone` (an IANA zone name),
defaulting to the daemon host's local zone. On a daylight-saving transition, a
wall-clock time that does not occur SHALL fire once at the next valid instant,
and a wall-clock time that occurs twice SHALL fire once, on the first
occurrence.

#### Scenario: Invalid expression

- **WHEN** a job declares `schedule = "not a cron"`
- **THEN** config parsing fails with an error naming the job and the expression

#### Scenario: Spring-forward transition

- **WHEN** a job scheduled at a wall-clock time skipped by a DST spring-forward
  reaches that day
- **THEN** it fires exactly once, at the next valid instant

#### Scenario: Fall-back transition

- **WHEN** a job scheduled at a wall-clock time that occurs twice on a DST
  fall-back reaches that day
- **THEN** it fires exactly once, at the first occurrence

### Requirement: Suspend-Safe Schedule Evaluation

The scheduler SHALL determine due jobs by re-evaluating each armed job's next
fire time against the wall clock on a bounded interval. It MUST NOT rely on
long-armed timers to fire at their scheduled instant, because a host suspend
makes timer behavior across the sleep window OS-dependent. The scheduler SHALL
take an injectable clock so temporal behavior is testable without real elapsed
time.

#### Scenario: Host sleeps through a window

- **WHEN** the host suspends before a job's fire time and resumes after it
- **THEN** the scheduler reaches a correct decision from the wall clock on its
  next evaluation, rather than depending on whether the OS delivered a pending
  timer

#### Scenario: Clock is injectable

- **WHEN** tests advance an injected clock past a job's fire time
- **THEN** the scheduler fires the job without any real time elapsing

### Requirement: Missed Window Handling

The daemon SHALL persist `last_run_at` per job (SPEC-0003 / ADR-0007 state
file). When the daemon starts, and whenever schedule evaluation observes that
one or more fire times elapsed while the daemon was not running or the host was
suspended, it SHALL apply the job's `catch_up` setting, which defaults to
`false`:

- `catch_up = true` — the daemon SHALL start exactly **one** run, regardless of
  how many fire times elapsed.
- `catch_up = false` — the daemon SHALL NOT start a run, and SHALL record one
  run-history entry with outcome `missed`.

A `missed` entry SHALL be recorded in both cases so that a window that did not
run on time is always visible.

#### Scenario: Catch-up enabled after a long outage

- **WHEN** a job with `catch_up = true` and an hourly schedule has been unable
  to run for six hours and the daemon starts
- **THEN** exactly one run starts, not six

#### Scenario: Catch-up disabled

- **WHEN** a job with `catch_up = false` has a fire time elapse while the daemon
  is down, and the daemon starts afterward
- **THEN** no run starts, a run-history entry with outcome `missed` is recorded,
  and the job waits for its next scheduled fire time

### Requirement: Overlap Policy

The `on_overlap` key SHALL accept `skip` (default), `queue`, or `replace`, and
SHALL govern what happens when a fire time arrives while a run of the same job
is in flight:

- `skip` — the daemon SHALL NOT start a run and SHALL record an entry with
  outcome `skipped`.
- `queue` — the daemon SHALL hold at most **one** pending run; a fire time
  arriving while a run is already queued SHALL be recorded `skipped`. The queued
  run SHALL start when the in-flight run ends.
- `replace` — the daemon SHALL terminate the in-flight run per SPEC-0003 REQ
  "Graceful Stop", record it with outcome `replaced`, and then start the new run.

Any other value SHALL be a config validation error.

#### Scenario: Overrunning job skips its next tick

- **WHEN** a job with `on_overlap = "skip"` is still running at its next fire
  time
- **THEN** no second process is spawned and a `skipped` entry is recorded

#### Scenario: Queue depth is bounded at one

- **WHEN** a job with `on_overlap = "queue"` is running with one run already
  queued and a further fire time arrives
- **THEN** the further tick is recorded `skipped` and the queue depth stays at
  one

#### Scenario: Replace terminates the incumbent

- **WHEN** a job with `on_overlap = "replace"` is running at its next fire time
- **THEN** the in-flight run receives SIGTERM, is escalated to SIGKILL if it
  survives the grace period, is recorded `replaced`, and the new run starts

### Requirement: Run Timeout

The `timeout` key SHALL bound a single run's wall-clock duration and SHALL
default to one hour. The literal value `"0"` SHALL mean unlimited. When a run
exceeds its timeout, the daemon SHALL terminate it per SPEC-0003 REQ "Graceful
Stop" and record the run with outcome `timed_out`, which SHALL count as a
failure for state and `consecutive_failures` purposes.

#### Scenario: Hung run is reaped

- **WHEN** a run exceeds its configured `timeout`
- **THEN** the daemon sends SIGTERM, escalates to SIGKILL after the grace
  period, records outcome `timed_out`, and the job's state becomes `failed`

#### Scenario: Unlimited opt-in

- **WHEN** a job sets `timeout = "0"`
- **THEN** the daemon does not impose a duration bound on its runs

### Requirement: Run Execution

A run SHALL be spawned through the same supervisor path as a harness process:
under a daemon-owned PTY (ADR-0003) in the job's `workdir`, with `env_file`
loaded per ADR-0008. A run SHALL be attachable while in flight, using the
existing attach session machinery of SPEC-0002, so an operator can watch a
scheduled run as it executes. Secrets loaded from `env_file` MUST NOT be written
to run history, per-run logs metadata, or protocol payloads.

#### Scenario: Attach to an in-flight run

- **WHEN** an operator attaches to a job whose state is `running`
- **THEN** they receive a screen snapshot followed by the live stream, per
  SPEC-0002 REQ "Attach Session"

#### Scenario: Attach to an idle job

- **WHEN** an operator attaches to a job whose state is `scheduled`,
  `completed`, `failed`, or `stopped`
- **THEN** the daemon returns a structured error indicating no run is in flight

### Requirement: Manual Trigger

The daemon SHALL expose an operation that starts a run of a named job
immediately, independent of its schedule and of whether it is armed. A manually
triggered run SHALL execute the identical code path as a scheduled run and SHALL
be recorded with trigger `manual`. A manual trigger SHALL be subject to the
job's `on_overlap` policy.

The client SHALL support waiting for a manually triggered run to finish,
streaming its output and exiting with the run's exit code.

#### Scenario: Trigger a disarmed job

- **WHEN** an operator triggers a job whose `enabled` is false
- **THEN** the run executes and the job remains disarmed afterward

#### Scenario: Exit code propagation

- **WHEN** an operator triggers a job in waiting mode and the run exits with
  code 3
- **THEN** the client exits with code 3

#### Scenario: Manual trigger respects overlap

- **WHEN** an operator triggers a job with `on_overlap = "skip"` while a run is
  in flight
- **THEN** no second run starts and the client is told the trigger was skipped

### Requirement: Run History

The daemon SHALL retain a bounded, per-job history of run records in its state
file (ADR-0007), each containing `run_id`, `started_at`, `ended_at`,
`exit_code`, `outcome`, and `trigger`. `run_id` SHALL be a monotonically
increasing per-job integer that survives daemon restarts. `outcome` SHALL be one
of `success`, `failed`, `timed_out`, `skipped`, `replaced`, `missed`, or
`cancelled`. `trigger` SHALL be one of `schedule`, `manual`, or `catch_up`.
History depth SHALL be bounded by `keep_runs`, defaulting to 20, with the
oldest records pruned first.

#### Scenario: History survives a daemon restart

- **WHEN** the daemon restarts after a job has completed three runs
- **THEN** the three run records remain readable and the next run receives
  `run_id` 4

#### Scenario: History is pruned

- **WHEN** a job with `keep_runs = 20` completes its 21st run
- **THEN** the oldest run record is dropped and 20 remain

### Requirement: Per-Run Logs

The daemon SHALL write each run's output to its own file under
`$XDG_STATE_HOME/harness/jobs/<job>/<run_id>.log`, created `0600` in a `0700`
directory per ADR-0008. Log files SHALL be retained in step with run history and
pruned by `keep_runs`. The daemon SHALL serve the most recent run's log by
default and SHALL accept a run selector to serve an earlier one. Requesting a
run whose log has been pruned or that never existed SHALL return a structured
error.

#### Scenario: Default log target

- **WHEN** a client requests logs for a job without specifying a run
- **THEN** the daemon serves the most recent run's log

#### Scenario: Historical run log

- **WHEN** a client requests logs for a specific earlier `run_id` still within
  the retention window
- **THEN** the daemon serves that run's log

#### Scenario: Pruned run log

- **WHEN** a client requests logs for a `run_id` outside the retention window
- **THEN** the daemon returns a structured error rather than an empty stream

### Requirement: Protocol Operations

The control plane (SPEC-0002 REQ "Control Operations") SHALL gain the
operations `jobs` (enumerate jobs with schedule, next fire time, state, last
outcome, and `consecutive_failures`), `run` (trigger a named job, returning its
`run_id`), and `runs` (return a named job's run history). The existing `logs`
operation SHALL gain an OPTIONAL run selector rather than a parallel operation.
The existing `start`, `stop`, and `restart` operations SHALL, when addressed to
a job, arm, disarm, and re-arm its schedule respectively; `stop` SHALL also
cancel any in-flight run per SPEC-0003 REQ "Graceful Stop", recording it with
outcome `cancelled`. No new operations SHALL be added for arming and disarming.

#### Scenario: Enumerating jobs

- **WHEN** a client issues `jobs`
- **THEN** it receives every job with its schedule, next fire time, state, and
  last outcome

#### Scenario: Stop cancels an in-flight run

- **WHEN** a client issues `stop` for a job whose state is `running`
- **THEN** the run is terminated gracefully, recorded `cancelled`, and the job
  becomes `stopped`

#### Scenario: Unknown job

- **WHEN** a client issues `run` for a name that is not a registered job
- **THEN** the daemon returns a structured ERROR with a machine code and human
  message, per SPEC-0002 REQ "Control Operations"

### Requirement: Lifecycle Events

The daemon SHALL emit events (SPEC-0002 REQ "Event Subscription") on job
activity: `job_run_started { name, run_id, trigger }`,
`job_run_finished { name, run_id, outcome, exit_code, duration }`, and
`job_schedule_changed { name, next_run_at }`. `harness_state_changed` SHALL
continue to be emitted for job state transitions so a single subscription covers
both unit kinds.

The daemon SHALL NOT deliver notifications itself — no built-in notifiers and no
per-job failure hooks are in scope. External notification is expected to be
implemented as a client that subscribes to these events.

#### Scenario: Reactive job dashboard

- **WHEN** a scheduled run finishes while a subscribed client is connected
- **THEN** the client receives `job_run_finished` without polling

#### Scenario: Schedule change on reload

- **WHEN** a config reload changes a job's `schedule`
- **THEN** subscribed clients receive `job_schedule_changed` with the
  recomputed next fire time

### Requirement: Config Reload Of Jobs

Changes to a job's definition under hot reload (ADR-0006) SHALL apply per
SPEC-0003 REQ "Config Change Application": an in-flight run SHALL NOT be
disturbed, and changed `cmd`/`args`/`workdir`/`env_file`/`timeout` SHALL take
effect on the next run. A changed `schedule`, `timezone`, or `catch_up` SHALL
take effect immediately, since those govern when the next run occurs rather than
how it executes. A job removed from config SHALL be deregistered; an in-flight
run SHALL be allowed to finish and its history SHALL be retained.

#### Scenario: Schedule edited while a run is in flight

- **WHEN** a reload changes a running job's `schedule`
- **THEN** the in-flight run continues untouched and the next fire time is
  recomputed from the new expression immediately

#### Scenario: Command edited while a run is in flight

- **WHEN** a reload changes a running job's `cmd`
- **THEN** the in-flight run continues with the old command and the next run
  uses the new one

### Requirement: Project-Scoped Jobs

A project file (SPEC-0004) MAY declare `[job.*]` tables. Bringing a project up
SHALL register and arm its jobs under the `<project>/<job>` namespace; taking a
project down SHALL disarm and deregister them and SHALL cancel any in-flight
run. Because project registration is runtime-only (ADR-0009), project jobs SHALL
NOT be re-armed automatically after a daemon restart.

#### Scenario: Project jobs are namespaced

- **WHEN** two projects each declare `[job.sweep]` and both are brought up
- **THEN** both are armed, as `<project-a>/sweep` and `<project-b>/sweep`

#### Scenario: Project teardown cancels a run

- **WHEN** a project is taken down while one of its jobs is running
- **THEN** the run is terminated gracefully, recorded `cancelled`, and the job
  is deregistered

#### Scenario: Project jobs do not survive a restart

- **WHEN** the daemon restarts after a project was brought up
- **THEN** that project's jobs are not registered or armed until the project is
  brought up again

### Requirement: Error Handling Standards

All error-producing operations in the scheduler, run executor, and history/log
store MUST follow structured error handling:

- Errors MUST be wrapped with contextual information at each layer boundary
  (e.g. "job sweep: run 7: opening run log: permission denied")
- Sentinel errors MUST be defined for failure modes callers distinguish
  programmatically — unknown job, run not found, pruned run log, schedule parse
  failure
- Silent error swallowing MUST NOT occur; every error MUST be returned, logged
  with sufficient context, or explicitly handled with a documented reason
- Structured logging MUST be used for error reporting (key-value pairs, not
  string interpolation)

A failure to write a run's log or history record MUST NOT prevent the run's
outcome from being recorded in memory and emitted as an event.

#### Scenario: Log write fails mid-run

- **WHEN** writing a run's log file fails because the state directory is not
  writable
- **THEN** the error is logged with the job name and run id, the run still
  completes, and `job_run_finished` is still emitted

#### Scenario: Distinguishable failure

- **WHEN** a client requests a run whose log has been pruned
- **THEN** the daemon returns a sentinel error distinguishable from "unknown
  job" rather than a generic failure

### Requirement: Concurrency Safety

The scheduler and run executor MUST follow safe concurrency patterns:

- Context propagation MUST be used for cancellation and timeout signaling from
  the scheduler through each run to its process and log writer
- Run lifecycle MUST be explicitly managed — every started run MUST have a
  defined path to termination, and daemon shutdown MUST terminate in-flight runs
  gracefully rather than leaking processes
- Race safety MUST be ensured — job registry, run history, and
  `consecutive_failures` are shared mutable state and MUST be protected by
  synchronization or confined to a single owning task
- Concurrent tests MUST be run with race detection enabled in CI

#### Scenario: Daemon shutdown with runs in flight

- **WHEN** the daemon shuts down while jobs are running
- **THEN** each in-flight run is terminated gracefully, recorded `cancelled`,
  and no process is left orphaned

#### Scenario: Concurrent trigger and tick

- **WHEN** a manual trigger and a scheduled fire time occur simultaneously for
  the same job
- **THEN** the overlap policy is applied exactly once and no two runs start
  concurrently for a job whose policy forbids it

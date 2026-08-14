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

Scheduling is expressed as one key on the existing `[harness.*]` table rather
than as a distinct table kind. See ADR-0013 for that decision and for the
substantial list of deferred capability — run history, per-run logs, timeouts,
missed-window recording, catch-up, timezones, and queue/replace overlap policies
are **not** part of this revision.

This spec does **not** amend SPEC-0003. The restart-policy axis ADR-0013
originally called for shipped independently as the `restart` key, and SPEC-0003
REQ "Restart On Exit" is already conditional on it.

## Requirements

### Requirement: Schedule Key

The daemon SHALL accept an optional `schedule` key on a `[harness.*]` table in
the global `harness.toml`. Its value SHALL be a 5-field cron expression
(`min hour dom mon dow`), one of the `@daily`/`@hourly`/`@weekly`/`@monthly`/
`@yearly` descriptors, or `@every <duration>`.

The daemon SHALL validate the expression at config-parse time using the same
parser the scheduler uses at registration time, so a value accepted by the parser
cannot be rejected by the scheduler. A harness whose `schedule` is present SHALL
be referred to as a *scheduled harness*.

#### Scenario: Minimal valid scheduled harness

- **WHEN** a config declares `[harness.sweep]` with `prompt` and
  `schedule = "0 */6 * * *"` set
- **THEN** the daemon registers the harness and arms its schedule

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
| `schedule` without `prompt` | A scheduled unit is a one-shot agent run, not an always-on `cmd` |
| `schedule` with `enabled = true` | Autostart intent and schedule are distinct concerns |
| `schedule` on a harness that is a `[profile.*]` member | Profile autostart would fire the one-shot off-schedule |
| `schedule` with `restart = "always"` or `"unless-stopped"` | A respawning policy restarts the one-shot after a clean exit |
| `schedule` in a project `harness.toml` | Project harnesses never enter the daemon's config view, so the schedule could never fire |

`enabled` SHALL retain its SPEC-0003 meaning of *autostart intent*; this spec
SHALL NOT redefine it to mean *armed*.

#### Scenario: Schedule without prompt

- **WHEN** a `[harness.*]` table sets `schedule` alongside `cmd` rather than
  `prompt`
- **THEN** config parsing fails with an error naming the harness and the missing
  `prompt`

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
harness lost its `schedule` or disappeared from the config SHALL be removed.

Preserving phase is REQUIRED, not an optimization: this config file is rewritten
periodically by external tooling whether or not its contents changed, and a
scheduler that rebuilt its entries on each reload would reset every `@every`
interval's countdown and could starve such a schedule indefinitely.

#### Scenario: No-change reload preserves phase

- **WHEN** a config declaring `schedule = "@every 6h"` is reloaded with identical
  content
- **THEN** the entry retains its existing registration identity and its next fire
  time is unchanged

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
key. The TUI harness form rewrites the whole table on save, so it SHALL pre-fill
`schedule` from the config file (ADR-0006 file-is-truth) and SHALL re-emit it.

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

* Run history, per-run logs, and a `keep_runs` retention key.
* Missed-window recording and `catch_up`.
* `timeout` and timed-out run outcomes.
* `on_overlap = "queue" | "replace"`.
* Per-harness `timezone` and specified DST behavior.
* Suspend-safe wall-clock evaluation in place of armed timers.
* `scheduled` and `completed` states, and a `consecutive_failures` counter.
* Protocol operations `jobs`, `run`, and `runs`; `job_run_*` events; and
  exposure of `schedule` or next-fire time over the protocol
  ([#160](https://gitea.stump.rocks/stump.wtf/harness/issues/160)).
* Project-scoped schedules.

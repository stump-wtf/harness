---
status: draft
date: 2026-07-18
implements: [ADR-0005, ADR-0006, ADR-0007]
---

# SPEC-0003: Harness Lifecycle State Machine

## Overview

The explicit state machine every harness moves through under daemon
supervision. Today's implementation collapses everything to green/yellow/red by
ANDing "supervisor active" with "tmux session up"; this spec makes the machine
explicit, adds crash-loop safety, and gives the TUI real states to render. See
ADR-0005 (supervision layers) and `design.md` for the transition diagram.

## Requirements

### Requirement: State Model

The daemon SHALL track each harness in exactly one of seven states: `stopped`
(`○`), `starting` (`◌`), `running` (`●`), `degraded` (`◐`), `restarting` (`◌`),
`stopping` (`◌`), and `failed` (`✖`). Separately from `state`, the daemon SHALL
persist an orthogonal `enabled` boolean (does the daemon want this running?) in
its state file (ADR-0007). `enabled` is intent; `state` is reality.

#### Scenario: Intent vs. reality

- **WHEN** a harness exits while `enabled` is true during a restart cycle
- **THEN** the harness MAY briefly report `enabled && stopped` without either
  field being considered inconsistent

### Requirement: Autostart

On daemon start, every harness that is `enabled` — directly or via an
`autostart` profile (ADR-0006) — SHALL transition to `starting`.

#### Scenario: Daemon boot

- **WHEN** the daemon starts and a harness is marked `enabled`
- **THEN** the daemon spawns it (state `starting`) without operator action

### Requirement: Restart On Exit

While a harness is `enabled`, any exit — including a clean exit code 0 — SHALL
be followed by a restart per policy (`restarting`, wait `restart_delay`, then
`starting`), incrementing the restart count (`↻`). If the harness is not
`enabled`, an exit SHALL transition it to `stopped`.

#### Scenario: Clean exit while enabled

- **WHEN** an enabled harness exits with code 0
- **THEN** the daemon restarts it after `restart_delay` (agents and watchers
  are meant to be long-lived)

#### Scenario: Exit while disabled

- **WHEN** a harness exits and `enabled` is false
- **THEN** the harness transitions to `stopped` and is not respawned

### Requirement: Crash-Loop Detection

The daemon SHALL detect crash loops: N exits within a window T transitions the
harness to `degraded`, and `restart_delay` SHALL escalate as a capped
exponential backoff (ADR-0005). A single run surviving longer than T SHALL
reset the counter and return the harness to `running`.

#### Scenario: Flapping harness

- **WHEN** a harness exits N times within window T
- **THEN** its state becomes `degraded` and subsequent restart delays escalate
  exponentially up to the cap

#### Scenario: Recovery resets backoff

- **WHEN** a degraded harness's current run survives longer than window T
- **THEN** the exit counter resets and the state returns to `running`

### Requirement: Backoff Give-Up

After the backoff cap / a maximum-attempts threshold, the daemon SHALL stop
auto-restarting the harness and transition it to `failed`. The TUI surfaces
`failed` loudly (needs a human). A manual `restart` SHALL clear the failed
state and begin a fresh start cycle.

#### Scenario: Giving up

- **WHEN** a degraded harness exhausts its restart attempts
- **THEN** the daemon moves it to `failed` and stops respawning it

#### Scenario: Manual recovery

- **WHEN** an operator issues `restart` on a `failed` harness
- **THEN** the failure latch clears and the harness transitions to `starting`

### Requirement: Graceful Stop

A stop request on a `running`, `degraded`, or `restarting` harness SHALL
transition it to `stopping`: send SIGTERM, wait a grace period, SIGKILL if
needed, tear down the PTY session, then transition to `stopped` and set
`enabled=false`.

#### Scenario: Process ignores SIGTERM

- **WHEN** a stop-requested process is still alive after the grace period
- **THEN** the daemon sends SIGKILL, reaps the PTY, and records `stopped`

### Requirement: Config Change Application

Configuration changes to a running harness (ADR-0006 hot reload) SHALL apply on
the next (re)start, never by silently bouncing the process. The daemon SHALL
expose enough information for the TUI to flag "config changed — restart to
apply."

#### Scenario: Edit while running

- **WHEN** `harnessd.toml` changes a running harness's definition and is
  reloaded
- **THEN** the running process is untouched and the change takes effect on the
  next restart

### Requirement: Lifecycle Events

The daemon SHALL emit protocol events (SPEC-0002) on lifecycle activity:
`harness_state_changed { name, from, to }`, `harness_exited { name, code }`,
and `harness_flapping { name, restarts, next_retry_in }`.

#### Scenario: State change notification

- **WHEN** any harness transitions between states
- **THEN** subscribed clients receive `harness_state_changed` without polling

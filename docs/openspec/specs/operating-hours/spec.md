---
status: draft
date: 2026-09-17
implements: [ADR-0019]
requires: [SPEC-0002, SPEC-0003, SPEC-0008]
---

# SPEC-0012: Operating Hours

## Overview

A resident harness may carry `operating_hours`: weekly windows during which it
is allowed to run. Outside them the daemon holds the harness down: it stops the
process without clearing `enabled`, and starts it again when a window opens.
The purpose is cost. An agent that is not running does not spend tokens. See
ADR-0019 for the decision and `design.md` for the implementation shape.

This spec amends SPEC-0003 REQ "Autostart" and REQ "Restart On Exit". Both now
defer to REQ "Gate Enforcement" below. It reuses the SPEC-0008 scheduler tick
and zone handling (REQ "Suspend-Safe Schedule Evaluation", REQ "Schedule Time
Zone").

Terms used throughout:

* **Gated harness**: a harness whose `operating_hours` is set.
* **In hours**: the wall clock, read in the expression's zone, falls inside at
  least one window.
* **Held**: a gated harness that is `enabled`, `stopped`, and down because it
  is out of hours rather than because of an operator stop or a restart policy.
* **Lease**: an after-hours permission to run, created by a manual start outside
  hours, with a persisted end time.

## Requirements

### Requirement: Operating Hours Key

The daemon SHALL accept an optional string key `operating_hours` on a
`[harness.*]` table in the daemon config of record. Its grammar SHALL be:

```
operating_hours = [ zone-prefix " " ] window *( ";" window )
zone-prefix     = ( "TZ=" / "CRON_TZ=" ) zone-name
window          = [ day-spec " " ] time "-" time
day-spec        = day-item *( "," day-item )
day-item        = day / day "-" day
day             = "Mon" / "Tue" / "Wed" / "Thu" / "Fri" / "Sat" / "Sun"   ; case-insensitive
time            = HH ":" MM                                          ; 00:00 through 24:00
```

Whitespace around `;` and `,` SHALL be ignored. A window without a day spec
SHALL apply to every day. A day range SHALL be inclusive and MAY wrap the week
(`Fri-Mon` is Fri, Sat, Sun, Mon). A window's start is inclusive and its end is
exclusive. `24:00` SHALL be valid only as an end. A window whose end is at or
before its start, other than an end of `24:00`, is an **overnight window**. It
SHALL run from its start on each listed day until its end on the following day.
Windows MAY overlap, and the harness is in hours during their union.

The zone prefix SHALL be resolved exactly as SPEC-0008 REQ "Schedule Time Zone"
resolves a schedule's prefix, including the embedded IANA database. Without a
prefix, the expression SHALL be evaluated in the daemon's local zone.

The daemon SHALL validate the value when parsing the config. A blank value, an
unknown day or zone, a malformed or out-of-range time, and a window whose start
equals its end SHALL each be a parse error naming the harness and the key.

#### Scenario: Weekday window in a named zone

- **WHEN** a harness sets
  `operating_hours = "TZ=America/Los_Angeles Mon-Fri 09:00-13:00"`
- **THEN** it is in hours from 09:00 up to 13:00 Pacific on Monday through
  Friday, whatever zone the daemon runs in

#### Scenario: Lunch break

- **WHEN** `operating_hours = "Mon-Fri 09:00-12:00; Mon-Fri 13:00-17:00"`
- **THEN** a weekday 12:30 is out of hours and 13:00 is in hours

#### Scenario: Overnight window

- **WHEN** `operating_hours = "Fri 22:00-02:00"`
- **THEN** Friday 23:00 and Saturday 01:59 are in hours, and Saturday 02:00 and
  Friday 21:59 are out of hours

#### Scenario: Wrapping day range

- **WHEN** `operating_hours = "Fri-Mon 10:00-11:00"`
- **THEN** 10:30 on Friday, Saturday, Sunday and Monday is in hours, and 10:30
  on Tuesday is not

#### Scenario: Equal start and end

- **WHEN** `operating_hours = "Mon 09:00-09:00"`
- **THEN** config parsing fails with an error naming the harness and
  `operating_hours`

#### Scenario: Unknown zone

- **WHEN** `operating_hours = "TZ=Mars/Olympus 09:00-13:00"`
- **THEN** config parsing fails with an error naming the harness and the zone

### Requirement: Operating Hours Exclusions

The daemon SHALL reject each combination below as a parse error naming the
harness and the offending key.

| Rejected combination | Rationale |
| --- | --- |
| `operating_hours` with `schedule` | A scheduled one-shot is already time-gated by its cron expression |
| `operating_hours` in a project `harness.toml` | Reload reconciliation and lease persistence are defined only for the config of record (ADR-0019 *Deferred*) |

`operating_hours` SHALL be accepted alongside `enabled`, profile membership, any
`restart` policy, `cmd`, and a prompt harness without `schedule`.

#### Scenario: Hours on a scheduled harness

- **WHEN** a harness sets both `schedule` and `operating_hours`
- **THEN** config parsing fails naming the harness and `operating_hours`

#### Scenario: Hours on a profile member

- **WHEN** a harness with `operating_hours` is a member of an autostart profile
- **THEN** the config loads, and the profile decides `enabled` while the hours
  decide when it runs

### Requirement: Gate Evaluation

The daemon SHALL evaluate every gated harness on the scheduler's wall-clock
tick (SPEC-0008 REQ "Suspend-Safe Schedule Evaluation"), comparing the wall
clock with its monotonic component removed. Evaluation SHALL be
level-triggered: each tick SHALL decide from the current time alone whether the
harness is in hours, and SHALL NOT depend on having observed a window boundary.
The daemon SHALL NOT arm a timer to a window boundary.

In-hours membership SHALL be decided on local wall-clock time in the
expression's zone. On a DST transition day, a window SHALL cover whatever local
times the day actually has: a spring-forward gap shortens a window that spans
it, a fall-back repeat lengthens one, and neither case runs a window twice or
skips one.

#### Scenario: Sleeping through a close

- **WHEN** a gated harness is running at 12:50 inside a window ending 13:00, the
  host suspends, and it resumes at 15:00
- **THEN** the first tick after resume finds the harness out of hours and holds
  it

#### Scenario: Waking inside a window

- **WHEN** the host suspends at 08:00 with a held harness and resumes at 10:00
  inside its window
- **THEN** the first tick after resume starts it

#### Scenario: Spring forward

- **WHEN** `operating_hours = "TZ=America/New_York 01:30-02:30"` and the clock
  springs from 01:59:59 EST to 03:00:00 EDT
- **THEN** the harness is in hours at 01:59:59 and out of hours at 03:00:00

#### Scenario: Fall back

- **WHEN** `operating_hours = "TZ=America/New_York 01:00-02:00"` and 01:00–01:59
  local time occurs twice
- **THEN** the harness is in hours during both occurrences

### Requirement: Gate Enforcement

When a tick finds a gated harness out of hours and not covered by a valid lease
(REQ "After-Hours Lease"), and the harness is `starting`, `running`, `degraded`
or `restarting`, the daemon SHALL hold it:

1. cancel any pending respawn;
2. run the SPEC-0003 REQ "Graceful Stop" sequence (SIGTERM, stop grace, SIGKILL,
   PTY teardown) and transition to `stopped`;
3. leave `enabled` unchanged;
4. treat the resulting exit as neither a crash nor a policy exit: no restart
   (overriding SPEC-0003 REQ "Restart On Exit"), no restart-count increment, and
   crash-loop bookkeeping reset.

When a tick finds a gated harness in hours and the harness is held, the daemon
SHALL start it without modifying `enabled`. The daemon SHALL NOT start a gated
harness that is `failed`, or one whose `enabled` is false, when hours open.

On daemon start, SPEC-0003 REQ "Autostart" SHALL start an `enabled` gated
harness only if it is in hours or covered by a valid lease. Otherwise the
harness SHALL begin held.

#### Scenario: Close

- **WHEN** a running, enabled harness with `operating_hours = "09:00-13:00"`
  reaches 13:00
- **THEN** it transitions through `stopping` to `stopped`, `enabled` is still
  true, its restart count is unchanged, and it is not respawned

#### Scenario: Open

- **WHEN** that harness is held and the clock reaches 09:00 the next day
- **THEN** it transitions to `starting` and `enabled` is still true

#### Scenario: Close during a restart delay

- **WHEN** a gated harness is `restarting` (waiting out `restart_delay`) at the
  close
- **THEN** the pending respawn is cancelled and the harness is held

#### Scenario: Failed harness at open

- **WHEN** a gated harness is `failed` when its window opens
- **THEN** it stays `failed`

#### Scenario: Operator-stopped harness at open

- **WHEN** a gated harness was stopped with `harness stop` inside hours
  (`enabled = false`) and the next window opens
- **THEN** it stays `stopped`

#### Scenario: Boot out of hours

- **WHEN** the daemon starts at 20:00 and an enabled harness has
  `operating_hours = "09:00-13:00"` and no lease
- **THEN** the harness is not started and begins held

### Requirement: After-Hours Lease

A `start` control op (SPEC-0002) on a gated harness that is out of hours SHALL
create a lease and start the harness. The op SHALL accept an optional `for`
duration setting the lease length. The default SHALL be one hour, and a
non-positive or unparseable `for` SHALL be rejected. The CLI SHALL expose it as
`harness start NAME --for DURATION`. `for` on a harness that is ungated or in
hours SHALL be rejected with an error saying no lease applies.

The daemon SHALL persist the lease's absolute end time in `state.json` before
starting the harness, and SHALL restore it on boot. While a lease is valid, Gate
Enforcement SHALL NOT hold the harness. When the lease ends, the next tick SHALL
enforce the gate as for a close. When hours open before the lease ends, the
daemon SHALL discard the lease, and the harness continues as an in-hours
harness.

A `start` on a harness that already holds a valid lease SHALL replace the lease
end with a new one computed from the current time and the given (or default)
`for`.

A `stop` control op on a harness running under a valid lease SHALL end the lease
and hold the harness, leaving `enabled` unchanged. A `stop` on a gated harness
without a lease keeps its SPEC-0003 meaning and clears `enabled`.

#### Scenario: Late-night start

- **WHEN** an operator runs `harness start claude-src` at 20:00 outside its
  hours
- **THEN** it starts, `enabled` is true, and it is held at 21:00

#### Scenario: Longer lease

- **WHEN** the operator runs `harness start claude-src --for 3h` at 20:00
- **THEN** it is held at 23:00

#### Scenario: Restart mid-lease

- **WHEN** the daemon restarts at 20:30 during a lease ending 21:00
- **THEN** on boot the harness starts and is held at 21:00, not later

#### Scenario: Stopping a leased harness

- **WHEN** the operator runs `harness stop claude-src` at 20:15 during a lease
- **THEN** the lease ends, the harness is held with `enabled` still true, and
  it starts when the next window opens

#### Scenario: Lease runs into hours

- **WHEN** a lease started at 08:30 would end at 09:30 and the window opens at
  09:00
- **THEN** the lease is discarded at 09:00 and the harness runs until the
  window closes

#### Scenario: `for` on an ungated harness

- **WHEN** `harness start plain --for 2h` targets a harness without
  `operating_hours`
- **THEN** the op fails with an error saying no lease applies, and the harness
  is not started

### Requirement: Operating Hours Reload

`operating_hours` SHALL be applied on the first tick after a successful reload,
without waiting for the harness to restart. Changing it SHALL NOT bounce a
harness that remains in hours. Reconciliation SHALL follow SPEC-0008's
incremental model: an unchanged value keeps its state.

* Adding `operating_hours` to a harness that is out of hours SHALL hold it.
* Removing `operating_hours` from a held harness SHALL start it (it is
  `enabled`), and SHALL discard any lease.
* A lease SHALL survive a reload that changes but does not remove
  `operating_hours`.
* A newly introduced harness (ADR-0014) that is `enabled` and out of hours
  SHALL have its intent recorded as true and SHALL begin held.

#### Scenario: Adding hours in the evening

- **WHEN** at 20:00 a reload adds `operating_hours = "09:00-13:00"` to a
  running enabled harness
- **THEN** on the next tick the harness is held

#### Scenario: Removing hours

- **WHEN** a reload removes `operating_hours` from a held harness
- **THEN** on the next tick it starts

### Requirement: Operating Hours Visibility

The harness projection returned by `list` and `describe` (SPEC-0002) SHALL
carry, for a gated harness:

* `operating_hours`: the expression as configured;
* `in_hours`: whether the gate is open now;
* `hours_next`: the next open (out of hours) or close (in hours), RFC 3339,
  omitted when the expression covers the whole week;
* `held`: whether the harness is held;
* `lease_until`: the lease end, RFC 3339, when a lease is valid.

The daemon SHALL emit `harness_hours_changed { name, in_hours, hours_next }`
when a gated harness's `in_hours` changes. It SHALL write a lifecycle line to
the harness's durable log (ADR-0007) for each hold, open, lease start, and
lease end, stating the reason and the next transition.

Every listing surface (`harness list`, `describe`, the TUI) SHALL show a held
harness as `off-hours` rather than `stopped`, in the not-failed styling used for
an armed scheduled harness (SPEC-0008 REQ "Schedule Visibility"). The NEXT
column SHALL show when it opens, so no new column is added. A gated harness
running in hours SHALL show when it closes, and one under a lease SHALL show the
lease end.

`harness doctor` SHALL warn when a gated harness has `enabled = false` (hours
will never start it) and when an expression covers the whole week (it gates
nothing).

#### Scenario: Listing a held harness

- **WHEN** `harness list` runs at 20:00 with `claude-src` held and a window
  opening Monday 09:00
- **THEN** its STATE reads `off-hours` and its NEXT shows the Monday 09:00 open

#### Scenario: Log explains the stop

- **WHEN** a harness is held at 13:00
- **THEN** its durable log gains a line saying it stopped for operating hours
  and when it next opens

## Out Of Scope

Tracked in ADR-0019 *Deferred*:

* Drain on idle before a close.
* Project-scoped operating hours.
* A daemon-wide or per-profile default.
* Holidays and one-off closures.
* Token or cost budgets.
* Holding or replaying push events and doorbells that arrive while a harness is
  held. That belongs to the sender (for example Switchboard), not the daemon.

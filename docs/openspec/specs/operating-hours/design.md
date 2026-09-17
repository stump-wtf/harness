# Design: Operating Hours

## Context

Resident harnesses run 24/7 (ADR-0005, SPEC-0003), and a running agent can
spend tokens on anything that reaches it. ADR-0019 adds `operating_hours`: a
weekly time gate that holds a resident harness down outside its windows without
touching `enabled`. Governing spec: SPEC-0012. Related: SPEC-0003 (the
lifecycle it amends), SPEC-0008 and ADR-0013 (the tick, clock seam and zones it
reuses), SPEC-0002 (projection, `start` op and events), ADR-0007 (`state.json`),
ADR-0014 (reload intent for new harnesses).

## Goals / Non-Goals

### Goals

- An agent that works four hours a day costs nothing for the other twenty, with
  one config line.
- Two kinds of down, "the operator stopped it" and "it is off hours", are
  stored separately and rendered separately.
- Suspend, daemon outages, clock steps and DST are handled by the evaluation
  model itself, not by special cases.
- A late-night override is one command and ends by itself.
- By default a close lets the agent finish its turn, and it is always bounded.

### Non-Goals

- Seeing or metering tokens.
- Detecting turns without agent-trace (PTY heuristics).
- Queueing, holding or replaying work that arrives while a harness is held.
- Project-scoped hours, holidays, per-profile or daemon-wide defaults.

## Decisions

### A pure `internal/hours` package

The grammar and the membership test live in a new `internal/hours` package with
no clock and no I/O:

```go
type Expr struct { /* zone + windows, parsed */ }

func Parse(s string) (Expr, error)
// In reports whether t is inside a window, and the next instant that answer
// changes. ok is false when the expression covers the whole week.
func (e Expr) In(t time.Time) (in bool, next time.Time, ok bool)
```

`Parse` resolves zones through the same helper SPEC-0008 uses for `CRON_TZ=`, so
the embedded tzdata import in `cmd/harness/tzdata.go` covers both. `In` converts
`t` to the zone and checks today's windows plus yesterday's overnight windows.
It finds `next` by walking candidate boundaries (each window start and end over
the next eight local days) and returning the first where membership flips. The
walk is bounded because every valid expression repeats weekly.

Keeping it pure means every grammar case, DST day and overnight wrap is a table
test in microseconds, and the scheduler's source-scan test ("only `clock.go`
reads time") keeps holding.

### Evaluate on the existing scheduler tick

The scheduler already owns the one-second wall-clock ticker with the monotonic
component stripped. A second ticker would double the daemon's wakeups for no
gain. The scheduler gains a gate pass per tick:

```
for each gated supervisor:
    in, next, _ := expr.In(now)
    lease := manager.Lease(name)            // zero if none
    switch {
    case in && lease.Valid():               manager.EndLease(name)        // hours opened first
    case !in && lease.Valid(now):           // covered, nothing to do
    case !in && snap.Up():                  manager.Hold(name)
    case in && snap.Held:                   manager.Release(name)
    }
    if in != lastIn[name] { emit harness_hours_changed }
```

`lastIn` is in-memory only and exists to emit the event. No decision reads it,
so enforcement stays level-triggered and a daemon restart cannot lose a
transition.

### `Hold` and `Release` on the Manager

`Manager.Stop` clears intent and `Manager.StartTransient` exists for scheduled
firings (#159). Operating hours needs a third pair:

- **`Hold(name)`** sends a `hold` request to the supervisor's actor loop. The
  loop cancels a pending respawn, runs the graceful-stop sequence, marks the
  exit as a hold (so the exit handler skips the restart policy and resets flap
  bookkeeping), sets `Held = true`, and leaves `Enabled` alone.
- **`Release(name)`** asks the actor loop to clear `Held` and start the
  harness, again without writing `Enabled`.

`Hold` takes the close's mode and deadline. Under `graceful` the actor loop
marks the supervisor `Closing` with the deadline and returns. The scheduler
tick then calls `Manager.CloseStep(name, now)`, which asks the loop to stop the
harness once the turn state (below) says so, or the deadline passes. `Release`
and a lease clear `Closing`. `Manager.Stop` clears it too, and stops at once.

Both go through the actor loop because it is the one goroutine that sees spawn,
exit, stop and shutdown in order. Doing the check and the action there makes
"still up? then hold" atomic, the same reason ADR-0013 moved `on_overlap` onto
the loop.

`Held` is derived state and is not persisted. On boot `Autostart` computes it:
an enabled gated harness that is out of hours and has no valid lease starts
held.

### Turn state from a daemon-side watcher

Today only the TUI follows agent-trace live (`internal/tui/watcher.go`), and
`internal/runtrace` answers on demand for `harness logs`. Graceful shutdown adds
a daemon-owned watcher in `internal/runtrace`:

```go
type TurnState struct {
    LastEventAt time.Time
    TurnMarkers bool // the reader reports turn boundaries for this agent
    TurnEnded   bool // meaningful only when TurnMarkers
}

// Turn returns the live turn state of name's current run, or ok=false when no
// session is attributed to it.
func (w *Watcher) Turn(name string) (TurnState, bool)
```

The watcher follows only harnesses that are closing, or about to close within a
minute, so the daemon does no trace I/O the rest of the day. It applies the
same `Attribute` rule as `harness logs`, extended to resumed sessions
(SPEC-0006 REQ "Run Correlation"), so turn state can never come from a session
`logs` would refuse to show.

It needs three things from agent-trace
([github.com/stump-wtf/agent-trace](https://github.com/stump-wtf/agent-trace)):

- **claude-code:** turn-end events from `stop_reason`, which the transcripts
  already carry on every assistant record;
- **codex:** turn-end events from `task_complete`;
- **crush:** turn-end events from the finish part it writes at every turn end,
  not only failed ones.

Until a reader ships its markers, it reports `TurnMarkers = false` and closes
fall back to the quiet period.

### Leases in `state.json`

The persisted harness record gains `lease_until` (RFC 3339, omitted when
unset). The `start` control op handler, when the target is gated and out of
hours:

1. computes `until = now + for` (default `1h`);
2. writes `lease_until` synchronously;
3. calls `Manager.Start` (which persists `enabled = true`, as a manual start
   always has).

Writing before starting means a crash between the two leaves a lease with a
stopped harness, which boot resolves by starting it, never an unbounded
run. `stop` on any gated harness clears `lease_until` and calls `Manager.Stop`,
which clears `enabled` exactly as it does for an ungated harness.

### Presentation reuses the schedule machinery

`internal/schedfmt` already renders armed/next for scheduled harnesses, and
#331 moved the cadence into the SCHEDULE/NEXT columns. Operating hours adds:

- a state label `off-hours` for `Held`, styled like `armed`, and `closing`
  for a close in progress, in the transient-state styling;
- NEXT: `opens Mon 09:00` / `closes 13:00` / `lease until 21:00` /
  `stops by 13:15`;
- SCHEDULE: the expression, with the zone prefix trimmed when it equals the
  daemon's zone.

No new column (#343).

## Architecture

```mermaid
sequenceDiagram
    participant T as scheduler tick
    participant H as internal/hours
    participant M as Manager
    participant S as supervisor actor
    participant J as state.json

    T->>H: In(now)
    H-->>T: in=false, next=Mon 09:00
    T->>M: Lease(name)
    M-->>T: none
    T->>M: Hold(name)
    M->>S: hold
    S->>S: cancel respawn · SIGTERM → grace → SIGKILL
    S->>S: exit marked hold: no restart, flap reset, Held=true
    S-->>M: state stopped (enabled unchanged)
    M-->>T: done
    T-->>T: in changed → emit harness_hours_changed

    Note over T,J: operator: harness start NAME --for 2h (out of hours)
    M->>J: lease_until = now+2h (sync)
    M->>S: start
```

## Risks / Trade-offs

- **Graceful shutdown depends on unbuilt agent-trace markers.** Until they ship,
  every close is a quiet-period or immediate close. The decision table in
  SPEC-0012 REQ "Graceful Shutdown" makes that degradation explicit and logged,
  never silent.
- **The quiet period is a heuristic.** A long model call or tool run writes
  nothing until it finishes, so a quiet agent may be mid-turn. The cap bounds
  the cost of being wrong in either direction.
- **A close can overrun the window.** By up to `hours_shutdown_timeout` (15
  minutes by default), and a doorbell arriving during the close can use all of
  it. The sender-side fix is Switchboard endpoint presence.
- **The hold exit races a real crash.** A process that dies of its own accord
  in the instant the hold starts must not be counted as a crash and then
  respawned. Mitigated by deciding on the actor loop: once `hold` is accepted,
  every exit that follows is a hold exit.
- **Grammar surface.** A new mini-language is a new parser to fuzz. Mitigated
  by keeping it small (days, `HH:MM`, `;`) and adding a `FuzzParse` target
  that round-trips `Parse` → `String` → `Parse`.
- **Doorbells while held.** An agent that is held misses push notifications.
  Harness can't fix this and shouldn't try. An agent should reconcile its queue
  at startup, and holding doorbells for an off-shift agent is a sender feature.
- **More listing branches.** Held versus stopped is one more case in every
  renderer, on top of `Schedule != ""`. `schedfmt` keeps the wording in one
  place.

## Migration Plan

Additive. A config without `operating_hours` behaves exactly as before, and an
old `state.json` has no `lease_until`. A daemon that doesn't know the key
rejects it as unknown, so rolling back requires removing the key.

## Open Questions

- Should the default lease length be configurable per harness
  (`after_hours_lease = "2h"`), or is `--for` enough?
- Should the settle (10s) and quiet (2m) periods be configurable, or stay
  constants until a real agent proves them wrong?
- Should the watcher follow a closing harness's sessions only, or keep turn state
  warm for every adapter-backed harness so `harness describe` can show it?

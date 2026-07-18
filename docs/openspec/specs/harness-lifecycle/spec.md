---
id: SPEC-0003
title: Harness lifecycle state machine
status: draft
implements: [ADR-0005, ADR-0006, ADR-0007]
requires: []
---

# Spec — Harness lifecycle state machine

- **Status:** Draft
- **Relates to:** ADR-0005 (daemon supervises harnesses), ADR-0007 (persisted
  runtime state), spec-tui (glyphs/colors), spec-daemon-protocol (`state` field &
  events)

## States

| state | glyph | meaning |
|-------|-------|---------|
| `stopped` | `○` | Not running; intentionally down. Not autostarted. |
| `starting` | `◌` | Spawn in progress (PTY allocated, process starting). |
| `running` | `●` | Process alive under supervision. |
| `degraded` | `◐` | Alive but flapping recently, **or** in restart backoff (crash-loop detected). |
| `restarting` | `◌` | Exited while enabled; waiting `restart_delay` before respawn. |
| `stopping` | `◌` | Stop requested; sending SIGTERM→(grace)→SIGKILL. |
| `failed` | `✖` | Exited and **not** being restarted (disabled, or backoff gave up / manual stop after crash). |

`enabled` is an orthogonal boolean (does the daemon want this running?), persisted
in state.json (ADR-0007). A harness can be `enabled && stopped` briefly during a
restart cycle; `enabled` is intent, `state` is reality.

## Transitions

```
                 start / autostart(enabled|profile)
        stopped ─────────────────────────────▶ starting
           ▲                                      │ spawn ok
    stop   │                                      ▼
   (grace) │                                   running ───────────┐
        stopping ◀──── stop ──────────────────────┤               │ healthy exit(0)
           │                                       │ crash/exit≠0  │ while enabled?
           │ exited                                ▼               ▼
           └──────────────▶ stopped        restarting          (enabled? → restarting)
                                               │  wait restart_delay   (disabled? → stopped)
                                               ▼
                                            starting
                                               │
                              too-fast/too-often exits in window
                                               ▼
                                           degraded  ──(a clean run resets)──▶ running
                                               │  backoff exhausted / give-up
                                               ▼
                                            failed
```

### Rules

- **Autostart:** on daemon start, every `enabled` harness (directly, or via an
  `autostart` profile — ADR-0006) → `starting`. (ADR-0005)
- **Healthy exit (code 0):** if still `enabled`, treated like any exit → restart
  per policy (agents/watchers are meant to be long-lived; a clean exit still
  restarts unless the harness is disabled). If `!enabled`, → `stopped`.
- **Crash (code ≠ 0) while enabled:** → `restarting`, wait `restart_delay`, →
  `starting`. Restart count (`↻`) increments.
- **Crash-loop detection (ADR-0005):** N exits within window T → `degraded`;
  `restart_delay` escalates (capped exponential backoff). A single run that
  survives longer than T resets the counter and returns to `running`.
- **Backoff give-up:** after the cap / a max attempts threshold, → `failed`; the
  daemon stops auto-restarting and the TUI surfaces it loudly (needs a human).
  Manual `restart` clears it.
- **Stop:** `running|degraded|restarting → stopping`: SIGTERM, wait grace period,
  SIGKILL if needed, kill the PTY session → `stopped`, set `enabled=false`.
- **Config change to a running harness (ADR-0006):** applies on next (re)start;
  the TUI can flag "config changed — restart to apply."

## Events emitted (spec-daemon-protocol)

- `harness_state_changed { name, from, to }`
- `harness_exited { name, code }`
- `harness_flapping { name, restarts, next_retry_in }`

## Comparison to today

Today: two of these concepts existed implicitly — the systemd unit's active/failed
and the in-pane `while true; …; sleep` restart. There was **no** flapping
detection, no backoff, no distinct `degraded`/`failed`, and the `harnessd`
plugin's `_harnessd_state` collapsed everything to green/yellow/red by ANDing
"supervisor active" with "tmux session up." This spec makes the implicit machine
explicit, adds crash-loop safety, and gives the TUI real states to render.

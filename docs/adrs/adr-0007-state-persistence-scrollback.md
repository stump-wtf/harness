# ADR-0007 — State & scrollback ownership

- **Status:** Proposed
- **Date:** 2026-07-18

## Context and problem statement

With the daemon owning the terminal emulator (ADR-0003), it also owns two things
that used to be tmux's / the journal's job:

1. **Scrollback** — the recent output of each harness, so you can attach and see
   history, scroll up, and search. Today tmux holds the live pane and
   `journalctl`/the launchd log file hold the supervisor log.
2. **Runtime state** — which harnesses are enabled, which profile is active, last
   exit codes, restart counts — the things the daemon needs to restore itself
   after a restart (ADR-0005) and to render the dashboard.

We need to decide where each lives and how much survives a daemon restart.

## Decision drivers

- Attach must show recent history instantly, and survive *client* detach/reconnect
  with no gaps (the daemon keeps running).
- `harness logs <name>` should work without a live attach (like `harnessd log`
  today), including for a harness that has since crashed.
- A *daemon* restart shouldn't lose the world: enabled harnesses should come back,
  and ideally recent logs shouldn't vanish.
- Don't grow an unbounded memory footprint from long-running chatty agents.
- Secrets must never land in persisted state (ADR-0008).

## Considered options

- Scrollback: **(a) in-memory ring buffer only**, **(b) ring buffer + on-disk log
  file per harness**, **(c) full persistent event store**.
- Runtime state: **(x) in-memory only**, **(y) small JSON/SQLite state file the
  daemon writes**.

## Decision outcome

**Chosen: scrollback = ring buffer + on-disk log file (b); runtime state = small
persisted state file (y).**

### Scrollback (per harness)

- The `x/vt` emulator maintains the **live screen + a bounded scrollback ring**
  (default N lines, configurable, e.g. 10k). This backs attach + in-TUI scroll +
  search. It's the fast path and it's in memory.
- **In parallel**, the daemon tees raw PTY output to a **rotating log file** per
  harness under `$XDG_STATE_HOME/harnessd/logs/<name>.log` (size/age rotation).
  This backs `harness logs <name>` (including after a crash) and gives us a
  durable record independent of the ring. Analogous to today's launchd
  `StandardOutPath`, but uniform across platforms.
- On **attach**, a client gets: a screen snapshot (repaint) + a tail of scrollback,
  then the live stream. On **detach**, nothing is lost — the daemon kept reading
  the PTY the whole time.
- **Backpressure:** the PTY reader never blocks on a slow client. Output always
  goes to the ring + log file; per-client attach streams get a bounded queue and,
  if a client can't keep up, frames are **coalesced/dropped for that client only**
  (it repaints from the current screen) — one slow SSH client can never stall the
  harness. (Detail in spec-daemon-protocol.md.)

### Runtime state (daemon-global)

A small state file (`$XDG_STATE_HOME/harnessd/state.json`, or SQLite if it grows):

- enabled/disabled per harness, active/last profile, last exit code + restart
  count + flapping status, `created`/`last-started` timestamps.
- Written on transitions (debounced), read on daemon start to **restore the
  world**: re-open log files, restart the harnesses that should be running
  (ADR-0005), reselect the active profile.
- **Config is *not* duplicated here** — config's source of truth is the TOML
  (ADR-0006). This file is *runtime* state only. Clean split: TOML = intent,
  state.json = what actually happened.

### What does *not* persist

- Live PTY/emulator screen across a **daemon** restart. If the daemon restarts,
  running processes are (by default) restarted fresh (ADR-0005); their prior live
  screen is gone, but the **log file** retains history. (The advanced "re-adopt
  orphaned harness processes" idea in ADR-0005 could later preserve live state too;
  not committed.)
- Secrets. `env_file` contents are loaded into the child's environment at spawn and
  never copied into scrollback exports, state.json, or logs we control (we can't
  stop a program from printing its own secrets — that's on the harnessed command).

## Consequences

**Positive**

- Attach/detach/reattach is lossless while the daemon lives; history is instant.
- `harness logs` works for live *and* dead harnesses, cross-platform, uniformly.
- Daemon restart restores the intended running set and keeps log history.
- Bounded memory (ring) + bounded disk (rotation); no runaway growth from chatty
  agents.
- Clean intent/state separation keeps config git-friendly (ADR-0006).

**Negative / costs**

- Disk usage for logs (mitigated by rotation; configurable/disable-able per
  harness for the privacy-conscious).
- A second persistence concern (state.json) with its own atomic-write + schema-
  version discipline.
- We must be disciplined that **no secret** reaches logs/state that *we* write
  (we can't police what the child prints).

**Rejected options**

- **Ring-only, no log file (a)** — loses `harness logs` after a crash and any
  durable record; a regression from today's log files/journal.
- **Full event store (c)** — over-engineered for v1; revisit only if we want
  session replay/recording as a feature.
- **In-memory-only runtime state (x)** — daemon restart would forget what should be
  running; defeats ADR-0005's restore.

## Related

ADR-0002 (daemon owns state), ADR-0003 (emulator + ring), ADR-0005 (restore on
restart), ADR-0006 (config stays in TOML), ADR-0008 (secrets), spec-daemon-protocol.md.

# ADR-0005 — Supervision: daemon self-supervises harnesses; init only supervises the daemon

- **Status:** Proposed
- **Date:** 2026-07-18

## Context and problem statement

Today supervision is *per harness*: each is a systemd `--user` unit (or launchd
plist) whose `ExecStart` is `harness-run <name>`, and a `while true; …; sleep`
loop inside the tmux pane restarts the command on crash. Two layers (init + pane
loop) keep one harness alive.

With a resident daemon (ADR-0002) that owns every PTY, this arrangement no longer
fits: the daemon, not systemd, is now the thing spawning and watching harness
processes. So: **who restarts a crashed harness, and who restarts the daemon?**

## Decision drivers

- A crashed harness must come back automatically, honoring `restart_delay` and not
  hot-looping on a command that instantly fails.
- The daemon itself must survive logout and come back on boot/crash.
- Keep the boot/login integration familiar (systemd `--user` on Linux, launchd on
  macOS) — the current design already does this well and Joe's infra expects it.
- Don't create a supervision hall-of-mirrors (init watching a loop watching a
  loop).

## Considered options

1. **Daemon supervises harnesses; init supervises only the daemon.** ✅
2. **Keep per-harness init units** (each harness still a systemd/launchd unit that
   somehow registers with the daemon).
3. **Daemon supervises everything including itself** (double-fork, PID file, its
   own boot hook), no systemd/launchd.

## Decision outcome

**Chosen: Option 1 — two clean layers.**

### Layer 1 — the daemon supervises harnesses (in-process)

Each running harness has a supervisor goroutine:

- Spawn `cmd args` under a PTY in `workdir`, with `env_file` loaded (ADR-0008).
- On exit, record exit code + timestamp, transition the state machine
  (spec-harness-lifecycle.md), and if the harness is enabled, restart after
  `restart_delay`.
- **Crash-loop backoff:** if a harness exits "too fast, too often" (e.g. N exits
  within a window), escalate `restart_delay` (capped exponential) and mark the
  harness **degraded/flapping** in the UI rather than silently thrashing. This is
  a strict improvement over today's fixed `sleep $HR_DELAY` loop.
- `restart_delay` semantics from the current TOML are preserved.

This replaces *both* today's per-harness init unit **and** the in-pane
`while true` loop with one well-instrumented supervisor the UI can actually see
into (last exit code, restart count, flapping state).

### Layer 2 — init supervises the daemon (one unit)

A **single** long-lived service:

- **Linux:** a systemd `--user` unit `harnessd.service` — `ExecStart=harness daemon`,
  `Restart=on-failure`, `RestartSec=…`, the same `PATH=%h/.local/bin:…` fix the
  current template carries (so agent CLIs find `uv`/`npx`/`go`). `WantedBy=default.target`.
  `loginctl enable-linger $USER` to keep it running while logged out — unchanged
  guidance from today.
- **macOS:** a single launchd LaunchAgent `rocks.stump.harnessd` with
  `RunAtLoad`/`KeepAlive` — replacing the per-harness plists the plugin generates
  today.

The template drops from *N per-harness units* to **one**. `harness` (the client)
never needs systemd/launchd — it just connects to the socket.

### Harness enable/disable ≠ init

"Enabled on boot" moves from *systemd enablement* to **daemon state**: a harness
marked `enabled` (or belonging to an autostart profile — ADR-0006) is started by
the daemon when the daemon starts. `harness start/stop` talk to the daemon, not to
systemctl. This is simpler and uniform across Linux/macOS (today the code branches
`systemctl` vs `launchctl` everywhere).

## Consequences

**Positive**

- One init unit total; the daemon owns harness lifecycle with visibility
  (flapping detection, restart counts, last exit) the shell loop never had.
- Uniform cross-platform behavior — the OS-specific branching collapses to "how do
  we keep *one* daemon alive," which systemd/launchd each do well.
- Boot/login autostart stays in the familiar systemd/launchd idiom Joe's infra
  already uses.

**Negative / costs**

- **Blast radius:** if `harnessd` crashes, every harness dies with it (ADR-0002
  raised this). Mitigations: (a) keep the daemon small and heavily tested; (b)
  `Restart=on-failure` brings it right back and it restarts enabled harnesses; (c)
  **optional hardening** — spawn harnesses in their own process group / session so
  a daemon crash can leave them orphaned-but-alive and let the restarted daemon
  *re-adopt* them (advanced; a v2 consideration, noted not committed).
- Losing per-harness systemd units means losing `journalctl --user -u harness@foo`
  muscle memory; we replace it with `harness logs foo` (daemon-served, ADR-0007)
  and can *also* emit to the journal/syslog if we want that back.
- The daemon must persist "which harnesses were enabled" so a restart restores the
  right set (ADR-0007).

**Rejected options**

- **Per-harness init units (Opt 2)** — recreates today's N-units sprawl and splits
  authority: init thinks it owns the process, but the daemon owns the PTY. Two
  bosses. Rejected.
- **Daemon supervises itself, no systemd/launchd (Opt 3)** — we'd hand-roll
  daemonization, PID files, and boot hooks that systemd/launchd already do better
  and that Joe's environment already standardizes on. Rejected.

## Related

ADR-0002 (daemon owns processes), ADR-0006 (autostart profiles),
ADR-0007 (persisting enabled-state + logs), spec-harness-lifecycle.md.

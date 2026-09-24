---
status: accepted
date: 2026-07-18
decision-makers: [joestump]
extends: [ADR-0002]
governs: [SPEC-0003]
related: [ADR-0006, ADR-0007]
---

# ADR-0005: Supervision — the daemon supervises harnesses; init only supervises the daemon

## Context and Problem Statement

In the predecessor, supervision is *per harness*: each is a systemd `--user` unit
(or launchd plist) whose `ExecStart` is `harness-run <name>`, and a
`while true; …; sleep` loop inside the tmux pane restarts the command on crash.
Two layers (init + pane loop) keep one harness alive.

With a resident daemon (ADR-0002) that owns every PTY, this arrangement no longer
fits: the daemon, not systemd, is now the thing spawning and watching harness
processes. So: **who restarts a crashed harness, and who restarts the daemon?**

## Decision Drivers

* A crashed harness must come back automatically, honoring `restart_delay` and not
  hot-looping on a command that instantly fails.
* The daemon itself must survive logout and come back on boot/crash.
* Keep the boot/login integration familiar (systemd `--user` on Linux, launchd on
  macOS) — the predecessor already does this well and the hosts it runs on expect
  it.
* Don't create a supervision hall-of-mirrors (init watching a loop watching a
  loop).

## Considered Options

* Option 1 — Daemon supervises harnesses; init supervises only the daemon
* Option 2 — Keep per-harness init units (each harness still a systemd/launchd
  unit that somehow registers with the daemon)
* Option 3 — Daemon supervises everything including itself (double-fork, PID file,
  its own boot hook), no systemd/launchd

## Decision Outcome

Chosen option: **Option 1 — Daemon supervises harnesses; init supervises only
the daemon**, because it gives two clean layers: one process with full
visibility into harness lifecycle, and one init unit that keeps that process
alive.

### Layer 1 — the daemon supervises harnesses (in-process)

Each running harness has a supervisor goroutine:

* Spawn `cmd args` under a PTY in `workdir`, with `env_file` loaded (ADR-0008).
* On exit, record exit code + timestamp, transition the state machine
  (SPEC-0003), and if the harness is enabled, restart after `restart_delay`.
* **Crash-loop backoff:** if a harness exits "too fast, too often" (e.g. N exits
  within a window), escalate `restart_delay` (capped exponential) and mark the
  harness **degraded/flapping** in the UI rather than silently thrashing. This is
  a strict improvement over the predecessor's fixed `sleep $HR_DELAY` loop.
* `restart_delay` semantics from the predecessor's TOML are preserved.

This replaces *both* the per-harness init unit **and** the in-pane `while true`
loop with one well-instrumented supervisor the UI can actually see into (last
exit code, restart count, flapping state).

### Layer 2 — init supervises the daemon (one unit)

A **single** long-lived service:

* **Linux:** a systemd `--user` unit `harness.service` —
  `ExecStart=harness daemon`, `Restart=on-failure`, `RestartSec=…`, and a
  `PATH=%h/.local/bin:…` line so agent CLIs find `uv`/`npx`/`go`.
  `WantedBy=default.target`. `loginctl enable-linger $USER` keeps it running
  while logged out.
* **macOS:** a single launchd LaunchAgent with `RunAtLoad`/`KeepAlive` — replacing
  the per-harness plists the predecessor generates.

The template drops from *N per-harness units* to **one**. `harness` (the client)
never needs systemd/launchd — it just connects to the socket. The
[run-as-a-service guide](../guides/run-as-a-service.md) carries both unit shapes.

### Harness enable/disable ≠ init

"Enabled on boot" moves from *systemd enablement* to **daemon state**: a harness
marked `enabled` (or belonging to an autostart profile — ADR-0006) is started by
the daemon when the daemon starts. `harness start/stop` talk to the daemon, not to
systemctl. This is simpler and uniform across Linux/macOS (the predecessor
branches `systemctl` vs `launchctl` everywhere).

### Consequences

* Good, because there is one init unit total; the daemon owns harness lifecycle
  with visibility (flapping detection, restart counts, last exit) the shell loop
  never had.
* Good, because behavior is uniform cross-platform — the OS-specific branching
  collapses to "how do we keep *one* daemon alive," which systemd/launchd each do
  well.
* Good, because boot/login autostart stays in the familiar systemd/launchd idiom.
* Bad, because of **blast radius:** if the daemon crashes, every harness dies with
  it (ADR-0002 raised this). Mitigations: (a) keep the daemon small and heavily
  tested; (b) `Restart=on-failure` brings it right back and it restarts enabled
  harnesses; (c) **optional hardening** — spawn harnesses in their own process
  group / session so a daemon crash can leave them orphaned-but-alive and let the
  restarted daemon *re-adopt* them (advanced; a v2 consideration, noted not
  committed).
* Bad, because losing per-harness systemd units means losing
  `journalctl --user -u harness@foo` muscle memory; we replace it with
  `harness logs foo` (daemon-served, ADR-0007) and can *also* emit to the
  journal/syslog if we want that back.
* Neutral, because the daemon must persist "which harnesses were enabled" so a
  restart restores the right set (ADR-0007).

### Confirmation

* A harness that exits is restarted by the daemon after `restart_delay`, and a
  crash loop emits `harness_flapping` with an escalated retry delay.
* The service guide installs exactly one unit (`harness.service` on Linux, one
  LaunchAgent on macOS) whose command is `harness daemon`; `harness start` and
  `harness stop` never call `systemctl` or `launchctl`.

## Pros and Cons of the Options

### Option 1 — Daemon supervises harnesses; init supervises the daemon

* Good, because each layer has one boss: init owns the daemon, the daemon owns
  every harness process and its PTY.
* Good, because the supervisor exposes restart counts and flapping state to the UI.
* Bad, because a daemon crash takes every harness with it until init restarts it.

### Option 2 — Per-harness init units

* Good, because each harness is isolated from a daemon crash.
* Bad, because it recreates the predecessor's N-units sprawl and splits authority:
  init thinks it owns the process, but the daemon owns the PTY. Two bosses.
  Rejected.

### Option 3 — Daemon supervises itself, no systemd/launchd

* Good, because it has no dependency on the host's init system.
* Bad, because we'd hand-roll daemonization, PID files, and boot hooks that
  systemd/launchd already do better and that the target hosts already
  standardize on. Rejected.

## More Information

* **Extends ADR-0002** — the daemon owns processes.
* **Related ADR-0006** — autostart profiles.
* **Related ADR-0007** — persisting enabled-state and logs.
* **Governs SPEC-0003** — the harness lifecycle state machine.

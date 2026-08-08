---
title: "Supervision"
sidebar_position: 4
---

# Supervision

Harness's supervision model is deliberately two-tier (ADR-0005):

```
init (systemd/launchd)  ──supervises──▶  harness daemon  ──supervises──▶  each harness
```

The daemon supervises harnesses; your init system supervises the daemon. This
keeps the tier that owns sensitive state (PTYs, scrollback, restart policy)
small and well-tested, and lets `systemd`/`launchd` be the crash recovery for
the daemon itself.

## Running the daemon as a service

As a systemd `--user` unit:

```ini
# ~/.config/systemd/user/harness.service
[Unit]
Description=Harness agent supervisor

[Service]
ExecStart=%h/go/bin/harness daemon
Restart=on-failure

[Install]
WantedBy=default.target
```

```sh
systemctl --user daemon-reload
systemctl --user enable --now harness.service
```

On macOS, use an equivalent launchd LaunchAgent
(`dev.harness.daemon.plist`) with `ProgramArguments` set to
`<path>/harness daemon`.

## What the daemon guarantees

- **Kept alive**: a harness configured `enabled = true` (or part of an autostart
  profile) is brought up at daemon start and re-attachable thereafter.
- **Restart on exit**: the `restart` policy and `restart_delay` in
  [Configuration](./configuration#restart-policy) control whether and how fast a
  harness is brought back after it exits.
- **State persistence (ADR-0007)**: the daemon persists intent to `state.json`,
  restores it on boot, and re-attaches to intended running set regardless of how
  it restarted.
- **Daemon-owned scrollback (ADR-0007)**: each harness's PTY output is tee'd
  into an in-memory ring (depth configurable with `harness daemon --scrollback N`)
  plus a durable log, so you can read back output even while detached.

## Flapping a.k.a. crash-loop protection

Under repeated quick restarts, the daemon escalates backoff so a crash-looping
harness can't burn the CPU. `harness describe <name>` surfaces this with a
`flapping` field and, when applicable, a `restart to apply` config prompt.

## One-shot vs. supervised

For a quick session, run `harness daemon` in a terminal or `--detach` into the
background. That's fine for development. For anything you want to survive logout
or reboot, wire the unit above — a shell that closes is a supervision gap the
init service closes.

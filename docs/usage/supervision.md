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

## Running the daemon as a service on macOS

On macOS the init tier is **launchd**, and the daemon runs as a LaunchAgent
(owned by your user, started at login). The full equivalent of the systemd
unit above:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!-- ~/Library/LaunchAgents/dev.harness.daemon.plist -->
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>dev.harness.daemon</string>

    <key>ProgramArguments</key>
    <array>
        <string>/Users/you/go/bin/harness</string>
        <string>daemon</string>
    </array>

    <!-- systemd's Restart=on-failure analogue: relaunch on a non-zero exit.
         launchd throttles restarts to at most once per 10s by default. -->
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>

    <key>RunAtLoad</key>
    <true/>

    <!-- stdout/stderr of the daemon itself (not of harnesses — those are
         owned by the daemon's own PTY + scrollback ring). -->
    <key>StandardOutPath</key>
    <string>/tmp/harness-daemon.log</string>
    <key>StandardErrorPath</key>
    <string>/tmp/harness-daemon.log</string>
</dict>
</plist>
```

```sh
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/dev.harness.daemon.plist
launchctl enable gui/$(id -u)/dev.harness.daemon
```

Day to day:

```sh
launchctl print gui/$(id -u)/dev.harness.daemon   # status (the launchd 'journalctl')
tail -f /tmp/harness-daemon.log                   # daemon log
launchctl bootout gui/$(id -u)/dev.harness.daemon # stop (survives reboot of the plist)
```

Two launchd specifics worth knowing:

- **No login shell, no environment.** A LaunchAgent does not source your
  `~/.zshrc`; if your harnesses need env vars (API tokens, `PATH` additions),
  set them with an `EnvironmentVariables` dict in the plist or load them from
  an env file the daemon layers per-harness (see the harness `env_file` field
  in [Configuration](./configuration)).
- **GUI session, not SSH sessions.** LaunchAgents run inside your GUI login
  session. A daemon started this way is up while you are logged in at the
  console — for an always-on box that auto-logins, that is the same as
  always-on; for a headless Mac you want that auto-login or an SSH-started
  daemon instead.

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

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
- **Provider failover is not supervision**: the restart budget only helps when
  the process crashes. A quota wall or provider outage leaves the harness
  `running` — dead on the inside. See
  [Configuration → Model routing](./configuration#model-routing-and-provider-failover)
  for the mitigation.
- **Agent events are observed, not just processes**: every 5 seconds the daemon
  reads the session transcripts its `crush`, `claude-code` and `codex`
  harnesses write and picks up new tool calls and marks — for `crush` and
  `claude-code`, that includes the provider errors a quota wall leaves behind
  while the process stays `running` (`codex` transcripts do not record those
  as errors yet). Only activity after the daemon started is reported, a
  session two harnesses could have written is attributed to neither, and
  credentials in the text are masked before anything else in the daemon sees
  it. Nothing leaves the host yet: this is the feed the upcoming metrics and
  telemetry exporters consume.
- **Runaway tool loops are stopped**: an agent that calls the same tool with
  the same arguments 8 times in a row — no other tool call and no new prompt
  in between — is stuck, not working, however healthy its process looks. The
  daemon stops that harness (a stop, like `harness stop`: its enabled intent
  is cleared and its `restart` policy does not bring it back, because a
  restarted `crush` resumes the looping session) and logs an `ERROR` naming
  the harness, the tool and the count to both the daemon log and the
  harness's own log. Start it again once you have looked. Repeating a call
  with other work between — `make test` after each edit, one queue poll per
  prompt — never counts; nor does calling the same tool with different
  arguments. A loop that alternates between two calls is not caught.
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

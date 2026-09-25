---
title: "Run the daemon as a service"
sidebar_position: 2
---

# Run the daemon as a service

Supervision in Harness has two tiers. Your init system keeps the daemon alive,
and the daemon keeps every agent alive:

```
systemd / launchd  ──supervises──▶  harness daemon  ──supervises──▶  your agents
```

A daemon started in a terminal dies when that terminal closes. This guide wires
it into `systemd --user` on Linux or a LaunchAgent on macOS, so it survives
logout and reboot.

Run the daemon in the **foreground** under either init system: `harness daemon
start`, with no `--detach`. The init system does the backgrounding and the
restarts.

## Linux: a systemd user unit

Create `~/.config/systemd/user/harness.service`:

```ini
[Unit]
Description=Harness agent supervisor
Documentation=https://stump-wtf.github.io/harness/

[Service]
Type=simple
ExecStart=%h/.local/bin/harness daemon start

# Agent CLIs are looked up on the DAEMON's PATH when a harness spawns.
# A user unit gets a minimal PATH, so list every directory your agent
# CLIs (claude, crush, codex) and their runtimes (node, bun) live in.
Environment=PATH=%h/.local/bin:%h/go/bin:%h/.bun/bin:/usr/local/bin:/usr/bin:/bin

# Optional KEY=VALUE file for the daemon itself. The leading "-" makes a
# missing file a no-op instead of a failed start.
EnvironmentFile=-%h/.config/harness/daemon.env

Restart=on-failure
RestartSec=5

[Install]
WantedBy=default.target
```

Point `ExecStart` at wherever `harness` actually lives: `command -v harness`
tells you. Then:

```sh
systemctl --user daemon-reload
systemctl --user enable --now harness.service

# Keep user services running when you are not logged in (servers, headless boxes)
sudo loginctl enable-linger "$USER"
```

Day to day:

```sh
systemctl --user status harness     # is the daemon up
journalctl --user -u harness -f     # the daemon's own log
harness daemon status               # version, pid, uptime, socket, harness count
harness doctor                      # the full health report
```

### Don't hard-depend on things that restart

It is tempting to write `Requires=`, `BindsTo=` or `PartOf=` against a
secrets agent, a VPN, Docker, or anything else your agents need.

Don't. systemd propagates a stop along those edges. When the dependency
restarts, even for a routine upgrade or a credential refresh, systemd stops
Harness too. That takes every agent it supervises down with it, and each one
pays its startup cost again on the way back.

- If ordering matters at boot, use the soft pair `Wants=` + `After=`.
- Make the agents tolerate a dependency that blips. A harness with `restart =
  "on-failure"` already comes back on its own.

The same reasoning applies to your own habits:

- **Use `harness reload` for config changes.** It applies them without touching
  running agents. The daemon also watches `harness.toml` and reloads on save.
- **Keep `systemctl --user restart harness` for upgrading the binary.**
  Restarting the unit stops every agent in it.

## macOS: a LaunchAgent

If you installed Harness with Homebrew, you do not need to write a plist at all.
The formula ships a service definition, and `brew services start harness` is the
supported way to run the daemon:

```sh
brew services start harness     # start, and start again at login
brew services stop harness      # stop
brew services info harness      # status
```

It runs the same command as the plist below (`harness daemon start`, in the
foreground, relaunching on a crash but not after a clean stop) and gives the
service a minimal PATH. Because `brew services` runs inside your GUI login
session, agents it launches inherit your login Keychain, so a `claude-code`
harness needs no `env_file`.

One thing to know before you reach for it: an agent CLI installed outside
Homebrew is **not** on that PATH, and the daemon looks the executable up on its
own PATH when a harness spawns. See
[agent CLIs resolve on the daemon's PATH](#agent-clis-resolve-on-the-daemons-path).

The rest of this section is the hand-written plist, for a Linux-style manual
install, a pinned binary outside Homebrew, or when you want to control the
plist yourself.

Create `~/Library/LaunchAgents/dev.harness.daemon.plist`. launchd does not
expand `~` or `$HOME`, so replace `/Users/you` with your real home directory
throughout:

```xml
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN"
  "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>dev.harness.daemon</string>

    <key>ProgramArguments</key>
    <array>
        <string>/opt/homebrew/bin/harness</string>
        <string>daemon</string>
        <string>start</string>
    </array>

    <!-- A LaunchAgent does not read your shell profile. Agent CLIs are looked
         up on this PATH when a harness spawns, so include Homebrew, ~/.local/bin,
         and wherever node or bun live if your agent needs them. -->
    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>/Users/you/.local/bin:/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin</string>
    </dict>

    <!-- Relaunch on a crash, not after a clean `harness daemon stop`. -->
    <key>KeepAlive</key>
    <dict>
        <key>SuccessfulExit</key>
        <false/>
    </dict>
    <key>RunAtLoad</key>
    <true/>

    <!-- The daemon's own log. Harness output lives in Harness's logs. -->
    <key>StandardOutPath</key>
    <string>/Users/you/Library/Logs/harness-daemon.log</string>
    <key>StandardErrorPath</key>
    <string>/Users/you/Library/Logs/harness-daemon.log</string>
</dict>
</plist>
```

Load it:

```sh
launchctl bootstrap gui/$(id -u) ~/Library/LaunchAgents/dev.harness.daemon.plist
launchctl enable gui/$(id -u)/dev.harness.daemon
```

Day to day:

```sh
launchctl print gui/$(id -u)/dev.harness.daemon     # status
tail -f ~/Library/Logs/harness-daemon.log           # the daemon's own log
launchctl kickstart -k gui/$(id -u)/dev.harness.daemon   # restart after an upgrade
launchctl bootout gui/$(id -u)/dev.harness.daemon   # stop and unload
```

A LaunchAgent runs inside your GUI login session. On a Mac that must stay up
headless, turn on automatic login so the session exists after a reboot.

That session is also what gives agents their Keychain-backed credentials. A
LaunchAgent bootstrapped into `gui/$(id -u)`, as above, can read your login
Keychain, so a `claude-code` harness uses the session `claude` already stored
there and needs no `env_file`. A *system* LaunchDaemon cannot — it runs
outside your login session, and agents there fall back to an `env_file`. Use
a LaunchAgent unless you have a specific reason not to.

## Environment and secrets

There are two places to put environment, and they have different reach:

| Where | Seen by | Use it for |
|-------|---------|------------|
| The unit's `Environment=` / `EnvironmentFile=`, or the plist's `EnvironmentVariables` | the daemon **and every harness** it starts | `PATH`, locale, anything genuinely shared |
| A harness's `env_file` in `harness.toml` | **that harness only** | API keys, tokens, per-agent settings |

### Agent CLIs resolve on the daemon's PATH

The executable a harness runs is looked up **once, on the daemon's `PATH`**, and
a harness's `env_file` cannot change that: it is applied after the lookup, so a
`PATH` line in it is ignored for this purpose.

That matters for every managed service, because a service does not read your
shell profile:

- **Linux (`systemd --user`):** put `Environment=PATH=…` in the unit, listing
  every directory your agent CLIs and their runtimes live in. The
  [unit above](#linux-a-systemd-user-unit) does this.
- **macOS (LaunchAgent):** put `PATH` in the plist's `EnvironmentVariables`.
  The [plist above](#macos-a-launchagent) does this.
- **macOS (`brew services`):** the formula's `PATH` is Homebrew plus the system
  directories. An agent installed under `~/.local/bin`, an npm or bun global
  prefix, or a mise/asdf shim directory will not be found. Install the agent
  through Homebrew, or use a LaunchAgent instead.

The symptom is a harness that fails **at spawn**, not at `brew services start`
or `systemctl --user start` — which is why this is worth reading before the
first scheduled run rather than after it.

Prefer the per-harness `env_file` for secrets. It keeps a token scoped to the one
agent that needs it, and out of `harness.toml`, which you may want to keep in
version control:

```toml
[harness.reviewer]
harness = "crush"
workdir = "~/agents/reviewer"
env_file = "~/.config/harness/env/reviewer.env"
enabled = true
```

```sh
# ~/.config/harness/env/reviewer.env   (chmod 600)
CLAUDE_CODE_OAUTH_TOKEN=...   # from `claude setup-token`; bills your subscription
GITHUB_TOKEN=ghp_...
```

For Claude Code, `CLAUDE_CODE_OAUTH_TOKEN` (from `claude setup-token`) bills
your Claude subscription; `ANTHROPIC_API_KEY` bills the API instead. See
[the first agent guide](./first-agent#before-the-first-start) for all three
auth modes.

Things worth knowing about `env_file`:

- It is plain `KEY=VALUE`. Blank lines, `#` comments, a leading `export`, and
  one pair of surrounding quotes are handled; there is no `$VAR` expansion.
- Its values are layered over the daemon's environment, and the file wins on
  collisions.
- It is read **when the harness starts**. After editing it, `harness restart
  NAME`. The config watcher does not watch env files.
- **A missing `env_file` is not an error.** The harness starts without those
  variables. A typo in the path therefore shows up as an agent that is running
  but cannot authenticate, which is the first thing to check in
  [Troubleshooting](./troubleshooting#the-agent-is-running-but-does-nothing).
- **It cannot fix `PATH` for finding the agent binary.** The executable is looked
  up on the daemon's `PATH` before the harness's environment applies. Put `PATH`
  in the unit or plist.

macOS has no `EnvironmentFile`. Use per-harness `env_file`s, which work
identically on both platforms.

## The client has to find the socket

`harness` talks to the daemon over a Unix socket whose default location comes
from the environment:

- **Linux:** `$XDG_RUNTIME_DIR/harness.sock`. systemd sets `XDG_RUNTIME_DIR`
  for both your login shell and your user units, so this just works.
- **macOS:** there is no `XDG_RUNTIME_DIR`, so the socket falls back to
  `$XDG_STATE_HOME/harness/harness.sock` (`~/.local/state/harness/harness.sock`).

The two sides only disagree if your shell and the service see different `XDG_*`
variables. For example, you export `XDG_STATE_HOME` in `~/.zshrc`, but the
LaunchAgent doesn't. The symptom is `harness list` saying the daemon is not
running while `launchctl print` says it is. Fix it by pinning the socket
explicitly on both sides, with the same value:

```sh
# in your shell profile
export HARNESS_SOCKET="$HOME/.local/state/harness/harness.sock"
```

Then add the same `HARNESS_SOCKET` to the unit (`Environment=`) or the plist
(`EnvironmentVariables`).

Keep the path short. A Unix socket path is limited to about 104 bytes on
macOS and 108 on Linux, and a longer one makes the daemon exit at startup with
`bind: invalid argument`.

## Check it

After a reboot, or after a logout and login, you should see:

```sh
$ harness daemon status
FIELD         VALUE
version       …
pid           …
uptime        …
socket        /run/user/1000/harness.sock
harnesses     0
```

Next: [your first supervised agent](./first-agent).

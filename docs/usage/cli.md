---
title: "CLI reference"
sidebar_position: 2
---

# CLI reference

The client is a set of **one-shot verbs**: dial the daemon, perform one
request, print (human or `--json`), and exit. Every verb also supports a global
`--json` flag for machine-readable output that mirrors the daemon RPC contract.

Common flags on every call:

- `--socket PATH` — daemon socket (defaults to the daemon's default path).
- `--config PATH` — `harness.toml` path (defaults to `~/.config/harness/harness.toml`).
- `--json` — machine-readable output.

## Lifecycle verbs

```sh
harness start <name>          # start a harness (or a project harness)
harness stop <name>           # stop a harness
harness restart <name>        # restart a harness
harness start --all           # start/stop/restart every harness at once
```

`<name>` resolves to `<project>/<name>` inside a project (see
[Projects](./projects)); `--all` keeps its daemon-wide meaning. `--all` runs
render live per-harness progress with a Bubble Tea animation so a large fleet
start/stop is visible as it converges.

The client warns when its own build is older or newer than the daemon's
(client/daemon skew) — after upgrading, restart the daemon so both sides speak
the same protocol version.

## Listing & inspection

```sh
harness list                  # table of every harness: name, state, enabled, restarts, PID
harness ps                    # inside a project: only that project's harnesses
harness describe <name>       # one harness in detail (state, cmd, backend, flapping, ...)
harness daemon-info           # daemon version, proto, PID, uptime, socket, active profile
```

`list` and `describe` also surface **schedule metadata** for scheduled
one-shots (see [Scheduled jobs](#scheduled-jobs) under Configuration). In the
listing, a scheduled harness is marked inline rather than by extra columns: its
state glyph becomes a clock (⏱, in the same colour, so the state still reads at
a glance) and its next firing is appended to the description as a relative time
— `sweeps the fleet · in 4h3m`. `describe` shows the full picture: the cron
spec verbatim plus the absolute time of the next firing.

The cron spec itself is config, not status, so it stays off the listing
surface; reach for `describe` or `--json` when you need it.

`describe` additionally lists the harness's **live attach sessions** — who is
attached right now, and whether each session is read-only.

`--json` on any of these emits the same data as structured JSON.

## Scheduled jobs

Scheduled one-shots fire daemon-side on a cron schedule — no verb to remember,
just configure `schedule` on a `prompt` harness (see
[Configuration → Scheduled one-shots](./configuration#scheduled-one-shots)).
`harness list` flags each job with a clock glyph and its next firing time;
`harness describe` adds the cron spec.

## Logs

```sh
harness logs <name>           # tail (default 200 lines)
harness logs <name> --lines 50    # a specific number of trailing lines
harness logs <name> --follow      # stream new output as it arrives
```

When a log rotates or truncates, `--follow` reprints the current tail so you
never silently lose context.

## Profiles

```sh
harness profiles              # list profiles and which is active (*)
harness use-profile <name>    # switch the active profile
```

Profiles (a "configuration of harnesses") switch whole sets at once. Only one
is active at a time; `harness list` flags it with `*`. See
[Configuration](./configuration#profiles).

## Reload & diagnostics

```sh
harness reload                # re-read config, reconcile running harnesses
harness doctor                # health check battery (config, daemon, versions, remote SSH, harnesses)
```

`reload` picks up config changes without restarting the daemon. `describe`
shows a `config — changed — restart to apply` row when the running harness is
out of date with the config file.

## Attach

```sh
harness attach <name>         # attach to a harness as a live terminal
harness attach <name> --ro    # read-only: attach but ignore keystrokes
```

`attach` reuses the same full-window terminal the dashboard uses, with the
1-line status bar and tmux-style detach chords. See [Cockpit TUI](./tui).

## Project verbs

```sh
harness up                    # bring the enclosing project up (detached)
harness down [PROJECT]        # stop & deregister a project's harnesses
harness ps                    # project-scoped listing
```

See [Projects](./projects) for the full story, discovery rules, and the project
file schema.

## Daemon subcommands

`harness daemon` is its own subcommand group:

```sh
harness daemon                # run the supervisor in the foreground (== daemon run)
harness daemon stop           # ask the running daemon to shut down (SIGTERM)
harness daemon status         # one-shot: daemon info
harness daemon --detach       # fork into the background (dev convenience)
```

Daemon flags: `--config`, `--socket`, `--scrollback N` (per-harness ring depth),
`--ssh`, `--ssh-listen`, `--log-level`, `--log-file`, `--detach`.

## Exit codes & error handling

Every error is classified and rendered as a styled error box with an actionable
hint. A `--json` error still prints a structured object. Exit code is 0 on
success, non-zero on failure (`doctor` returns non-zero when any check fails).

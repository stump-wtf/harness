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
harness describe <name>       # one harness in detail (state, harness kind, backend, flapping, ...)
harness daemon status         # daemon version, proto, PID, uptime, socket, active profile
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
`harness list` gives every harness a **SCHEDULE** column (the cadence, e.g.
`daily 10:00 UTC`, or the raw expression when it cannot be paraphrased) and a
**NEXT** column (the countdown, e.g. `in 2h`), both derived from the config and
the running scheduler — never from the description text. A job waiting for its
next firing reads `⏱ armed` rather than `stopped`, because it is loaded and
will fire on its own; `harness describe` reports `armed` instead of an
`enabled` that is false for every scheduled harness by construction.

```sh
harness jobs                    # every scheduled harness: next run, last run, consecutive failures
harness runs <name>             # its run history, newest first (--limit N, default 20)
harness trigger <name>          # run it now — on_overlap applies, as for a firing
harness trigger <name> --wait   # …stream the run's log and exit with its exit code
harness logs <name> --run 3     # what run 3 did (--raw for its own log)
```

`trigger --wait` exits with the run's own exit code, `124` when the run timed
out, and `75` when it was skipped because a run was already in flight — so a job
scripts like the command it wraps. The verb is `trigger` because `harness run`
starts a throwaway scratchpad.

`--wait` polls the run history rather than consuming events, because history is
authoritative even when an event is dropped. When the trigger was queued behind
a run already in flight, `--wait` attaches to the oldest manual, non-skipped run
newer than the moment it was issued — so if two manual triggers fire
concurrently, either may pick up the other's run and both stream the same log.
Scheduled firings never collide this way.

## Operating hours

A gated harness — one with `operating_hours` set (see
[Configuration → Operating hours](./configuration#operating-hours)) — is a
resident harness the daemon holds down outside its configured windows,
without ever touching `enabled`. `list`, `describe` and the TUI reuse the same
STATE/SCHEDULE/NEXT columns `harness jobs` does (no extra column) rather than
inventing a parallel set:

- **`off-hours`** replaces `stopped` for a held harness — it is down because
  its window is closed, not because someone turned it off. `stopped` is the
  same latch a give-up produces, so conflating the two would make an
  operating-hours hold page like a real failure. NEXT reads `opens Mon 09:00`.
- **`closing`** replaces the process's own state (`running`, `degraded`, ...)
  while a graceful close is in flight: the harness is still up, finishing its
  current turn before it stops. NEXT reads `stops by 13:15` — the close's own
  deadline, which can run past the window's own close time by up to
  `hours_shutdown_timeout`.
- A gated harness running in hours shows NEXT as `closes 13:00`.
- SCHEDULE shows the `operating_hours` expression, with a `TZ=`/`CRON_TZ=`
  prefix dropped when it names the daemon's own zone (`$TZ`), the same way a
  cron `schedule`'s zone is handled.

```sh
harness start <name>              # gated + out of hours: a one-hour after-hours lease
harness start <name> --for 3h     # …for a chosen length instead
```

`--for` only applies to a gated harness that is currently out of hours; on any
other harness it is rejected. The lease shows in NEXT as `lease until 21:00`
until it ends — the gate stops the harness exactly as it would at a close — or
until hours open first, when the lease simply ends and the harness keeps
running as an ordinary in-hours one. `harness stop <name>` always stops the
harness, ends any lease, and clears `enabled`, whatever state it is in.

`harness doctor` warns on a gated harness with `enabled = false` (hours will
never start it), an `operating_hours` expression covering the entire week (it
gates nothing), and graceful shutdown on a harness nothing can ever be
attributed to (a `generic` adapter, or no `workdir`) — every close then
degrades to immediate no matter what `hours_shutdown` says.

## Logs

```sh
harness logs <name>           # tail (default 200 lines)
harness logs <name> --lines 50    # a specific number of trailing lines
harness logs <name> --follow      # stream new output as it arrives
harness logs <name> --run 3       # one run of a scheduled harness (see harness runs)
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
`--ssh`, `--ssh-listen`, `--webhook-listen`, `--log-level`, `--log-file`, `--detach`.

## Exit codes & error handling

Every error is classified and rendered as a styled error box with an actionable
hint. A `--json` error still prints a structured object. Exit code is 0 on
success, non-zero on failure (`doctor` returns non-zero when any check fails;
`trigger --wait` returns the run's exit code, as described under
[Scheduled jobs](#scheduled-jobs)).

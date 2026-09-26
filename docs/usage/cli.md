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

On a terminal, a single-harness verb shows a spinner while the daemon works,
then records the transition in the state's colour, with a faint context line:

```text
● claude-rc  failed → running
  restarted · pid 48211 · 3 restarts · remote-control claude
```

A harness that comes out `failed` or `degraded` adds a
`→ see why: harness logs <name>` hint. The other mutating verbs (`use-profile`,
`reload`, `down`, `rm`, `run`, `trigger`, `daemon stop`) get the same styled
treatment on a terminal.

Piped or redirected output stays exactly one plain line per result, e.g.
`● claude-rc → running` or `reloaded — 12 harnesses`, so scripts are
unaffected. `--json` is unchanged. Colour follows your terminal's
capabilities and `NO_COLOR`; the glyph and state words carry the meaning
without it.

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
starts a throwaway [scratchpad](#scratchpads-harness-run).

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
without ever touching `enabled`. (On a harness with `triggers`, hours gate
firings instead, and nothing below about holding or closing applies — see
[Configuration → On a triggered harness](./configuration#on-a-triggered-harness-hours-gate-firings).)
`list`, `describe` and the TUI reuse the same
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
attributed to (a `generic` or `command` adapter, or no `workdir`) — every
close then degrades to immediate no matter what `hours_shutdown` says. On a
harness with `triggers` only the whole-week warning applies: it must be
`enabled = false`, and it has no resident process to close.

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
harness doctor                # health check battery (config, daemon, versions, remote SSH, notify, harnesses)
harness doctor --notify-test  # also have the daemon run the [notify] hook once with a test event
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

## Capture (screen dump without a TTY)

```sh
harness capture <name>        # the harness's current screen, as plain text
harness capture <name> --ansi # the styled ANSI repaint (as attach's first frame)
harness capture <name> --json # the full payload: text, ansi, viewport, idle ms
```

`capture` is the non-interactive `attach`: it renders what is on the harness's
terminal right now so a script — a monitoring sweep, an alert pipeline — can
read it. This matters because `harness logs` deliberately records only
**scrolled-off** lines ([ADR-0007](https://github.com/stump-wtf/harness)): a
full-screen prompt that repaints in place never scrolls, so it never reaches
the log. When a screen has never received any output, `capture` answers
`no screen` rather than pretending it is blank.

## Waiting for input

A harness frozen at an interactive prompt — a permission dialog, a workspace
trust screen, a consent wall — reports `running` like any healthy harness, and
its log is silent because the prompt never scrolled. `harness list` and
`describe` surface this as a distinct marker:

```sh
NAME    STATE                  ...
stuck   ● running ⏸ waiting for input (y/n prompt) · screen idle 4m12s
```

The verdict requires both halves: the screen has produced no output for a
while (30 seconds by default), AND a known interactive-prompt pattern is
visible on it. A busy agent repaints constantly and never trips it; an idle
but prompt-free screen (an idle shell, a finished turn) does not either. The
process lifecycle state is unchanged — `waiting` says what the **glass** is
doing, `state` says what the **process** is doing
([ADR-0040](https://github.com/stump-wtf/harness)).

## Scratchpads (`harness run`)

```sh
harness run claude                     # Claude Code in the current directory, then attach
harness run crush --yolo               # the words after the kind are the agent's args
harness run htop                       # not a kind: runs `sh -c "htop"`
harness run --detach codex             # print the name and leave it running
harness run --name spike --workdir ../api claude
```

`harness run` starts a **scratchpad**: a throwaway harness that exists only
until the daemon exits. It is the `tmux new-session` gesture. A scratchpad:

- is **not** in any `harness.toml`, so it is never reloaded, rescheduled or
  autostarted;
- gets a **random name**: a slug of the kind and words plus four random
  characters, such as `claude-code-x4yx` or `generic-htop-9k2p`, printed when it
  starts;
- starts, then **attaches** you to it, unless you pass `--detach` or either
  stdin or stdout is not a terminal;
- is **not restarted** when its process exits. It stays in `harness list` with
  its exit state until you remove it with `harness rm NAME`;
- is **gone when the daemon exits**, and never written to the daemon's state.

The first word picks what runs. If it names a harness kind (`claude` or
`claude-code`, `crush`, `codex`, `generic`), that adapter runs and every
following word becomes its `args`. Anything else falls back to `generic`, and
the **whole invocation** runs as one `sh -c` command. `run`'s own flags must come
before that first word; everything after it belongs to the command, so
`harness run htop -t` reaches the shell as `htop -t`.

| Flag | What it does |
|------|--------------|
| `--kind KIND` | Use this adapter (`crush`, `claude-code`, `codex`, `generic`) instead of guessing from the first word, and pass **every** word as its args. For the rare command whose name collides with a kind. |
| `--name SLUG` | Use this slug instead of the kind or command. A random suffix is still appended. |
| `--workdir DIR` | Start in `DIR` instead of your current directory. A relative path resolves against your shell's directory, not the daemon's. |
| `--model MODEL` | Prepend `--model MODEL` to the agent's args. Same as writing `harness run claude --model MODEL`. |
| `--detach` | Don't attach: print the name and leave it running. `--json` implies it. |

Use a scratchpad for a one-off session you want to detach from and come back to.
Anything that should survive a daemon restart, run on a clock, or fire on an
event belongs in `harness.toml` instead: a resident harness, or a prompt harness
you fire with `schedule`, `triggers` or [`harness trigger`](#scheduled-jobs).

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

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

`harness stop <name>` on a scheduled one-shot pauses the schedule itself: the
process stops, and the cron stops firing — `harness describe` reads
**`next run: suppressed by harness stop (harness start re-arms it)`** — until
an explicit `harness start`, which re-arms it (`harness trigger` runs once
without re-arming). The pause survives a daemon restart. Windows that pass
while stopped leave a skipped run record instead of firing.

A harness fired by trigger sources (`triggers`, see
[Trigger sources](#trigger-sources)) is a one-shot too, and reads the same
way: `↯ armed` between firings, never `stopped` or `(disabled)`. Its SCHEDULE
column lists its sources (`webhook.ci, channel.sb`, after the cadence when it
also has a `schedule`), and NEXT reads `on event` when there is no window to
count down to.

```sh
harness jobs                    # every triggered harness: sources, next run, last run, consecutive failures
harness runs <name>             # its run history, newest first (--limit N, default 20)
harness trigger <name>          # run it now — on_overlap applies, as for a firing
harness trigger <name> --wait   # …stream the run's log and exit with its exit code
harness logs <name> --run 3     # what run 3 did (--raw for its own log)
```

### Run history across harnesses

Every run of every harness, one-shot firings and resident process lifetimes
alike, is a record in the run ledger (`$XDG_STATE_HOME/harness/ledger/`,
ADR-0028). `harness runs` queries it:

```sh
harness runs                                   # every harness, the last 24 hours (--limit, default 50)
harness runs pr-review nightly --since 7d      # these harnesses, the last week
harness runs --since 24h --outcome failed,timed_out,interrupted --json   # a morning sweep
harness runs --trigger webhook,channel --wide  # add MODEL, TOKENS, COST and TODO
```

| Flag | Meaning |
|------|---------|
| `NAME...`, `--harness NAME` | harnesses to include (repeatable); none means every harness |
| `--since DUR\|TIME` | runs started since a duration ago (`7d`, `36h`) or an RFC 3339 instant; default `24h` for a query |
| `--until TIME` | runs started before an RFC 3339 instant |
| `--outcome O[,O...]` | only these outcomes; an unknown value fails and lists the valid ones |
| `--trigger T[,T...]` | only these triggers (`schedule`, `manual`, `catch_up`, `channel`, `webhook`, `autostart`, `restart`, `release`, `lease`) |
| `--limit N` | at most N records, newest first (1–1000) |
| `--wide` | add MODEL, TOKENS, COST and TODO |
| `--json` | the records as a JSON list |

`harness runs NAME` with no other filter is the one-harness history it has
always been: newest first, default 20, and `--json` prints the same
`{"name": …, "runs": […]}` object as before, with new fields only added. Each
run's MODEL cell in `--wide` shows the served model with the most output
tokens among that run's model calls (SPEC-0022 REQ-4).

With the daemon down, `harness runs` reads the ledger's files directly and says
so on stderr. It changes nothing on disk, and it shows a run the daemon left
open as `running?`, since it cannot tell a live run from one a crash left
behind. A harness redeploy after a crash kills scratchpad workers and drops
runs that were in flight at cut-over — the ledger reflects when the daemon
exited ([ADR-0007](https://github.com/stump-wtf/harness)).

`trigger --wait` exits with the run's own exit code, `124` when the run timed
out, and `75` when it was skipped because a run was already in flight — so a job
scripts like the command it wraps. The verb is `trigger` because `harness run`
starts a throwaway [scratchpad](#scratchpads-harness-run).

A `command` harness whose `argv` template needs a value the run does not have
(a required `{{run.source}}` on a manual trigger, say) runs nothing: the run is
recorded `skipped`, and `harness runs` prints a line under the table naming the
missing path (`run #4 skipped: template_unresolved ({{run.source}} has no value
for a manual run)`). `--json` carries it as `"reason": "template_unresolved"`
and `"missing_path": "run.source"`, and `trigger --wait` exits `75`. See
[Argv templates](./configuration#argv-templates).

`--wait` polls the run history rather than consuming events, because history is
authoritative even when an event is dropped. When the trigger was queued behind
a run already in flight, `--wait` attaches to the oldest manual, non-skipped run
newer than the moment it was issued — so if two manual triggers fire
concurrently, either may pick up the other's run and both stream the same log.
Scheduled firings never collide this way.

`jobs` lists every harness with a `schedule`, `triggers`, or both. Its
TRIGGERS column shows each source with that source's state
(`webhook.ci listening, channel.sb backoff`), so a job that never runs starts
its explanation in the row; a harness with no `schedule` shows no next
window.

## Trigger sources

```sh
harness triggers                # every [channel.*] / [webhook.*] source
harness triggers --json         # …with every per-outcome counter
```

`harness triggers` answers "did anything hear the doorbell?" — the one
question no run record can, because a source that never fired leaves none.
One row per declared source:

| Column | Meaning |
|---|---|
| SOURCE | The reference a harness's `triggers` names, e.g. `channel.sb` |
| STATE | `connecting`, `connected`, `backoff` or `error` for a channel; `listening` or `no_listener` for a webhook; `disabled` (`enabled = false`) or `unbound` (no harness lists it) for either |
| FOR | How long it has been in that state. For a channel that is not connected, how long it has been **down** — `down 10m` — measured from when it left `connected`, not from its latest retry |
| LAST EVENT | When it last fired |
| FIRED | Events fired since the daemon started |
| HARNESSES | The harnesses it fires, in config order |

Under the table, each source gets its endpoint — a channel's `url` with the
query removed and its header **names**, or a webhook's `POST /hooks/<name>`,
`verify` scheme and `events` — then its last error, and any deliveries it
dropped by outcome. `--json` carries the same fields plus every counter
(`fired`, `ignored`, `duplicate`, `unauthorized`, `too_large`,
`rate_limited`, `invalid`), zeros included.

No output of `triggers`, `describe` or the event stream carries a header value,
a URL query or a secret: the daemon scrubs a channel's last error of both
before it leaves the daemon, because Go's HTTP client quotes the request URL
in its errors. `describe` lists a harness's triggers with each source's state.

A client subscribed to events receives `trigger_source_changed` (`source`,
`source_kind`, `state`, `error`) on every source state change, in order, and
`job_run_started`/`job_run_finished` carry the `source` that fired the run.

`harness doctor` adds a `triggers` row that flags the setups that fail
quietly: a webhook listener bound off loopback without TLS, a `[webhook.*]`
source no listener serves (`no_listener`), and a channel source in `error`.
With the daemon down it still checks what the config alone can show. A source
`env_file` readable by group or other is a config load warning instead: the
daemon logs it when it loads the config and on every reload, and doctor's
`config` row lists it with the source and the file.

:::note Rejected webhook deliveries are not counted yet
`fired` and a channel's `invalid` are counted today. A webhook delivery the
listener rejects — `unauthorized`, `too_large`, `rate_limited`, `duplicate`,
`ignored` — is answered and logged but not yet counted, so those counters read
`0` until the listener reports them.
:::

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

A [stream-json one-shot](./configuration#stream-json-one-shots-run-without-a-terminal)
(a `claude-code` prompt harness) has no terminal, and its stdout is not in its
durable log. `harness logs <name> --raw` shows that log's lifecycle and stderr
lines, with a note saying where the output went. `harness logs <name> --run N
--raw` prints run N's `.stream.jsonl` tail, each line cut at 64 KiB, and once
the run has ended, its run log after it, each under a `==> path <==` header.
`harness trigger <name> --wait` streams the same text as the run goes. The file
itself is whole: read it with `jq` for anything longer.

`harness logs` reads the daemon's files for you, compressed or not. A rotated
backup and a closed run's log and stream are stored zstd-compressed
(`.log.zst`, `.stream.jsonl.zst`) by default. Read one by hand with
`zstd -dc FILE.zst`, or search plain and compressed logs together with
`zstdgrep`. See
[Supervision → Logs on disk](./supervision#logs-on-disk) for which files are
compressed and when, and for the `compress_logs` opt-out.

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

A stream-json one-shot has no terminal to attach to, and the daemon keeps no
screen for it. Attaching shows its output as lines: its recent lines first,
then new ones as they arrive, rendered readably — the same formatter the
dashboard's preview uses, so Claude Code's stream becomes the tool calls and
prose a human reads instead of masked JSON — with `Ctrl-b f` flipping back to
the byte-faithful raw mirror. Keystrokes go nowhere, since its stdin is
`/dev/null`, and resizing the window changes nothing for it. A client too slow
to keep up sees an `output dropped` line where it fell behind; the run's
`.stream.jsonl` has everything. `harness describe` lists the sessions, with no
viewport.

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
but prompt-free screen (an idle shell, a finished turn) does not either.

To see what tripped the marker, `harness capture <NAME>` prints the screen the
verdict came from; `harness attach <NAME>` takes over the interactive session. The
process lifecycle state is unchanged — `waiting` says what the **glass** is
doing, `state` says what the **process** is doing
([ADR-0044](https://github.com/stump-wtf/harness)).

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

## Agent packages

`harness agent` installs agent packages from trusted git repositories called
**stables** (ADR-0044, SPEC-0026). It is a client-only tree that edits your
global `harness.toml` directly, so most verbs work with no daemon running;
when one is reachable, the verb asks it to reload. The walkthrough is
[Install shared agents from a stable](../guides/agent-packages).

```sh
harness agent stable add NAME REMOTE [--private]   # clone, then write [stable.NAME]
harness agent stable list                           # trusted stables and their clones
harness agent stable update [NAME]                  # fetch + fast-forward (the only fetch)
harness agent stable remove NAME                    # drop the table; names harnesses still sourced

harness agent search [QUERY] [--stable NAME]        # packages in local clones
harness agent info STABLE/PACKAGE                   # manifest, requests, scan findings, files

harness agent install STABLE/PACKAGE[@VERSION] [--as NAME] [--replace] [--yes] [--force-unsafe]
harness agent upgrade STABLE/PACKAGE[@VERSION] [--all] [--yes]
harness agent list [--json]                         # package-sourced harnesses, pins, NEWER
harness agent uninstall NAME [--yes]                # remove the table; the pin stays
harness agent prune                                 # remove pins nothing references
```

- **Only `stable update` fetches.** Every other verb reads the local clone.
  `agent list`'s `NEWER` column compares against the clone as the last update
  left it.
- **`install`** pins the package at an exact commit in the content-addressed
  store, scans it, and asks before it writes `source = "STABLE/PACKAGE@<sha>"`
  onto `[harness.NAME]`.
  - Without a terminal it refuses unless `--yes`.
  - `--yes` never clears a `high` scan finding or an `mcp_allow = ["write"]`
    request.
  - `--force-unsafe` overrides a `high` finding after you retype the package.
  - `--replace` lets the table switch from another package's source.
- **`upgrade`** re-pins to a newer commit of the local clone (`--all` for
  every sourced harness). It shows the manifest diff, any new scan finding,
  and every change to the harness's effective values. It never applies those
  changes unattended: `--yes` refuses, naming them.
- **`prune`** reads only the global `harness.toml`. A pin referenced only by
  a project file is removed, and that project's next `up` reports it missing.

`harness describe NAME` marks each value a package supplied with
`(package)`, and `harness doctor` reports a sourced harness whose pin is
missing.

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

Daemon flags: `--config`, `--socket`, `--scrollback-bytes SIZE` (per-harness
scrollback ring storage, default `1MiB`), `--scrollback N` (and at most N lines
of it), `--ssh`, `--ssh-listen`, `--webhook-listen`, `--log-level`, `--log-file`,
`--memory-limit SIZE`, `--pprof-addr H:P`, `--detach`.
All but `--config` and `--detach` can also be set in `harness.toml`
(`[daemon]` / `[server]`) or a `HARNESS_*` variable; see
[Configuration → Environment variables](./configuration#environment-variables).

- `--memory-limit` (`HARNESS_MEMORY_LIMIT`, `[daemon] memory_limit`) sets the
  daemon's Go soft memory limit, e.g. `2GiB`; `0` is off. Any of the three
  overrides `GOMEMLIMIT`. With none set, `GOMEMLIMIT` applies if present, and
  otherwise there is no limit.
- `--pprof-addr` (`HARNESS_PPROF_ADDR`, `[daemon] pprof_addr`) serves
  `/debug/pprof/` on a loopback address. A non-loopback address makes the daemon
  refuse to start.

See [Memory limit and profiler](./configuration#memory-limit-and-profiler).

## Exit codes & error handling

Every error is classified and rendered as a styled error box with an actionable
hint. A `--json` error still prints a structured object. Exit code is 0 on
success, non-zero on failure (`doctor` returns non-zero when any check fails;
`trigger --wait` returns the run's exit code, as described under
[Scheduled jobs](#scheduled-jobs)).

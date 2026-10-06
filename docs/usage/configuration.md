---
title: "Configuration"
sidebar_position: 3
---

# Configuration

Harness reads a single TOML file. The default location is
`~/.config/harness/harness.toml` (honoring `$XDG_CONFIG_HOME`), overridable on
every verb with `--config`. A repo can also carry its own project-scoped
`harness.toml` — see [Projects](./projects).

A copy of the example lives in the repo as `harness.toml.example`.

## Harnesses

Each `[harness.<name>]` table defines one supervised process.

```toml
[harness.my-agent]
harness = "claude-code"
args = ["--foo", "bar"]
workdir = "~/src/my-project"
description = "long-running agent in ~/src/my-project"
enabled = false
```

| Field | Meaning |
|-------|---------|
| `harness` | **required** — the harness kind, an enum: `crush`, `claude-code`, `codex`, `pi`, `omp`, `generic`, `command`. There is no default; every harness says what it runs. It selects the adapter, which owns the executable a long-running harness runs — `args` are appended after it. `generic` runs `sh`, so its `args` are **sh's** args. To run any other program, use `command` with an `argv` — see [The `command` kind](#the-command-kind). `generic` takes no `prompt` or `prompt_file` — see [Agent adapters](#agent-adapters) |
| `args` | argument list appended after the adapter's executable. Not accepted on `command`, which takes `argv` instead |
| `argv` | `command` only: the whole process, `argv[0]` first, exec'd **without a shell**. See [The `command` kind](#the-command-kind) |
| `transcripts` | `command` only: observe the harness's sessions as those of `claude-code`, `crush`, `codex`, `pi` or `omp`. See [Binding transcripts](#binding-transcripts) |
| `workdir` | working directory (**required** for most commands) |
| `env_file` | optional `KEY=VALUE` file sourced before launch (secrets stay here, out of the config). Also accepts a **list** of files loaded in order, a later file winning a key collision — `env_file = ["claude.env", "reviewer.env"]` — so a shared credential file and a per-persona one compose without copying. A missing file is tolerated, exactly as a missing string is; an empty list is a load error |
| `description` | free-text shown in the dashboard |
| `enabled` | autostart on daemon boot / after a daemon restart (`true`) |
| `restart` | restart policy on exit — see [Restart policy](#restart-policy) |
| `restart_delay` | seconds to wait before restarting a harness that exited |
| `backend` | hosting strategy: `native` (default) or `tmux` |
| `tmux_socket` | tmux socket name; only used when `backend = "tmux"` |
| `export_telemetry` | publish this harness's agent activity to the [telemetry](#telemetry-export-telemetry) destinations: `true` opts in, `false` opts out (and beats `export_all`), unset follows `[telemetry] export_all`. A project file may set only `false`. Applies on reload |

An **agent one-shot** replaces `args` with a `prompt` (or a `prompt_file`) —
see the next section.

A bare `[name]` table is accepted for backward compatibility, but the
`[harness.<name>]` form is preferred (ADR-0006).

## Agent one-shot harnesses

Give a harness a `prompt`, and the daemon synthesizes the agent invocation at
spawn time from the harness's adapter (see the table below). This turns a
harness into a one-shot agent run:

```toml
[harness.deploy-check]
harness = "claude-code"
prompt = "check the deployments and report anything unhealthy"
model = "claude-opus-5"        # optional model (requires prompt)
auto_accept = true             # optional: unattended/yolo mode (requires prompt)
max_turns = 20                 # optional: turn budget, 0 = unlimited (requires prompt)
# the one-shot is headless by default; set quiet = false to stream output
# quiet = false
workdir = "~/src/my-project"
# one-shot runs default to restart = "no" (a completed run must not respawn);
# set restart explicitly to opt back into supervision
```

| Field | Meaning |
|-------|---------|
| `prompt` | the agent instruction. Mutually exclusive with `args` and with `prompt_file`, and not accepted on `harness = "generic"`; stored verbatim (never placeholder-expanded) and synthesized into the agent argv at spawn from the same `harness` adapter |
| `prompt_file` | path to a file whose contents are the instruction — the alternative to an inline `prompt` for anything too long for one TOML line. See below |
| `model` | which model the agent runs, e.g. `claude-opus-5`. Requires `prompt`; folded into the synthesized argv |
| `auto_accept` | run unattended, bypassing the agent's permission prompts (the vendor's yolo flag). Requires `prompt`; fold into the synthesized argv |
| `max_turns` | cap on how many iterations the agent may run before stopping. Requires `prompt`; 0 or omitted means unlimited |
| `system_prompt_file` | **claude-code one-shots only.** Path to a persona file appended to Claude Code's system prompt — it appends, it never replaces. Requires `prompt`/`prompt_file`; the path resolves against the declaring file, and a missing file fails the load |
| `mcp_config` | **claude-code one-shots only.** Path to an MCP servers file, emitted as `--mcp-config <path> --strict-mcp-config`, so the run sees exactly the servers the persona names and nothing from your own configuration |
| `allowed_tools` | **claude-code one-shots only.** List of tool permissions, each entry its own argv element after `--allowedTools` — `["Read", "Bash(git log:*)"]`. An entry that is blank or starts with `-` is a config error |
| `quiet` | run headless (suppress the agent's interactive output). Defaults to `true` for a prompt one-shot; set `false` to stream output to whoever attaches |

These agent fields are **config truth only** — they are never written into
`args` (that would corrupt the edit-form round-trip). The daemon folds them
into the synthesized agent argv at spawn time, and they **require `prompt`**:
there is no vendor-agnostic place to inject a flag into an arbitrary
argv, so a long-running harness passes its tool's flags through `args` itself.

Each adapter maps those fields onto its own CLI. A field the CLI has no flag for
is dropped, not emulated:

| `harness` | Synthesized command | Ignored fields |
|-----------|---------------------|----------------|
| `crush` | `crush [--yolo] run [--quiet] [--model M] <prompt>` | `max_turns` (Crush has no turn cap) |
| `claude-code` | `claude -p [--dangerously-skip-permissions] [--model M] [--max-turns N] [--append-system-prompt-file F] [--mcp-config F --strict-mcp-config] [--allowedTools T…] --verbose --output-format stream-json <prompt>` | `quiet` (`-p` is already headless) |
| `codex` | `codex exec [--model M] [--full-auto] <prompt>` | `quiet`, `max_turns` |
| `pi` | `pi --print [--model M] <prompt>` | `auto_accept` (Pi has no permission prompts), `max_turns`, `quiet` (`--print` is already headless) |
| `omp` | `omp --print [--model M] <prompt>` | `auto_accept`, `max_turns`, `quiet`, as for `pi` |
| `generic` | none — a `prompt` or `prompt_file` on `generic` is a config error | — |

⚠️ `auto_accept` bypasses **ALL** of the agent's permission prompts. Only enable
it on trusted, headless runs.

### Stream-json one-shots run without a terminal

A `claude-code` one-shot prints `stream-json`, one JSON object per line, so the
daemon runs it on pipes rather than under a PTY
([ADR-0033](/decisions/adr-0033-trace-first-run-records)). Every other kind
keeps its PTY: `crush`, `codex`, `pi` and `omp` one-shots, every resident
harness, and every `command` harness, whatever its `argv`.

- **No terminal.** The process has no controlling terminal and its stdin is
  `/dev/null`. The prompt is on the argv, so nothing waits on input. Stop,
  `timeout`, `on_overlap = "replace"` and the budgets reach its whole process
  group, as they do for a PTY harness.
- **stdout** is kept per run as `jobs/<name>/<run_id>.stream.jsonl` (see
  [Runs](#runs-history-logs-timeout-overlap)), masked line by line. A prompt
  harness with no `schedule` or `triggers` has no per-run files, so its stdout
  goes to its durable log (`logs/<name>.log`).
- **stderr** goes to the run's log and the durable log, masked, with escape
  sequences stripped.
- **No emulator.** Neither the log sanitizer nor the attach plane builds a
  terminal emulator for these runs, so the daemon's memory no longer grows with
  how much a run prints, and the lines reach the logs as the agent wrote them.
  There is no screen and no viewport: `harness attach` and the TUI preview show
  the recent output lines, then new ones as they arrive; see
  [CLI → Attach](./cli#attach).

### Prompts that live in a file

A TOML basic string carries no raw newline, so a prompt of any real length ends
up as one unreadable line. `prompt_file` names a file instead:

```toml
[harness.blog-sweep]
harness = "claude-code"
prompt_file = "~/.config/prompts/blog-sweep.md"
model = "claude-opus-5"
schedule = "0 9 * * 1"
```

- **Mutually exclusive with `prompt`**, and either one satisfies the "requires
  `prompt`" rule for `model`, `auto_accept`, `max_turns`, `quiet` and
  `schedule`.
- **The path is resolved at load; the file is read at spawn.** `harness.toml`
  stores the path, so editing the prompt takes effect on the next run with no
  reload — and config writers (the TUI edit form) round-trip the path rather
  than inlining the document.
- **A leading `~` expands, and a relative path resolves against the file that
  declared it** — the config file's own directory, the `harness_d` file's
  directory, or the project root in a project `harness.toml`. This is stricter
  than `workdir`/`env_file` on purpose: the daemon runs with an arbitrary
  working directory, so a cwd-relative prompt would mean different things to
  the CLI and the daemon.
- **A missing, unreadable, or empty file fails the config load** with a located
  error, and is re-checked at spawn. Unlike `env_file`, it is not optional: a
  harness with no instruction has nothing to run, and a scheduled one firing
  into an empty prompt would look like a successful no-op.

## Scheduled one-shots

Give an agent one-shot a `schedule` (a standard cron expression, validated at
config load) and the daemon fires it on that cadence — ADR-0013's replacement
for the original SPEC-0008 timer design:

```toml
[harness.fleet-sweep]
harness = "crush"
prompt = "check all services and report anything unhealthy"
auto_accept = true
schedule = "CRON_TZ=UTC 0 */6 * * *"   # every 6 hours
description = "fleet health sweep"
```

For a full walkthrough — prompt design, run history, and reading outcomes — see
the [Scheduled sweeps guide](/guides/scheduled-sweeps).

Rules:

- Requires `prompt` or `prompt_file`, or `harness = "command"` — only
  one-shots can be scheduled, and a command harness's argv is its whole job
  (see [The `command` kind](#the-command-kind)).
- At each firing the harness starts if no run is in flight; a firing that lands
  mid-run follows `on_overlap` (below), and never stacks a second process.
- The run exiting is terminal for that firing; the restart policy applies only
  to abnormal exit, so only `restart = "no"` (the prompt default) and
  `"on-failure"` are accepted here.
- Mutually exclusive with `enabled = true` and with profile membership.
- Global config only — project files reject the key.
- `harness stop <name>` pauses the schedule until an explicit
  `harness start` (a `harness trigger` runs once without re-arming); the
  pause survives a daemon restart, and windows that pass while stopped are
  recorded as skipped runs
  ([CLI → Scheduled jobs](./cli#scheduled-jobs)).

### Time zones

By default an expression runs in the daemon's local zone. Prefix it with
`CRON_TZ=<zone>` (or `TZ=<zone>`) to pin it to an IANA zone regardless of where
the daemon runs:

```toml
schedule = "CRON_TZ=UTC 0 9 * * *"   # 09:00 UTC on every host
```

Across daylight-saving changes, a schedule that names a time of day runs once
per day: a time the clock skips (02:30 on spring-forward day) runs just after
the jump, and a time the clock repeats runs only the first time. A schedule that
fires every hour, or an `@every` interval, keeps running once per real hour.

### Sleep, outages, and `catch_up`

`catch_up` is never periodic: however many firings were missed, it starts at
most **one** run. It is accepted where something can miss a firing —
`schedule`, a channel trigger, or `operating_hours` on a harness with
`triggers` — and is rejected on presence anywhere else. The three cases:

**Schedule.** The daemon checks the wall clock every second rather than
setting a timer, so a laptop that sleeps through a window notices the moment
it wakes, and a daemon that starts after an outage notices at boot. A window
noticed more than a minute late is **missed**, and `catch_up` decides what
happens:

```toml
[harness.nightly-sweep]
harness = "crush"
prompt = "…"
schedule = "CRON_TZ=UTC 0 3 * * *"
catch_up = true   # default false
```

- `catch_up = true` — run **once** on wake or boot, however many windows were
  missed.
- `catch_up = false` (default) — run nothing and log a `scheduled run MISSED`
  warning in the daemon log naming the harness, the first and last missed
  windows, and how many there were. The next window fires normally.

**Channel trigger.** On a harness with a `channel.…` trigger, one run when
the channel connects for the **first time since the daemon started**, and one
on every reconnect after the link was down for **more than a minute**. A
flapping link re-fires about once per backoff ceiling (its backoff resets only
after five open minutes, so a link that drops sooner every time climbs past
the minute within a few flaps); blips shorter than a minute do not, because
Switchboard re-rings unclaimed todos within minutes. The run carries no event
file — it is the agent's cue to go and look
([push events](../guides/push-events#catch_up-when-the-channel-was-down)).

**Operating hours.** On a triggered harness with `operating_hours`, one run
at the first in-hours check after the window reopens, if anything was skipped
while it was closed
([on a triggered harness](#on-a-triggered-harness-hours-gate-firings)).

The daemon records the last window it decided in `state.json`, so a restart
never runs the same window twice. A missed window is also a `missed` entry in
the harness's run history.

### Runs: history, logs, timeout, overlap

Every run of a scheduled harness gets a numbered record and a log of its own:

```toml
[harness.nightly-sweep]
harness = "crush"
prompt = "…"
schedule = "CRON_TZ=UTC 0 3 * * *"
timeout = "45m"        # default "1h"; "0" = no limit
on_overlap = "queue"   # default "skip"; or "replace"
keep_runs = 30         # default 20
```

- **History** lives in the run ledger, `$XDG_STATE_HOME/harness/ledger/`: one
  append-only JSONL file per UTC day, private to your user (ADR-0028). Each run
  records its id, trigger (`schedule`, `manual`, `catch_up`, `channel`,
  `webhook`), start, end, exit code, and an outcome: `success`, `failed`,
  `timed_out`, `skipped`, `replaced`, `missed`, `cancelled`, or `interrupted`.
  Firings that start nothing (skipped, missed) are recorded too. Run ids never
  repeat. A run the daemon crashed under reads `interrupted` (reason
  `daemon_crash`) on the next boot; one a clean shutdown stopped reads
  `interrupted` (reason `shutdown`).
- **Logs** are at `$XDG_STATE_HOME/harness/jobs/<name>/<run_id>.log` — the run's
  output history and lifecycle lines, alongside the usual harness log. Once the
  run closes, its log is compressed to `<run_id>.log.zst` (unless
  [`compress_logs = false`](#daemon-settings-daemon)); `harness logs <name> --run
  N` reads either. `keep_runs` bounds these log files only: the oldest logs are
  deleted, and their records stay in the ledger, marked `log_pruned`.
- **Streams.** A [stream-json one-shot](#stream-json-one-shots-run-without-a-terminal)
  also writes `jobs/<name>/<run_id>.stream.jsonl`: its stdout, one masked JSON
  line per line, private to your user. Its `<run_id>.log` then holds the
  lifecycle lines and the agent's stderr. The stream is compressed to
  `<run_id>.stream.jsonl.zst` when the run closes, like the log. `keep_runs`
  deletes a run's stream with its log. `harness logs <name> --run N --raw`
  prints the stream, then the run log once the run has ended.

:::note Upgrading from a release before the ledger
The first daemon that has the ledger copies each harness's run history out of
`state.json` into `ledger/` once, then drops it from `state.json`, which keeps
only each harness's last run id. Downgrading past that release loses the view
of run history.
:::
- **`timeout`** stops a run that goes on too long: SIGTERM, then SIGKILL after
  the stop grace. The run is `timed_out` and the harness shows `failed`.
- **`on_overlap`** decides a firing that lands while a run is still going:
  `skip` records it and does nothing; `queue` runs it once the current run
  finishes (holding at most one); `replace` stops the current run and starts the
  new one.

All three keys require `schedule`. Read history and per-run logs from the CLI
with `harness jobs`, `harness runs <name>` and `harness logs <name> --run N`, and
run a job now with `harness trigger <name>` — see
[CLI → Scheduled jobs](./cli#scheduled-jobs).

`harness list` gives the schedule its own columns — `SCHEDULE` (the cadence) and
`NEXT` (the countdown) — and reads the state as `⏱ armed`, since a cron job
between firings is waiting rather than switched off. An unscheduled harness shows
an em dash in both columns.
`harness describe` adds an `armed` row, the cron spec and the absolute next-run
time. A
zone-prefixed schedule's cadence carries the zone (`daily 09:00 UTC`). The
[cockpit](./tui#the-dashboard) tags the row `(scheduled)` with the same
countdown, and carries the cadence on the row's sub-line.

## Operating hours

`operating_hours` gives a **resident** harness a weekly window it is allowed
to run in — ADR-0019's daemon-owned alternative to an external launchd/cron
timer calling `harness start`/`harness stop`. Outside its windows the gate
holds the harness down (stops the process, `enabled` untouched) and starts it
again when a window opens:

```toml
[harness.night-owl]
harness = "claude-code"
args = ["--remote-control", "--continue"]
enabled = true
operating_hours = "TZ=America/Los_Angeles Mon-Fri 09:00-13:00"
hours_shutdown = "graceful"          # default; or "immediate"
hours_shutdown_timeout = "15m"       # default; the most a graceful close may overrun
```

Rules:

- Global config only — project files reject the key, like `schedule`.
- Mutually exclusive with `schedule`: a scheduled one-shot is already
  time-gated by its own cron expression, and two time gates on one harness
  would disagree.
- `enabled` still means "the operator wants this running". Hours sit beside
  it, never overwrite it: a held harness stays `enabled = true`, and
  `harness stop` always stops the harness and clears `enabled`, whatever
  state it is in.
- The gate only ever starts what it held. A harness with `enabled = false`,
  or one you `harness stop`ped, stays down when the next window opens
  (`harness doctor` warns about the first case).
- Outside its hours a harness shows **`off-hours`**, never `stopped` or
  `failed`, so a closed window never reads as a fault
  ([CLI → Operating hours](./cli#operating-hours)).
- `operating_hours` on a harness with `triggers` loads, but gating its
  firings is **coming**
  ([SPEC-0014](/specs/event-triggers/spec)); today a trigger fires it at any
  hour.

### On a triggered harness: hours gate firings

On a harness that sets `triggers` (and no `schedule`), `operating_hours` gates
**firings**, not the process:

```toml
# Assumes [webhook.gitea-pr] and [channel.switchboard] tables are declared
# too (see Webhook listener below); a triggers entry naming an undeclared
# source fails to load.
[harness.pr-review]
harness = "claude-code"
prompt_file = "~/.config/harness/prompts/pr-review.md"
triggers = ["webhook.gitea-pr", "channel.switchboard"]
operating_hours = "TZ=America/Los_Angeles Mon-Fri 09:00-18:00"
catch_up = true   # one run when hours open, if anything was skipped
```

- A doorbell or webhook delivery that arrives outside the window starts
  nothing. The run history gains a `skipped` record with reason
  `outside_hours`, and a burst of them coalesces into one record with a
  `coalesced` count. The webhook's `202` reports that harness as `skipped`.
- The window is judged at the moment the daemon received the event, and it
  is end-exclusive: a delivery at exactly 18:00:00 is out of hours.
- A run already going when the window closes keeps going. Its `timeout`
  bounds it, not the hours.
- With `catch_up = true`, the first check after the window opens starts
  **one** run with trigger `catch_up` if anything was skipped while it was
  closed, however many deliveries that was. The run carries no event file; it
  is the agent's cue to go and look. The daemon remembers the owed catch-up
  across a restart.
- `harness trigger <name>` is never gated.
- `hours_shutdown` and `hours_shutdown_timeout` do not apply here and are
  rejected: there is no resident session to close.

### Grammar and time zones

`operating_hours` is one string: an optional `TZ=<zone>` or `CRON_TZ=<zone>`
prefix (same resolution as `schedule`'s — defaults to the daemon's own zone
when omitted), then one or more `;`-separated windows of
`[day-spec] HH:MM-HH:MM`:

| Written | Means |
| --- | --- |
| `09:00-13:00` | every day, 09:00 up to (not including) 13:00 |
| `Mon-Fri 09:00-13:00` | weekdays |
| `Mon-Fri 09:00-12:00; Mon-Fri 13:00-17:00` | a lunch break |
| `Sat,Sun 10:00-12:00` | a day list |
| `Sun-Thu 22:00-02:00` | overnight — an end at or before its start runs into the next day |
| `Fri-Mon 18:00-23:00` | a day range wraps the week: Fri, Sat, Sun and Mon |
| `CRON_TZ=Europe/Berlin Mon-Fri 08:30-18:00` | pinned to a zone other than the daemon's |
| `Mon-Sun 00:00-24:00` | always in hours (valid, and `harness doctor` warns that it gates nothing) |

`24:00` is accepted only as an end, meaning the end of the day.

### Validation errors

A bad value fails config load with the file, line, harness and key, so
`harness doctor` (or the daemon's reload) shows exactly what to fix:

| Mistake | Error |
| --- | --- |
| `operating_hours = " "` | `"operating_hours" must not be blank` |
| `Mon-Fry 09:00-17:00` | `window "Mon-Fry 09:00-17:00": unknown day "Fry" (want Mon, Tue, Wed, Thu, Fri, Sat, or Sun)` |
| `9-5` | `window "9-5": start time "9": malformed (want "HH:MM")` |
| `09:00-09:00` | `window "09:00-09:00": start and end must not be equal` |
| `TZ=Mars/Olympus 09:00-17:00` | `unknown time zone "Mars/Olympus"` |
| `operating_hours` with `schedule` | `"schedule" and "operating_hours" are mutually exclusive (a scheduled one-shot is already time-gated by its cron expression)` |
| `hours_shutdown` without `operating_hours` | `"hours_shutdown" requires "operating_hours" (a shutdown mode with no hours to close does nothing)` |
| `hours_shutdown = "later"` | `invalid "hours_shutdown" "later" (want "graceful" or "immediate")` |
| `hours_shutdown_timeout = "0s"` | `invalid "hours_shutdown_timeout" "0s" (want a positive duration such as "15m")` |

### Closing: graceful by default

`hours_shutdown = "graceful"` (the default) lets the harness finish its
current turn before stopping it, capped by `hours_shutdown_timeout` (default
`15m`); `"immediate"` stops it at the close without waiting. Either way the
stop is not a crash: no restart, no restart-count increment, no flap, and
`enabled` survives. Graceful shutdown depends on the daemon reading turn-end
markers from the harness's own agent-trace; a harness with nothing
attributable to it (a `generic` or `command` adapter, or no `workdir`) always closes
immediately, and `harness doctor` warns when `hours_shutdown = "graceful"` is
set on one anyway.

### Overrides: an after-hours lease

`harness start <name>` outside a gated harness's hours starts it under a
bounded **after-hours lease** — one hour by default, `harness start <name>
--for 3h` for a chosen length. The gate stops it at the lease's end exactly as
it would at a close; if hours open first, the lease simply ends and the
harness keeps running as an ordinary in-hours one. See
[CLI → Operating hours](./cli#operating-hours) for how a gated harness's
state, lease and close in flight show on every listing surface (`off-hours`,
`closing`, `lease until …`) — no extra column, the existing STATE/SCHEDULE/NEXT
columns carry it.

## Run budgets

:::note Validated, not yet enforced

The keys below load, are checked, and survive a TUI edit, but nothing acts on
them yet: no run is counted or refused, no cost is metered, and no harness is
parked. Admission, quota parking, concurrency and the cost caps land in later
releases ([SPEC-0021](/specs/run-budgets/spec), ADR-0027). Setting them now is
safe, and a bad value fails today with the same located error it will fail
with then.

:::

Budgets bound what an unattended agent may spend: runs per day, tokens and
dollars per run, dollars per day, and how long a harness waits when its
provider says the quota is exhausted. Each harness may carry these keys:

```toml
[harness.pr-review]
harness = "claude-code"
prompt_file = "~/.config/harness/prompts/pr-review.md"
schedule = "*/15 * * * *"
max_runs_per_day = 40          # admissions per budget day
max_tokens = 400000            # per run: input + output + cache writes
max_cost_usd = 2.00            # per run
daily_cost_usd = 20.00         # this harness, per budget day
quota_group = "claude-max"     # harnesses on one allowance park together
quota_backoff = "15m"          # default; first park when no reset time is given
quota_backoff_max = "6h"       # default; the longest backoff park
```

| Key | Value | On |
| --- | --- | --- |
| `max_runs_per_day` | whole number, at least 1 | any harness; on a resident, every process start counts, restarts included |
| `max_tokens` | whole number, at least 1 | one-shots only (`prompt` or `prompt_file` set) |
| `max_cost_usd` | dollars, above 0 | one-shots only |
| `daily_cost_usd` | dollars, above 0 | any harness |
| `quota_group` | 1–64 of `a-z`, `0-9`, `.`, `_`, `-` | any harness |
| `quota_backoff` | duration, at least `1m` (default `15m`) | any harness |
| `quota_backoff_max` | duration, at least `quota_backoff`, at most `24h` (default `6h`) | any harness |

A `[budget]` table sets the daemon-wide limits and the prices used to cost a
run whose agent records tokens but no dollars:

```toml
[budget]
day_starts = "TZ=America/Los_Angeles 00:00"  # default 00:00 in the daemon's zone
max_concurrent = 4                           # triggered one-shot runs in flight at once
daily_cost_usd = 50.00                       # every harness together, per budget day

[budget.prices."claude-sonnet-4-6"]          # per million tokens, by served model name
input_per_mtok = 3.00                        # required
output_per_mtok = 15.00                      # required
cache_write_per_mtok = 3.75                  # optional, default 0
cache_read_per_mtok = 0.30                   # optional, default 0

[budget.group.claude-max]                    # harnesses with quota_group = "claude-max"
max_concurrent = 2
```

- **Global only.** A project `harness.toml` or a `harness_d` drop-in that sets
  any of these keys, or contains `[budget]`, is refused with the key and the
  line: a cloned repository, or a unit dropped in beside the config, does not
  get to set what your agents spend.
- **Per-run caps need a one-shot.** A resident harness's "run" is a process
  that can live for days, so `max_tokens` and `max_cost_usd` there are refused;
  bound it with `max_runs_per_day` and `daily_cost_usd`.
- **`day_starts`** takes the same `TZ=`/`CRON_TZ=` prefix as `schedule` and
  `operating_hours`, then one `HH:MM` (`00:00`–`23:59`).
- **Dollars** may be written as integers (`daily_cost_usd = 20`); `inf` and
  `nan` are refused.
- A `[budget.group.<name>]` that no harness's `quota_group` names still loads.

A bad value fails the load (or a reload, which keeps the previous config) with
the file, line, harness and key:

| Mistake | Error |
| --- | --- |
| `max_tokens` on a resident harness | `"max_tokens" is a per-run cap, and per-run caps apply only to one-shot harnesses (…)` |
| `max_runs_per_day = 0` | `"max_runs_per_day" must be a whole number of at least 1 (got 0)` |
| `max_cost_usd = -1` | `"max_cost_usd" must be a number of dollars greater than 0 (got -1)` |
| `quota_backoff_max = "48h"` | `"quota_backoff_max" must be a duration no longer than 24h, such as "6h" (got "48h")` |
| `quota_backoff = "8h"` alone | `"quota_backoff" must not exceed quota_backoff_max, which defaults to 6h; set quota_backoff_max too (got "8h")` |
| a budget key in a project file | `"max_runs_per_day" is not accepted in a project file (budgets are only accepted in the global config: …)` |
| `day_starts = "TZ=Mars/Olympus 00:00"` | `[budget] "day_starts": invalid "TZ=Mars/Olympus 00:00": unknown time zone "Mars/Olympus"` |
| a price without `input_per_mtok` | `[budget.prices."gpt-5"]: missing required "input_per_mtok" (…)` |

## Agent adapters

The `harness` key is a **required** enum selecting the adapter (ADR-0011,
SPEC-0006): `crush`, `claude-code` ([what it runs](/guides/claude-code)), `codex`, `pi`, `omp`, `generic`, `command`. It has no default —
what a harness runs is the most consequential thing it declares, so a table
that omits the key is a config error rather than an agent nobody asked for:

```
harness "web": missing required key "harness" (want one of: crush, claude-code,
codex, pi, omp, generic, command — use "command" with argv = ["…"] for an arbitrary
program)
```
 The
adapter owns both the tool-specific behaviour (trajectory discovery) and the
executable a long-running harness runs; it also synthesizes the CLI-specific
argv for prompt one-shots. `generic` means "none of the above" — its executable is `sh`, so an
arbitrary command is expressed as `args = ["-c", "<command line>"]`, and it
reports no native trajectory (scrollback-only). Note that `args` are handed to
`sh` itself: a bare `args = ["/usr/local/bin/thing"]` asks sh to *interpret*
that file as a shell script, which fails on a compiled binary.

`generic` takes **no prompt**. It runs `sh` and has no prompt synthesis, so a
`generic` harness that sets `prompt` or `prompt_file` fails to load, in the
global config, a `harness_d` drop-in or a project file, and `project up`, a
scratchpad and the edit form refuse it too:

```
harness "triage": "generic" runs sh and has no prompt synthesis, so it takes no
"prompt"; use harness = "crush"|"claude-code"|"codex"|"pi"|"omp" for a prompt one-shot, or
harness = "command" with argv to run another program without a shell
```

(It used to run `crush run <prompt>` instead, whether or not you had Crush.)

### The `command` kind

`harness = "command"` runs a program you name, directly. Its `argv` is the
whole process: `argv[0]` is the executable and every later element is one
argument, handed to it **byte for byte**. Nothing runs a shell and nothing
builds a command string, so an element holding spaces, `$(…)`, `;` or quotes
is exactly one argument with exactly that text
([ADR-0023](/decisions/adr-0023-command-one-shots-and-templating),
SPEC-0017 REQ-2).

```toml
[harness.report-server]
harness = "command"
argv = ["/usr/local/bin/report", "--listen", ":8080", "--title", "Nightly report"]
workdir = "~/src/report"
enabled = true
```

- `argv` is required and must be non-empty. `argv[0]` must not be blank.
- A bare `argv[0]` (`"report"`) is looked up on `PATH`, like every other
  harness's executable. An absolute one is used as is. A relative one with a
  `/` (`"./bin/report"`) resolves against the harness's `workdir`. Without a
  `workdir` it resolves against the daemon's own working directory, which
  depends on how the daemon was started — so set `workdir`, or use an
  absolute path.
- Elements are not expanded: `{workdir}` and `~` in `argv` stay literal.
  `argv[0]` can never be a `{{…}}` placeholder, so nothing substituted at run
  time can choose what runs. The other elements may be templates; see
  [Argv templates](#argv-templates).
- `args` is rejected on a `command` harness (put everything in `argv`), and
  `argv` is rejected on every other kind.
- `auto_accept`, `max_turns` and `quiet` are rejected: the harness owns its
  argv, so put the program's own flags there. `model` is accepted only when
  an `argv` element references `{{model}}`; otherwise it does nothing and is
  rejected.
- With no `schedule` and no `triggers` it is a **resident** harness. It takes
  `enabled`, `restart`, `restart_delay` and `operating_hours` with the same
  defaults as `generic` (`restart = "always"`).
- With `schedule` or `triggers` it is a **one-shot**, and needs no prompt: the
  argv is the whole job. Each firing produces a run record, a per-run log,
  `timeout`, `on_overlap` and `keep_runs`, exactly as for an agent one-shot,
  and it defaults to `restart = "no"`. Every other one-shot exclusion still
  applies (`enabled = true`, `restart = "always"`, `operating_hours` with a
  `schedule`, and so on).
- `prompt` and `prompt_file` are refused for now: nothing delivers a prompt to
  a command harness's argv yet (issue #500).
- Like `generic`, it reports no native trajectory (scrollback only), unless it
  binds one with `transcripts` (below).
- It works in a project `harness.toml`, through `harness up`, and in the TUI
  edit form, where `argv` is edited as the same TOML array. `harness describe`
  shows the kind and the argv exactly as written, templates included, never a
  rendering.

```toml
[harness.nightly-report]
harness = "command"
argv = ["/usr/local/bin/report", "--run", "{{run.id}}", "--date", "{{run.date}}"]
schedule = "CRON_TZ=Europe/Berlin 0 6 * * *"
workdir = "~/src/report"
```

Any agent CLI not listed above runs this way: `harness = "command"`,
`argv = ["…", …]`, resident, on a `schedule`, or on `triggers`. Pi and OMP
have adapters of their own (below).

#### Argv templates

An `argv` element after `argv[0]` may contain placeholders. Each element is
rendered **once per spawn** into **exactly one argument**: nothing is split,
trimmed, globbed or re-quoted, and an element that renders empty is passed as
an empty argument, so the program always receives as many arguments as `argv`
has elements (SPEC-0017 REQ-6, REQ-11).

| Form | Meaning |
|------|---------|
| `{{path}}` | a required value: if the run does not have it, nothing runs |
| `{{path?}}` | an optional value: empty when the run does not have it |
| `{{literal_open}}` | the two characters `{{` |

A lone `}}` is literal text. There are no functions, filters, pipelines,
conditionals or loops; `{{printf …}}` is a config error.

`argv` may reference these paths, and nothing else:

| Path | Value | Present when |
|------|-------|--------------|
| `harness.name` | the harness's name | always |
| `harness.workdir` | the expanded working directory | always |
| `model` | the harness's `model` | `model` is set |
| `run.id` | the run's ID | the harness has `schedule` or `triggers` |
| `run.trigger` | `schedule`, `catch_up`, `manual`, `channel` or `webhook` | the harness has `schedule` or `triggers` |
| `run.source` | the trigger source, e.g. `webhook.ci` | a source caused the run |
| `run.started_at` | the run's start, RFC 3339 UTC | always |
| `run.date` | `YYYY-MM-DD` in the schedule's `CRON_TZ`/`TZ` zone, else the daemon's local zone | always |

Every template is checked when the config loads, and each failure names the
element, line and column:

- a malformed placeholder, or an unknown path;
- event text in argv in **any** form (`{{event.title}}`, `{{untrusted
  event.body}}`, …): text an outside party wrote never becomes an argument.
  Give the program `$HARNESS_EVENT_FILE` to read instead;
- `event.*` paths and `{{prompt}}`, which are not available in argv yet;
- a required `{{run.id}}`, `{{run.trigger}}` or `{{run.source}}` on a harness
  with neither `schedule` nor `triggers` (it has no run records, so it could
  never render);
- a required `{{run.source}}` on a harness with a `schedule` (a clock firing
  has no source, so every scheduled run would be skipped). Write
  `{{run.source?}}`;
- a required `{{model}}` with no `model` set.

If a required value is still absent when a run spawns — a manual run of a
triggered harness has no `run.source`, say — nothing is exec'd. The run is
recorded `skipped` with reason `template_unresolved` and the missing path's
name, which `harness runs` prints under the table and `--json` carries as
`reason` and `missing_path`. A start with no run record fails instead, with a
`start failed` line naming the path in the harness log. Either way
`harness_template_render_failures_total` counts it.

Rendered values are never written anywhere: not to `state.json`, not to run
records, not to protocol frames, not to logs. They exist only in the child's
argv, which, like any argv, other local users can read with `ps`. Do not
template secrets into it.
Pi and OMP have adapters of their own (below). Any other agent CLI runs this
way, as a resident harness, with `transcripts` if it writes one of the formats
Harness reads.

#### Binding transcripts

A `command` harness that runs an agent CLI by hand can declare whose
transcripts it writes, so the daemon discovers its sessions, correlates them
to its runs, attributes its tool calls and counts them in the model-call
metrics exactly as it does for that adapter's own harnesses (SPEC-0017 REQ-4):

```toml
[harness.claude-by-hand]
harness = "command"
argv = ["claude", "--remote-control", "--continue"]
transcripts = "claude-code"
workdir = "~/src/app"
enabled = true
```

- The value is one of `claude-code`, `crush`, `codex`, `pi` or `omp`; anything
  else fails to load, listing those.
- It is accepted on `command` only. An adapter kind already binds its own
  transcripts, so `transcripts` on `crush` (even `transcripts = "crush"`) is a
  config error.
- It changes what is observed, never what runs: the argv is exec'd exactly as
  written. Store relocation follows the named adapter's rules, read from the
  harness's `env_file` (`CLAUDE_CONFIG_DIR`, `CRUSH_GLOBAL_DATA`,
  `CODEX_HOME`, `PI_CODING_AGENT_DIR`), and crush's `--data-dir` in `argv` is
  not read.
- It is the fallback when an adapter's flags drift from the CLI: run the CLI
  with the flags it now takes, and keep the observation.
- `harness describe` shows it next to the argv.

### Pi and OMP

`pi` runs the [Pi coding agent](https://github.com/badlogic/pi-mono) and `omp`
runs OMP ([oh-my-pi](https://github.com/can1357/oh-my-pi)), a Pi fork. They are
ordinary adapters: a resident harness runs `pi` or `omp` with `args` appended,
and a prompt one-shot runs the print mode in the table above, with `model`
passed as `--model` in the CLI's `provider/id` form. `auto_accept` and
`max_turns` are accepted and add nothing, since neither CLI prompts for tool
permission or has a turn budget.

```toml
[harness.omp-review]
harness = "omp"
model = "openrouter/z-ai/glm-5.3-flash"
prompt = "review the open pull requests"
schedule = "0 7 * * 1-5"
workdir = "~/src/app"
```

The flags were checked against the source of Pi v0.87.1 and OMP v18.3.0.

**Observation.** Both are observed. A `pi` or `omp` harness's sessions are read
from `$PI_CODING_AGENT_DIR/sessions`, or `~/.pi/agent/sessions` /
`~/.omp/agent/sessions` when the variable is unset, resolved from the harness's
own `env_file`. They are attributed to it and counted in the model-call
metrics. OMP's session files open with a fixed-width title line before the
session header; the session reader accepts it, and labels OMP sessions `omp`
so an `omp` harness claims exactly its own.

```toml
[harness.my-agent]
harness = "claude-code"
```

## Model routing and provider failover

When you pin a harness to a single model on a single provider, a quota wall or
provider outage stops every harness sharing it — simultaneously. The restart
policy does not help: the process is healthy, and the API is refusing every
call.

### Where it shows up

Not in `harness list` — a harness whose provider has hit a quota limit shows
`running`. The failure is visible in:

- `harness logs <name>` — repeated provider errors in the output stream
- The provider's dashboard — quota exhaustion or rate limits

Supervision cannot see upstream refusals; it only knows whether the process
restarted.

### Mitigation: an OpenAI-compatible gateway

A gateway in front of your providers (LiteLLM is one example) lets multiple
deployments share a single `model_name`, with failover and retries between them:

```yaml
# Example: LiteLLM proxy config
model_list:
  - model_name: glm-5.3-balanced         # deployment 1
    litellm_params:
      model: openai/glm-5.3
      api_base: https://provider-a.example.com/v1
      api_key: os.environ/A_KEY

  - model_name: glm-5.3-balanced         # deployment 2
    litellm_params:
      model: openai/glm-5.3
      api_base: https://provider-b.example.com/v1
      api_key: os.environ/B_KEY

router_settings:
  routing_strategy: simple-shuffle
  num_retries: 2
  allowed_fails: 2
  cooldown_time: 300  # re-probe after 5 minutes
  fallbacks:
    - glm-5.3-balanced: ["<cheap-tier-model>"]
```

The harness pins `model = "litellm/glm-5.3-balanced"` and knows nothing about
providers. Failover happens inside each request, and `cooldown_time` automatically
retries the benched provider after its quota resets.

### Per-request vs per-worker failover

Running one worker per provider is **not** equivalent:

- Per-worker failover burns the restart budget on each dead worker until it
  hits the `failed` state (terminal, needs a human).
- Capacity halves while one worker recovers.
- A gateway retries the sibling deployment inside the same request and keeps
  every worker productive.

### The trade

A gateway is a dependency on the hot path. Failing over from a
subscription-metered provider to a pay-per-token one **removes a fail-closed
spend cap**. That is the right trade for an always-on agent and the wrong one
for an unattended cron job that nobody is watching.

### Cost and observability

One gateway also means one set of spend and latency metrics instead of
per-provider guesswork — useful even when quota exhaustion is not your primary
risk.

## Trajectory harvesting & facade scope

:::note Reserved, not yet active

The daemon does not run the MCP facade (ADR-0010) today. These keys are
validated and kept in config — the TUI edit form round-trips them — but setting
them changes nothing at runtime yet. They are **not** the telemetry opt-in:
exporting to a collector is a different audience, gated by `export_telemetry`
(see [Telemetry export](#telemetry-export-telemetry)).

:::

- `harvest_trajectory = true` (default `false`) is the opt-in for exposing this
  harness's session transcripts read-only through the planned MCP facade
  (`list_trajectories` / `get_trajectory`). Opt-in because a transcript may
  contain secrets the harnessed program printed itself (ADR-0008).
- `mcp_allow` (default `["read"]`) lists the operations this harness will be
  permitted to invoke through that facade; `"write"` would permit
  `harness_start/stop/restart`. **Global config only** — project files already
  reject the key, so a cloned repository cannot grant itself write authority
  over the fleet.

## Daemon settings (`[daemon]`)

```toml
[daemon]
watch_config = true                      # auto-reload on config file changes (default true)
scrollback_bytes = "1MiB"                # per-harness scrollback ring storage (64KiB–1GiB)
scrollback   = 10000                     # and at most this many lines of it
log_level    = "info"                    # debug, info, warn, error
log_file     = "/var/log/harness.log"    # absent = stderr
socket       = "/run/harness/harness.sock"  # absent = $XDG_RUNTIME_DIR/harness.sock
compress_logs = true                     # zstd-compress sealed logs (default true)
memory_limit = "2GiB"                    # Go soft memory limit; absent = GOMEMLIMIT or off
pprof_addr   = "127.0.0.1:6060"          # net/http/pprof, loopback only; absent = off
```

Every key is optional. Apart from `watch_config`, each has a matching flag and
`HARNESS_*` variable (see [Environment variables](#environment-variables)), and
either one beats the file. Setting them here is how you tune a daemon you do not
launch yourself — a Homebrew `brew services` daemon, a launchd agent, a
container entrypoint — without editing the service definition.

- `scrollback_bytes` is the memory each running harness's scrollback ring may
  use: its recent raw output, which an attach replays after the screen
  snapshot. It is the lever for daemon memory use. Write a size (`"512KiB"`,
  `"4MiB"`, `"1GiB"`; every unit is 1024-based) or a whole number of bytes. It
  must be between 64 KiB and 1 GiB. The default, 1 MiB, is about 13,000 lines
  of ordinary terminal output. Raising it makes every attach, and every
  dashboard preview, replay more before going live.
- `scrollback` must be at least 1. It caps the lines the ring keeps, whatever
  their size; the byte budget usually binds first.
- A line longer than the per-line cap (64 KiB, or a quarter of
  `scrollback_bytes` if that is smaller) keeps only its head, followed by
  `…[harness: truncated N bytes]`, where N counts what was cut. Only the replay
  is truncated: live output and `harness logs` are not.
- In-TUI scroll and search read the durable log, not this ring, so neither
  setting changes how far back they reach.
- `socket` and `log_file` must be absolute paths; `~` is not expanded. The CLI
  reads `socket` from this file too, so `harness ls` finds a daemon on a
  non-default socket without a `--socket` flag.
- `compress_logs` compresses a log once nothing will write to it again: each
  rotated backup of a harness's log, and each closed run's log and raw stream.
  See [Supervision → Logs on disk](./supervision#logs-on-disk). Set it to
  `false` to keep every log plain; files already compressed stay readable.
- `scrollback_bytes`, `scrollback`, `log_level`, `log_file`, `socket`,
  `compress_logs`, `memory_limit` and `pprof_addr` are read when the daemon
  starts. Changing them needs a daemon restart, not a reload. Until you
  restart, a changed `socket` points the CLI at a socket the running daemon is
  not on.
- `harness doctor` shows which source supplied each value.

### Memory limit and profiler

Both are off by default. They are guardrails for the daemon's own memory, not
for the agents it runs.

`memory_limit` sets the Go runtime's **soft** memory limit
(`--memory-limit`, `HARNESS_MEMORY_LIMIT`). As the heap nears the limit, the GC
runs harder so the daemon stays under it, instead of growing to about twice its
live heap, which is the default (`GOGC=100`).

- Write a size, such as `"2GiB"` or `"1536MiB"` (see
  [sizes](#environment-variables); the units are 1024-based, as in systemd's
  `MemoryMax=`). A bare number, or a TOML integer, is **bytes**, so
  `memory_limit = 2048` is 2 KiB and the daemon warns. `"0"` or `0` means no
  limit.
- It cannot free memory the daemon is still holding. A leak still grows, only
  with less headroom on top of it. Set below the live heap, it keeps the GC
  running almost continuously. Keep the hard cap in the init system (systemd
  `MemoryMax=`, a container limit). That cap usually covers every agent the
  daemon spawns as well, so size `memory_limit` for the daemon alone, well under
  it.
- `GOMEMLIMIT` in the daemon's environment is honoured when none of
  `--memory-limit`, `HARNESS_MEMORY_LIMIT` and `memory_limit` is set. Any of
  them overrides it, and an explicit `0` removes it. Prefer `memory_limit`:
  every harness the daemon spawns inherits `GOMEMLIMIT`, and Go agents read it
  too.
- The daemon logs the limit in effect and where it came from at startup
  (`memory limit limit=2GiB source=file`), and `/metrics` reports it as
  `go_gc_gomemlimit_bytes`.

`pprof_addr` serves Go's profiler (`--pprof-addr`, `HARNESS_PPROF_ADDR`) at
`http://<addr>/debug/pprof/`.

- It binds **loopback only**: `127.0.0.1`, `::1` or `localhost`. Any other
  address fails the config load (and any reload) with its line number, or stops
  `harness daemon` at startup when it comes from the flag or the variable.
  No token unlocks a remote bind, as `metrics_token_file` does for `/metrics`.
  Heap profiles and goroutine dumps describe the daemon's internals, so reach
  them from another host through a tunnel:
  `ssh -L 6060:127.0.0.1:6060 host`.
- A port that is already taken is logged, and the daemon runs without the
  profiler.

See [Diagnosing daemon memory](./production-observability#diagnosing-daemon-memory)
for how to use both.

`otel_endpoint` has been **removed**: it was accepted but never exported
anything. A config that still sets it fails to load, with an error pointing
here. To export agent telemetry, add a [`[telemetry]`](#telemetry-export-telemetry)
table with `traces = true` (and/or `logs = true`) and set `endpoint` there or
`OTEL_EXPORTER_OTLP_ENDPOINT` in the daemon's environment. It is deliberately
not an alias: an inert line in an old config must not start publishing agent
transcripts on upgrade.

## Telemetry export (`[telemetry]`)

The daemon can export what its supervised agents do — every tool call and every
mark (user message, compaction, subagent launch, provider error) — as **OTLP
logs**, **OTLP traces**, and a **local JSONL events file**. The
[production observability guide](./production-observability) shows collector,
Grafana (Loki/Tempo), Honeycomb and Vector/Fluent Bit setups; this is the key
reference (ADR-0022, SPEC-0015).

Nothing is exported without **two** consents: a destination in this table, and a
harness that opted in (`export_telemetry = true` on the harness, or
`export_all = true` here). Environment variables alone never enable export — an
`OTEL_EXPORTER_OTLP_ENDPOINT` inherited from your shell is only mentioned in the
daemon log. With no destination configured the daemon opens no socket, creates
no file and subscribes to nothing.

```toml
[telemetry]
logs   = true                     # OTLP logs: one record per tool call or mark
traces = true                     # OTLP traces: one trace per agent session
events_file = "~/.local/state/harness/events.jsonl"  # local JSONL; empty = off

export_all   = false              # every harness without export_telemetry contributes
omit_prompts = false              # replace prompt text with "[prompt omitted]"

endpoint    = "http://127.0.0.1:4318"   # OTLP/HTTP base; /v1/logs and /v1/traces are appended
env_file    = "/etc/harness/otel.env"   # OTEL_EXPORTER_OTLP_* (headers/credentials go here)
compression = "none"              # "none" | "gzip"
timeout     = "10s"               # per request

queue_size       = 2048           # per signal (records, spans or lines); oldest dropped when full
batch_size       = 512            # max per request; must not exceed queue_size
batch_interval   = "5s"           # flush a partial batch after this long
idle_flush       = "5m"           # export a session's trace after this long without items
shutdown_timeout = "5s"           # total budget for the shutdown flush
events_file_max_mb = 100          # rotate the events file (by rename) at this size
events_file_keep   = 5            # rotated files kept: events.jsonl.1 … .5
```

| Key | Default | Meaning |
|-----|---------|---------|
| `logs` | `false` | enable the OTLP logs signal |
| `traces` | `false` | enable the OTLP traces signal |
| `events_file` | `""` (off) | path of the JSONL sink; `~` expands, relative paths resolve against the config file's directory. File `0600`, created directories `0700` |
| `export_all` | `false` | harnesses with no `export_telemetry` contribute |
| `omit_prompts` | `false` | replace user-message text with `[prompt omitted]` and drop the session title |
| `endpoint` | `""` | absolute `http`/`https` base URL with a host and **no userinfo** |
| `env_file` | `""` | file supplying the `OTEL_EXPORTER_OTLP_*` variables below; only those keys are read, and none are put into the daemon's environment, so supervised harnesses never inherit the credential. Missing or unreadable fails startup when an OTLP signal is on; group/world-readable is a warning |
| `compression` | `"none"` | `none` or `gzip` |
| `timeout` | `"10s"` | per-request timeout |
| `queue_size` | `2048` | per-signal bounded queue; at most `16384` |
| `batch_size` | `512` | units per request (or per events-file write); at most `4096` and at most `queue_size` |
| `batch_interval` | `"5s"` | partial-batch flush interval |
| `idle_flush` | `"5m"` | trace export after a quiet session; at least `batch_interval` |
| `shutdown_timeout` | `"5s"` | shutdown flush budget |
| `events_file_max_mb` | `100` | rotation size; at most `10240` |
| `events_file_keep` | `5` | rotated files kept; at most `100` |

Durations must be positive; integers must be positive and under their ceiling —
the queues are allocated at startup, so the ceiling is what stops a typo from
exhausting memory before the first item arrives. A **`headers` key is
rejected** — OTLP headers carry credentials, and `harness.toml` carries none
(ADR-0008). Put them in `OTEL_EXPORTER_OTLP_HEADERS`, preferably via `env_file`.

**Scope.** The table belongs to the daemon's global `harness.toml` only: a
project `harness.toml` and a `harness_d` drop-in both reject it. A project file
may set `export_telemetry = false` on its harnesses but not `true` — a cloned
repository does not get to publish its transcripts to your collector.

**Changes need a restart.** The exporters hold queues, connections and an open
file, so an edited `[telemetry]` table takes effect on the next daemon start; a
reload logs a warning saying so. Per-harness `export_telemetry` changes apply on
reload, from the next item.

### The OpenTelemetry environment

The standard OTLP exporter variables are honoured, from the daemon's process
environment first and then from `env_file`:

| Variable | Meaning |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | base URL; `/v1/logs` and `/v1/traces` are appended |
| `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | full signal URL, used as-is |
| `OTEL_EXPORTER_OTLP_HEADERS` (+ `_LOGS_` / `_TRACES_`) | `name=value,name2=value2`, values percent-decoded; merged, signal-specific wins per name |
| `OTEL_EXPORTER_OTLP_COMPRESSION` (+ `_LOGS_` / `_TRACES_`) | `none` or `gzip` |
| `OTEL_EXPORTER_OTLP_TIMEOUT` (+ `_LOGS_` / `_TRACES_`) | milliseconds |
| `OTEL_RESOURCE_ATTRIBUTES` | extra resource attributes (cannot override `service.name`) |
| `OTEL_SDK_DISABLED=true` | turns both OTLP signals off |

Precedence, per setting and per signal: **signal-specific variable > generic
variable > `[telemetry]` > default**. An exported-but-empty variable counts as
unset. `OTEL_EXPORTER_OTLP_PROTOCOL` set to anything but `http/json` is ignored
with a warning: Harness speaks only OTLP/HTTP JSON. An enabled OTLP signal with
no endpoint anywhere refuses to start.

At startup the daemon logs one line per signal with the resolved endpoint, the
source of each setting (`env`, `env_file`, `file`, `default`), and each header
as its name plus a SHA-256 fingerprint — never a value. `harness doctor` reports
the same under a `TELEMETRY` section (`.telemetry` in `--json`), resolved in the
shell you run it from: a daemon started by systemd with its own
`EnvironmentFile`, or as a user who can read an `env_file` you cannot, may
resolve differently, and the daemon's startup line is the authority.

### What is exported

Every string is redacted with the same rules as `harness logs` before it is
queued, and capped (a record body at 4 KiB, a span name at 256 bytes, other
attributes at 1 KiB, at most 32 targets). Tool output and file contents are
never read, so never exported. A failed tool call is `WARN`; a provider or model
error mark is `ERROR`, so an alert on `ERROR` means a turn failed. Log records
and spans for the same item share `traceId`/`spanId`, and every item carries
`agent.item.id` as a deduplication key.

A dead collector costs counted, dropped telemetry and nothing else: failed
requests retry with backoff (429/502/503/504 and network errors, honouring
`Retry-After`) for up to five minutes per batch, queues drop their oldest units
when full, and failures reach the daemon log at most once a minute per signal.

## Merge train (`[mergetrain]`)

The merge train lands approved pull requests one at a time. For each one it
builds `train/<pr>` (`main` plus a squash of the PR), waits for CI on that exact
commit, squash-merges, and then verifies that the tree that landed is the tree
that was tested. It is off unless you turn it on, and even then it starts in
`report` mode, which builds and tests trains but writes nothing to any pull
request. See ADR-0032 and SPEC-0025.

```toml
[mergetrain]
enabled = true                               # default false
mode = "report"                              # or "merge"; default "report"
repos = ["stump.wtf/harness"]                # required when enabled
base_branch = "main"                         # default "main"
poll_interval = "60s"                        # default 60s, minimum 5s
ci_timeout = "30m"                           # default 30m
forge_base_url = "https://gitea.stump.rocks" # required when enabled
forge_token_env = "HARNESS_MERGETRAIN_TOKEN" # the variable's NAME, required when enabled
```

- **The token is never in this file.** `forge_token_env` names an environment
  variable in the daemon's environment, and a `token` key is refused. The
  daemon refuses to start if the train is enabled and that variable is empty.
- **Global only.** A project `harness.toml` or a `harness_d` drop-in that
  contains `[mergetrain]` is refused.
- **One train per repo.** Each repo's driver holds a lock under
  `$XDG_STATE_HOME/harness/mergetrain/`, so a second daemon on the same host
  skips that repo rather than racing it.
- **Restart to apply.** A change to `[mergetrain]` takes effect at the next
  daemon restart.
- **`batch` is accepted, not yet built.** `batch = N` (a whole number, at
  least 1; default 1) is SPEC-0025 REQ-17's PRs-per-train size. The train
  still carries one PR per train, and the daemon logs a warning at start when
  `batch` is above 1. See [Merge train](./merge-train#batching-specified-not-yet-in-the-binary).

## Notifications (`[notify]`)

One program the daemon runs when a harness needs a person: it gave up into
`failed`, is crash-looping, was stopped by the loop guard, or had its session
rotated. See [Notifications](./notify) for the events, the payload and a
minimal hook.

```toml
[notify]
command  = ["/home/me/.config/harness/notify.sh"]  # required; argv[0] absolute, no shell
events   = ["failed", "flapping", "loop_stopped", "session_rotated", "recovered"]  # default
timeout  = "15s"                                    # default 15s, 1s–5m
cooldown = "15m"                                    # default 15m, 0s–24h; per harness and event
```

- **Validated at load.** A relative `command[0]`, an unknown event, an empty
  `events` list or an out-of-range duration refuses the config with the
  offending key's line. `run_failed` is the one event left out of the default
  set.
- **Global only.** A project `harness.toml` or a `harness_d` drop-in that
  contains `[notify]` is refused: a cloned repository does not get to choose a
  program the daemon runs.
- **Reload applies it.** Adding, changing or removing the table takes effect at
  the next `harness reload` (or config-watch reload), no restart needed.

## Restart policy

The `restart` key mirrors Docker Compose's directive and controls whether a
harness is restarted when it exits:

| Value | Behavior |
|-------|----------|
| `"no"` | never restart automatically |
| `"always"` | always restart (this is the default when the key is omitted) |
| `"unless-stopped"` | always restart, unless explicitly stopped |
| `"on-failure"` | restart only on a non-zero exit code |

Retry limits under a crash loop come from the daemon's crash-loop policy, which
escalates backoff — see [Supervision](./supervision).

## Profiles

Profiles are named groups of harnesses that switch together

```toml
[profile.default]
description = "everyday set"
harnesses = ["heartbeat"]
autostart = true

[profile.full]
description = "all agents"
harnesses = ["heartbeat", "my-agent"]
```

- Only one profile is active at a time.
- `autostart = true` brings that profile's harnesses up when the daemon starts
  (and on the active profile). The active profile is what `harness list` flags
  with `*`.
- Switch with `harness use-profile <name>`.

Profiles are a global concern — they are not allowed in a project file (see
[Projects](./projects)).

## Drop-in harness files (`harness_d`)

Instead of editing one growing `harness.toml`, point at a directory and add or
remove a harness one file at a time:

```toml
[server]
harness_d = "~/.config/harness/harness.d"
```

Then drop files like `~/.config/harness/harness.d/backup-daily.toml`:

```toml
[harness.backup-daily]
harness = "crush"
prompt = "run the daily backup"
schedule = "0 2 * * *"
```

- Only `*.toml` files are read; anything else in the directory is ignored.
- Files are merged in lexicographic order, so a numeric prefix (`10-`, `20-`)
  pins the order if you care about it.
- A drop-in may contain **`[harness.*]` tables only**. `[server]`,
  `[profile.*]`, `[daemon]`, and bare `[name]` tables are rejected with the
  offending file and line.
- Duplicate harness names — between two drop-ins, or with the main file — are
  rejected rather than silently overwritten.
- A leading `~` expands to your home directory, and a relative path resolves
  against the directory holding `harness.toml` (not the daemon's working
  directory, which under systemd is not the same thing).
- The directory must exist. A missing `harness_d` fails the config load rather
  than being treated as empty, so a typo cannot silently drop every drop-in.
- `[profile.*]` tables in the main file may reference drop-in harnesses.

Drop-in files are **not** watched for changes. Auto-reload (`watch_config`)
watches `harness.toml` only, so after adding or removing a drop-in run
`harness reload` (or touch the main config).

## Remote access (`[server]`)

The optional SSH front door exposes the same dashboard over the network:

```toml
[server]
enabled = true
listen = "0.0.0.0:23234"
authorized_keys_file = "~/.ssh/harness_authorized_keys"
```

Only listed SSH public keys can connect — there is **no password auth path**.
Per-key read-only scoping lets a key attach without typing:

```toml
[[server.key]]
key = "ssh-ed25519 AAAA…"
read_only = true
```

See [Remote access](./remote) for setup and security notes.

### Metrics listener

The daemon serves Prometheus metrics on `127.0.0.1:10229` by default,
independent of `enabled` above. The two keys below live in `[server]`:

```toml
[server]
# Changing either key needs a daemon restart: `harness reload` re-reads
# harness definitions and never rebinds a listener.
metrics_listen = "0.0.0.0:10229"                          # default 127.0.0.1:10229; "off" disables
metrics_token_file = "~/.config/harness/metrics.token"    # required off loopback
```

| Key | Default | Meaning |
|---|---|---|
| `metrics_listen` | `127.0.0.1:10229` | `host:port` for `GET /metrics`, or `"off"`. A non-loopback address without a token makes the daemon refuse to start. |
| `metrics_token_file` | none | A file holding the bearer token scrapers must send. harness.toml holds its path, never the token (ADR-0008). |

See [Metrics](./metrics) for the series, alert rules and scrape config.

### Webhook listener

`[webhook.*]` sources are served on an HTTP listener of their own, separate
from the SSH front door and the metrics listener. It is **off** unless an
address names it; declaring a `[webhook.*]` table does not open a port.

```toml
[server]
webhook_listen = "127.0.0.1:9080"   # or --webhook-listen / HARNESS_WEBHOOK_LISTEN
# Both or neither; with both set the listener serves HTTPS only.
# webhook_tls_cert_file = "/etc/harness/webhook.crt"
# webhook_tls_key_file  = "/etc/harness/webhook.key"

[webhook.ci]
verify = "bearer"
env_file = "~/.config/harness/triggers.env"   # CI_HOOK_TOKEN=...
secret = "${CI_HOOK_TOKEN}"

[harness.on-ci]
harness = "claude-code"
prompt_file = "~/.config/harness/prompts/on-ci.md"
triggers = ["webhook.ci"]
```

```sh
curl -X POST -H "Authorization: Bearer $CI_HOOK_TOKEN" \
     -H 'Content-Type: application/json' -d '{"status":"failed"}' \
     http://127.0.0.1:9080/hooks/ci
# 202 {"webhook":"ci","event_id":"wh-…","decision":"fired",
#      "firings":[{"harness":"on-ci","decision":"started","run_id":4}]}
```

The listener serves exactly `POST /hooks/<name>` and `GET /healthz` (`ok`).
It never redirects and never serves files.

| Status | When |
|---|---|
| `202` | Verified. `decision` is `fired` with one entry per bound harness (`started`/`skipped` carry `run_id`; `queued` does not; `error` means that harness could not be fired at all — unknown to the supervisor, shut down under a reload, or its firing panicked — and carries no `run_id`, with the detail in the daemon log only), `ignored` when `events` filtered it out, or `duplicate` when its delivery ID already fired (with the first firing's `event_id`). `ignored` and `duplicate` fire nothing and make no run record. The response never waits for a run. |
| `400` | The body could not be read for a reason other than size (for example, the connection dropped mid-body). Body `{"error":"bad_request"}`. |
| `401` | Missing, malformed or wrong credential. Every cause gets the same body; the log names the route and peer, never the value presented. |
| `404` | The name is unknown, disabled, or bound by no harness. All three are byte-identical, so routes cannot be enumerated. |
| `405` | Any method other than `POST` on `/hooks/<name>` (or other than `GET` on `/healthz`). |
| `413` | The body is over `max_body`. Checked before the credential is. |
| `429` | The route is over its `rate_limit`. `Retry-After` says how many seconds until the next token. |
| `503` | 64 deliveries are already in flight. |

Every response carries `Content-Security-Policy`, `X-Frame-Options`,
`X-Content-Type-Options`, `Referrer-Policy` and `Cache-Control: no-store`,
plus `Strict-Transport-Security` when the listener terminates TLS itself.
Slow clients are cut off: 10 s to send headers, 30 s to read the request,
30 s to write the response, 60 s idle, 64 KiB of headers.

After verification, each delivery goes through three filters, in order:

1. **`events`**: an event name not in the list (or no event name at all) is
   `202 ignored`.
2. **De-duplication**: when the scheme has a delivery header (`delivery_header`
   for `bearer`/`hmac-sha256`, the preset's otherwise) and the delivery
   carries one, an ID that already fired on this route in the last 24 hours —
   among the last 1024 kept — is `202 duplicate`. The set is in memory and
   starts empty when the daemon does.
3. **`rate_limit`**: a token bucket of `n` refilling `n` per unit, so `"2/m"`
   allows a burst of two, then one every 30 s. Only a delivery that verified,
   passed `events` and is not a duplicate spends a token, so forged traffic
   cannot use up the real sender's budget. A `429` is not remembered as seen,
   so the sender's retry fires. `"0"` turns the limit off.

A reload keeps a route's bucket and de-duplication set as long as its name and
`rate_limit` are unchanged; changing `rate_limit` starts both afresh.

#### Verification schemes

`verify` picks how a delivery proves it came from whoever holds the secret.
The check runs over the body bytes exactly as they arrived, before anything
parses them. The four presets fix their header names, so a GitHub, Gitea or
GitLab route is three lines:

```toml
[webhook.gh]
verify = "github"
env_file = "~/.config/harness/triggers.env"   # GH_HOOK_SECRET=...
secret = "${GH_HOOK_SECRET}"
```

| `verify` | Credential the sender presents | Event header | Delivery header |
|---|---|---|---|
| `bearer` | `Authorization: Bearer <secret>` | `event_header` | `delivery_header` |
| `hmac-sha256` | hex HMAC-SHA256 of the body, keyed by the secret, in `signature_header` after `signature_prefix` | `event_header` | `delivery_header` |
| `github` | `X-Hub-Signature-256: sha256=<hex HMAC-SHA256>` | `X-GitHub-Event` | `X-GitHub-Delivery` |
| `gitea` | `X-Gitea-Signature: <hex HMAC-SHA256>` | `X-Gitea-Event` | `X-Gitea-Delivery` |
| `gitlab` | `X-Gitlab-Token: <secret>` | `X-Gitlab-Event` | `X-Gitlab-Event-UUID` |
| `standard-webhooks` | `webhook-signature: v1,<base64 HMAC-SHA256>` over `<webhook-id>.<webhook-timestamp>.<body>`, keyed by the decoded secret, with `webhook-timestamp` within 5 minutes of the daemon's clock | none; the body's top-level `type` | `webhook-id` |

For `github` and `gitea`, set the same value as the forge hook's **Secret**;
for `gitlab`, as its **Secret token**. A missing signature header, two of
them, a wrong prefix, anything but 64 hex digits, or a MAC that does not
match is a `401`. The event header becomes the event name `events` matches,
and the delivery header becomes the run's `event_id`. The event file carries
only `Content-Type`, `User-Agent` and those two headers, never the signature
or token.

A `gitlab` token is a shared password, not a signature: it proves the sender
knows the secret, but does not cover the body. Prefer `gitea`- or
`github`-style signing where the sender offers it, and TLS either way.

A `standard-webhooks` route speaks the [Standard Webhooks](https://www.standardwebhooks.com/)
v1 signature — the scheme Switchboard's notify hooks use. The secret must be
`whsec_` followed by the padded standard base64 of 24 to 64 bytes, and the
HMAC is keyed by what that decodes to, never by the string. A delivery passes
when **any** `v1` entry in the space-separated `webhook-signature` list
matches, so a sender mid-rotation can sign with its old and its new secret
side by side. `webhook-timestamp` more than 300 seconds from the daemon's
clock, in either direction, is a `401`; so is a body or `webhook-id` that was
not the one signed. The event name `events` matches is the body's top-level
`type`, and `webhook-id` becomes the run's `event_id`, so re-sending the same
delivery is a `202 duplicate`, not a second firing.

A non-loopback `webhook_listen` without TLS starts with a warning: bearer,
GitLab and Standard Webhooks secrets then cross the network in cleartext.
Bind loopback behind a TLS-terminating proxy, or set both TLS files.

`harness triggers` shows each source's state — `listening`, or `no_listener`
when no address is set — and `harness doctor` flags both a non-loopback bind
without TLS and a bound source no listener serves (see
[CLI → Trigger sources](./cli#trigger-sources)).

Changing `webhook_listen` or the TLS files needs a daemon restart; a reload
logs that a restart is required and keeps serving on the old settings. Route
changes (a source added, disabled, rebound) apply on reload. On shutdown the
listener stops accepting and gives in-flight deliveries 5 seconds.

## Environment variables

Process-level settings — where the socket lives, how loud the log is, whether
the SSH server is on — can come from the environment instead of a flag or this
file. That is what makes Harness deployable as a container or a systemd unit
without baking in a config file.

| Variable | Flag | `harness.toml` key | Type | Default |
|---|---|---|---|---|
| `HARNESS_SOCKET` | `--socket` | `[daemon] socket` | path | `$XDG_RUNTIME_DIR/harness.sock` |
| `HARNESS_CONFIG` | `--config` | — | path | `$XDG_CONFIG_HOME/harness/harness.toml` |
| `HARNESS_JSON` | `--json` | — | bool | `false` |
| `HARNESS_LOG_LEVEL` | `--log-level` | `[daemon] log_level` | `debug`/`info`/`warn`/`error` | `info` |
| `HARNESS_LOG_FILE` | `--log-file` | `[daemon] log_file` | path | stderr |
| `HARNESS_SCROLLBACK` | `--scrollback` | `[daemon] scrollback` | int | 10000 |
| `HARNESS_SCROLLBACK_BYTES` | `--scrollback-bytes` | `[daemon] scrollback_bytes` | size (`4MiB`, bytes) | `1MiB` |
| `HARNESS_SSH` | `--ssh` | `[server] enabled` | bool | `false` |
| `HARNESS_SSH_LISTEN` | `--ssh-listen` | `[server] listen` | `host:port` | unset |
| `HARNESS_WEBHOOK_LISTEN` | `--webhook-listen` | `[server] webhook_listen` | `host:port` | unset (no webhook listener) |
| `HARNESS_WATCH_CONFIG` | — | `[daemon] watch_config` | bool | `true` |
| `HARNESS_COMPRESS_LOGS` | `--compress-logs` | `[daemon] compress_logs` | bool | `true` (zstd-compress sealed logs) |
| `HARNESS_MEMORY_LIMIT` | `--memory-limit` | `[daemon] memory_limit` | size (`2GiB`; `0` = off) | unset (`GOMEMLIMIT`, else off) |
| `HARNESS_PPROF_ADDR` | `--pprof-addr` | `[daemon] pprof_addr` | loopback `host:port` | unset (no profiler) |

`GOMEMLIMIT` is not a Harness variable, but the daemon honours it when no source
above sets a memory limit. See [Memory limit and profiler](#memory-limit-and-profiler).

`config` has no file key because it names the file. `json` has none because it
is an output choice for one invocation: a file default would change what every
script parsing Harness's output receives.

Booleans accept `1`, `0`, `true`, `false`, `yes`, `no`, `on`, `off`.

Sizes accept a whole number of bytes or a number with a unit: `B`, `KiB`,
`MiB`, `GiB`, `TiB`, with `K`/`KB`, `M`/`MB`, `G`/`GB`, `T`/`TB` as the same
1024-based units. Case does not matter.

### Precedence

An explicit flag beats an environment variable, which beats this file, which
beats the compiled default:

```
--socket /tmp/y.sock   >   HARNESS_SOCKET=/tmp/x.sock   >   harness.toml   >   default
```

"Explicit" means you actually typed the flag. Leaving a flag alone does not
suppress an environment variable, so `HARNESS_LOG_LEVEL=debug harness daemon run`
starts at debug even though `--log-level` has a default of `info`.

An exported-but-empty variable is treated as unset, because
`export HARNESS_SOCKET=$SOME_UNSET_VAR` is a shell accident rather than a request
for an empty socket path.

A bad value is a hard failure, never a silent fallback:

```
HARNESS_SCROLLBACK=lots harness daemon run
# HARNESS_SCROLLBACK: invalid value "lots": expected an integer

HARNESS_SCROLLBACK_BYTES=16GiB harness daemon run
# HARNESS_SCROLLBACK_BYTES: must be between 64KiB and 1GiB, got 16GiB
```

### Which source won?

`harness doctor` reports every setting with the source that supplied it, so you
never have to guess:

```bash
harness doctor
```

```
SETTING       SOURCE    VALUE
socket        env       /run/harness.sock
config        default   /home/you/.config/harness/harness.toml
log-level     flag      debug
ssh           file      true
```

`harness doctor --json` carries the same data under `.settings` for scripts.

### Harnesses stay in the file

There is no environment variable that defines a harness or a profile. Those are
collections, and mangling them into variable names is not reliably reversible —
`HARNESS_HARNESS_CLAUDE_SRC_WORKDIR` cannot tell you whether the harness is
`claude-src` or `claude_src`. So:

**A container can run a fully-configured daemon with no `harness.toml` at all,
but it cannot define a harness without one.** A missing file is not an error;
the daemon comes up and reports zero harnesses. A file that exists but does not
parse is still an error.

### Secrets do not go here

No `HARNESS_*` variable takes a token, key, or password. Per-harness secrets
belong in `env_file`, which is read by the harness rather than by Harness
itself. See [Remote access](./remote) for the SSH key handling.

`HARNESS_DETACH_READY_FD` is reserved: it is internal plumbing between
`harness daemon --detach` and the process it forks, not a setting.

### systemd

```ini
[Service]
Environment=HARNESS_SOCKET=/run/harness/harness.sock
Environment=HARNESS_LOG_LEVEL=info
Environment=HARNESS_CONFIG=/etc/harness/harness.toml
ExecStart=/usr/local/bin/harness daemon run
```

## After editing

`harness reload` re-reads the config and reconciles running harnesses without a
daemon restart. `harness doctor` verifies the file parses and the daemon agrees
with it.

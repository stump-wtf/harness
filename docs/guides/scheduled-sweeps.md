---
title: "Scheduled sweeps"
sidebar_position: 4
---

# Scheduled sweeps

A **sweep** is an unattended agent run on a schedule: "every weekday at 14:00,
review open pull requests", "every six hours, check the services and report
anything unhealthy". In Harness a sweep is a harness with a `prompt` and a
`schedule`. The daemon owns the cron. Each firing starts one agent run with your
instruction, waits for it to exit, and records the outcome.

This guide covers the config, then the part that decides whether sweeps are
useful: how to design one so that a run that went wrong never looks like one
that went right.

## A prompt harness

Give a harness a `prompt` (or `prompt_file`) instead of `args`, and Harness
builds the agent's one-shot command for you:

```toml
[harness.pr-sweep]
harness = "claude-code"
prompt_file = "~/.config/harness/prompts/pr-sweep.md"
model = "claude-sonnet-5"
auto_accept = true
max_turns = 40
workdir = "~/sweeps/pr-sweep"
env_file = "~/.config/harness/env/pr-sweep.env"
schedule = "CRON_TZ=UTC 0 14 * * 1-5"
catch_up = true
timeout = "30m"
on_overlap = "skip"
keep_runs = 30
description = "weekday PR review sweep"
```

| Key | What it does |
|-----|--------------|
| `prompt` | The instruction, inline. Fine for one sentence. |
| `prompt_file` | A file holding the instruction. Use this for anything real. A leading `~` expands, and a relative path resolves against `harness.toml`'s directory. The file is read at each run, so editing the prompt needs no reload. A missing or empty file fails the config load. |
| `model` | Which model the agent uses. |
| `auto_accept` | Run without permission prompts: `--dangerously-skip-permissions` for Claude Code, `--yolo` for Crush, `--full-auto` for Codex. Nobody is attached to approve a tool call during a scheduled run, so a sweep usually needs this. **It lets the agent run any tool without asking.** Scope the workdir and credentials accordingly. |
| `max_turns` | A turn budget. Only Claude Code has a flag for it; Crush and Codex ignore the key. |
| `quiet` | Headless output, on by default for prompt harnesses. Only Crush uses it. |

What actually runs, per adapter:

```
claude-code  claude -p [--dangerously-skip-permissions] [--model M] [--max-turns N] --verbose --output-format stream-json PROMPT
crush        crush [--yolo] run [--quiet] [--model M] PROMPT
codex        codex exec [--model M] [--full-auto] PROMPT
```

A prompt harness defaults to `restart = "no"`: a run that finishes is done. The
only other value a scheduled harness accepts is `"on-failure"`.

## `schedule`: cron, pinned to a zone

`schedule` is a standard five-field cron expression, validated when the config
loads, so a typo is a located error instead of a sweep that never fires:

```toml
[harness.nightly-sweep]
harness = "crush"
prompt = "check the backups and report anything unusual"
schedule = "CRON_TZ=UTC 30 3 * * *"
```

```
┌──────── minute (0-59)
│ ┌────── hour (0-23)
│ │ ┌──── day of month (1-31)
│ │ │ ┌── month (1-12)
│ │ │ │ ┌ day of week (0-6, Sunday = 0)
│ │ │ │ │
30 3 * * *      03:30 every day
0 14 * * 1-5    14:00 Monday to Friday
0 */6 * * *     every six hours
*/15 * * * *    every fifteen minutes
@every 2h       every two hours from when the daemon armed it
```

**Prefix every schedule with `CRON_TZ=UTC`.** Without a prefix, the expression
runs in the daemon's local time zone, which surprises you in a few ways:

- **Two machines, two zones, two different "09:00".** A laptop that travels, or a
  server in another region, fires the same config at different real times.
- **Daylight saving moves your sweep.** A local-time `0 2 * * *` drifts an hour
  against everything that runs in UTC (CI, your provider's quota reset, other
  people's cron) twice a year. On the change days it can run just after the
  skipped hour, or once in the repeated hour.
- **Logs disagree.** Run records, forge timestamps and API responses are
  usually UTC. A local-time schedule makes every correlation a mental
  conversion.

`CRON_TZ=Europe/Berlin` (any IANA zone) works too, when a sweep really should
follow a local wall clock.

### A schedule runs on exactly one machine

If you sync `harness.toml` between machines, a scheduled harness in it fires on
**every** machine running a daemon. That means two PR reviews, two Slack
summaries, and twice the tokens, and nothing in either run will tell you. Keep
each schedule on one designated host:

- Put the synced, shared harnesses in `harness.toml`.
- Put a host's scheduled harnesses in a **drop-in directory that is not
  synced** (below).

## `catch_up`: sleep and outages

The daemon checks the wall clock every second instead of setting timers, so it
notices a window it slept through the moment the machine wakes, or at boot
after an outage. A window noticed more than a minute late is **missed**:

- `catch_up = false` (the default) runs nothing. The daemon log gets a
  `scheduled run MISSED` warning naming the harness and the windows, and the run
  history gets a `missed` record. The next window fires normally.
- `catch_up = true` runs **once** on wake or boot, however many windows were
  missed.

Use `catch_up = true` for "at least daily" work, like a morning digest on a
laptop that was closed at 07:00. Leave it `false` for work that is pointless
late, or dangerous to double up.

## `timeout`, `on_overlap`, `keep_runs`

```toml
[harness.hourly-triage]
harness = "crush"
prompt_file = "prompts/hourly-triage.md"
schedule = "CRON_TZ=UTC 0 * * * *"
timeout = "45m"        # default "1h"; "0" disables the limit
on_overlap = "queue"   # default "skip"; also "replace"
keep_runs = 48         # default 20
```

- **`timeout`** stops a run that goes on too long: SIGTERM, then SIGKILL after
  a grace period. The run is recorded as `timed_out`. Set it a bit above your
  longest healthy run. An agent that is stuck in a loop is also spending
  tokens.
- **`on_overlap`** decides what happens when the next window fires while a run
  is still going:
  - `skip` records a `skipped` entry and does nothing.
  - `queue` runs once the current run ends, holding at most one queued run.
  - `replace` stops the current run and starts fresh.
- **`keep_runs`** bounds history. Older run records and their log files are
  pruned together.

All three, like `catch_up`, require `schedule`. A scheduled harness cannot also
be `enabled = true` or belong to a profile. The schedule is what starts it.

## Drop-in files: one harness per file

Instead of growing one big `harness.toml`, point the daemon at a directory:

```toml
[server]
harness_d = "harness.d"
```

The path is relative to `harness.toml`'s directory, so this is
`~/.config/harness/harness.d/`. That directory must exist; a missing one fails
the load. Each `*.toml` inside holds only `[harness.*]` tables:

```toml
# ~/.config/harness/harness.d/pr-sweep.toml
[harness.pr-sweep]
harness = "claude-code"
prompt_file = "~/.config/harness/prompts/pr-sweep.md"
auto_accept = true
workdir = "~/sweeps/pr-sweep"
schedule = "CRON_TZ=UTC 0 14 * * 1-5"
timeout = "30m"
```

Files load in lexical order, and a name defined twice is an error, not a silent
override. This is also the clean way to keep host-specific schedules out of a
synced config.

## Reloading

- **`harness.toml`** is watched. Saving it reloads the daemon, which re-arms
  every schedule. The daemon log shows `config auto-reloaded`.
- **Drop-in files are not watched.** After adding, editing or removing one, run:

```sh
harness reload
```

A config that fails to parse leaves the last good config running and reports
the error with its file and line. `harness doctor` shows the same error.

## Checking the schedule

```sh
$ harness list
NAME      STATE         ENABLED    RESTARTS   DESCRIPTION
pr-sweep  ⏱ idle        no         0          weekday PR review sweep · in 3h12m
```

A clock glyph marks a scheduled harness, and its next firing is appended to the
description. `ENABLED no` is normal: the schedule starts it, not autostart.
`harness describe pr-sweep` adds the cron spec and the absolute time of the next
run.

`harness jobs` shows only the scheduled harnesses, with the schedule in words,
the next window, the latest run, and the current streak of failures:

```sh
$ harness jobs
NAME           STATE    SCHEDULE         NEXT        LAST RUN             FAILS
nightly-sweep  ⏱ idle   daily 03:00 UTC  in 7h56m    #14 success 21h ago  0
triage         ⏱ idle   every 6h         in 4h56m    #31 failed 1h ago    2
```

`FAILS` counts consecutive `failed` and `timed_out` runs back to the last
success. A number that keeps climbing is the first thing to look at.

### Run it now

To run a sweep **right now**, for example to test a prompt change, trigger it:

```sh
$ harness trigger pr-sweep --wait
pr-sweep: started run #13
2026/09/11 21:04:01 INFO run started run_id=13 trigger=manual
…the run's log streams here…
2026/09/11 21:10:42 INFO run finished run_id=13 outcome=success exit_code=0
pr-sweep: run #13 success (exit 0) after 6m41s
```

A triggered run goes through the same path as a scheduled firing, so `timeout`,
`on_overlap`, run history and the per-run log all apply. It is recorded with
`trigger=manual`, and the schedule carries on as before. Without `--wait`,
`trigger` starts the run and returns immediately.

`--wait` follows the run by polling the run history, which stays authoritative
even when an event is dropped. One consequence is worth knowing if you trigger
by hand while a run is in flight: a queued `--wait` attaches to the oldest
manual run newer than the moment you issued it, so two manual triggers fired at
the same time can each end up streaming the other's run. Scheduled firings never
collide this way.

With `--wait`, the command exits the way the run did, so scripts can use it like
the command it wraps:

| Exit code | Meaning |
|-----------|---------|
| the run's own exit code | the run finished; `0` is success |
| `124` | the run hit `timeout` |
| `75` | a run was already in flight, so this trigger was recorded as `skipped` |
| `1` | the run failed without a usable exit code |

`trigger` only accepts scheduled harnesses; use `harness start` for anything
else. Don't reach for `harness run`: that is the scratchpad verb, which runs a
throwaway harness and attaches to it. `harness trigger` is the one that fires a
job.

## Reading run outcomes

Every run gets a numbered record in the daemon's state and a log file of its
own. `harness runs` lists the records, newest first:

```sh
$ harness runs pr-sweep --limit 3
RUN   TRIGGER    OUTCOME    STARTED        DURATION   EXIT
14    manual     skipped    Sep 11 14:02   —          —
13    manual     success    Sep 11 14:00   6m41s      0
12    schedule   failed     Sep 10 14:00   2m03s      1
```

Firings that started nothing, such as `skipped` and `missed`, are recorded too,
so a quiet history still tells you what happened. `--json` gives the same
records to scripts.

`harness logs` shows what a run did. Without `--run` it shows the latest run;
`harness logs pr-sweep --run 12` shows run 12:

```sh
$ harness logs pr-sweep
run 2026-09-11 14:00:00 → 14:06:41 · exit 0 · claude-code
14:00:00  state     stopped → starting
14:00:00  state     starting → running
14:00:03  session   3f9a1c2e · claude-code · claude-sonnet-5
14:00:04  read      /home/you/sweeps/pr-sweep/AGENTS.md
14:00:09  exec      gh pr list --state open --json number,title
…
14:06:41  exited    code=0
14:06:41  state     running → stopped
```

When the agent's own session transcript can be matched to the run, the lines
between the lifecycle events are the agent's actions:

- `read` and `edit` show the file.
- `exec` shows the command, with `(failed)` if it failed.
- `ERROR` is the error the agent hit.

The match uses the harness's `workdir`, which is one more reason to set one. When
nothing can be matched, `harness logs` says so in a `note` line and prints the
durable log tail instead.

`--raw` shows the durable log instead: the run's own output, bracketed by
lifecycle lines that carry its outcome:

```sh
$ harness logs pr-sweep --raw --lines 20
2026/09/11 14:00:00 INFO run started run_id=12 trigger=schedule
2026/09/11 14:00:00 INFO state changed from=stopped to=starting
2026/09/11 14:00:00 INFO state changed from=starting to=running
…agent output…
2026/09/11 14:06:41 INFO exited code=0
2026/09/11 14:06:41 INFO state changed from=running to=stopped
2026/09/11 14:06:41 INFO run finished run_id=12 outcome=success exit_code=0
```

`--run N` works with `--raw` too. It can't be combined with `--follow`; to watch
a run as it happens, use `harness trigger NAME --wait` or `harness logs NAME
--follow`.

Each run's log is also a plain file, which makes history easy to grep:

```sh
ls ~/.local/state/harness/jobs/pr-sweep/          # 11.log 12.log …
grep -h 'run finished' ~/.local/state/harness/jobs/pr-sweep/*.log
```

The outcome on each `run finished` line is one of:

| Outcome | Meaning |
|---------|---------|
| `success` | the process exited 0 |
| `failed` | it exited non-zero, or could not start |
| `timed_out` | `timeout` stopped it |
| `skipped` | a window fired while a run was in flight and `on_overlap = "skip"` |
| `replaced` | `on_overlap = "replace"` stopped it for a newer run |
| `missed` | the machine was asleep or the daemon was down, and `catch_up = false` |
| `cancelled` | you stopped it |
| `interrupted` | the daemon went away mid-run; recorded on the next boot |

`success` only means **the process exited 0**. The next section is about
closing the gap between that and "the sweep did its job".

## Designing a sweep you can trust

A sweep runs with nobody watching. The failure modes are quiet: the agent
misreads the task, stops halfway, can't authenticate and apologizes, or does
the work and never tells anyone. All of those exit 0. A few habits make them
visible.

### One scope per sweep

A sweep that "checks CI, reviews PRs, triages issues and updates the docs" does
all four badly. It also fails as a unit, and it is impossible to tell from the
outcome which part went wrong. Give each concern its own harness, its own
prompt file, its own schedule and its own run history. Harnesses are cheap;
drop-in files make them cheaper still.

### Keep the starting context small

Everything the agent loads before it reads your prompt is paid for on every
run: its system prompt, every MCP server's tool definitions, and project
instruction files like `CLAUDE.md` or `AGENTS.md` found in the `workdir`.

- **Give each sweep its own small `workdir`**, such as `~/sweeps/pr-sweep`,
  holding only what that sweep needs, rather than pointing it at a large
  repository.
- **Configure only the MCP servers the sweep uses.**
- **Pick the smallest model that does the job reliably.** Many triage, digest
  and check sweeps run well on a small model, or a local one through a
  provider your agent supports. Save the large models for sweeps that need
  judgment.

### Write a completion contract into the prompt

Tell the agent exactly what "finished" looks like, and make it say so in a form
you can check:

```markdown
# PR review sweep

Scope: open pull requests in ONE repository, example-org/example-repo.
Do not touch any other repository.

1. List open PRs that have no review from you yet.
2. Review each one: leave one comment with findings, or approve if clean.
3. Post a summary (see "Always report").

## Always report

Post a summary to the team channel on EVERY run, including runs where
there was nothing to do ("0 PRs needed review").

## Finish

Your final message MUST be exactly one line, in one of these forms:

SWEEP_RESULT: ok | reviewed=N | summary=LINK
SWEEP_RESULT: blocked | REASON

If you cannot finish (auth failure, missing tool, anything), stop and
emit the blocked form. Never end without a SWEEP_RESULT line.
```

Then a run that exited 0 **without** the line is the one to look at:

```sh
# runs that finished but never emitted the contract line
grep -L 'SWEEP_RESULT:' ~/.local/state/harness/jobs/pr-sweep/*.log
grep -h 'SWEEP_RESULT: blocked' ~/.local/state/harness/jobs/pr-sweep/*.log
```

### Always send a summary; silence must mean nothing happened

Have every run report somewhere you actually read, such as a chat message, an
issue comment, or a [Cairn](https://cairn.stump.wtf/docs/intro/) report. Do this
even when there was nothing to do. The rule you want is:

- **a summary arrived** → the sweep ran, and the summary says what it did;
- **no summary** → something is wrong: it didn't fire, crashed, or couldn't
  reach its tools.

If "nothing to do" is also silent, you can never tell a quiet day from a broken
sweep. Make the summary short, and put detail behind a link.

## Putting it together

```toml
# ~/.config/harness/harness.toml
[server]
harness_d = "harness.d"
```

```toml
# ~/.config/harness/harness.d/ci-digest.toml
[harness.ci-digest]
harness = "crush"
prompt_file = "~/.config/harness/prompts/ci-digest.md"
model = "your-small-model"
auto_accept = true
workdir = "~/sweeps/ci-digest"
env_file = "~/.config/harness/env/ci-digest.env"
schedule = "CRON_TZ=UTC 0 7 * * *"
catch_up = true
timeout = "20m"
on_overlap = "skip"
keep_runs = 30
description = "morning CI digest"
```

```sh
harness reload                       # pick up the new drop-in
harness trigger ci-digest --wait     # run it once now, and watch it
harness runs ci-digest               # its history from here on
harness jobs                         # every sweep at a glance
```

Next: [push events with MCP channels](./push-events), where agents wake on events
instead of a clock.

---
status: proposed
date: 2026-10-17
decision-makers: [joestump]
extends: [ADR-0005, ADR-0007, ADR-0033]
related: [ADR-0006, ADR-0019, ADR-0021]
---

# ADR-0046: Per-harness health probes — the daemon runs an operator-supplied probe command and owns the verdict, the streak, and the action

> **Not yet accepted.** Proposed in the PR for stump.wtf/harness#960 by the
> m lane; no maintainer decision exists yet. The issue is an imported outside
> report, and the only upstream reply was the mirror-intake ack.

## Context and Problem Statement

Operators with their own liveness rules run a sidecar — a cron or launchd
sweep that polls `harness list`, scrapes logs, restarts through the CLI and
keeps its own cooldown state. Every such sweep re-implements the same loop,
and the sweep is itself a separate thing that can silently stop working: the
watchdog nobody watches. The daemon already supervises the process, so it is
the only party that can interleave a liveness check with the lifecycle
without a second owner of the restart decision.

The report's proposal is systemd-`WatchdogSec`-style: a per-harness probe
command on an interval, a consecutive-failure count, and an action. The
daemon can check that a process is up; it cannot know that "my work queue
has items older than fifteen minutes and the agent hasn't claimed any."
That domain knowledge belongs to the operator; everything around it —
scheduling, counting, cooldown, restart, alerting — belongs to the daemon.

## Decision

Four keys on `[harness.*]`:

```toml
[harness.worker]
health_cmd      = "/path/to/probe.sh"  # exit 0 = healthy
health_interval = "5m"                 # default
health_failures = 3                    # default; consecutive failures before action
health_action   = "restart"            # "restart" | "notify" (default restart)
```

The probe is a daemon-side hook in ADR-0033's sense, like the `[notify]`
program: it gets no PTY, no restart policy, no run record, and it runs only
because the daemon has something to ask. Details that were choices:

* **When it runs.** Only while the harness is enabled and its state is
  `running` or `starting`. A stopped harness has no process to interrogate,
  and a streak accrued before an operator's stop must not fire an action at
  something the operator is working on; the streak resets when the harness
  comes down.
* **What it sees.** Its environment is the daemon's plus `HARNESS_NAME`,
  `HARNESS_PID`, `HARNESS_STATE` and `HARNESS_LAST_SUCCESSFUL_CALL` (the
  last successful probe, RFC 3339, empty when there is none). The last name
  is shared deliberately with the `stall_timeout` watchdog (#959) so one
  probe body serves both.
* **How it is exec'd.** `health_cmd` is a program, not a shell line: it is
  exec'd directly, like a `command` harness's `argv`. The config file is
  trusted for what it runs; the config text is never a script.
* **Where the exec lives.** In `internal/healthprobe`, not in
  `internal/supervisor`. The admission funnel (SPEC-0021 REQ-4) holds every
  process start in the supervisor package to spawn's exec sites, with no
  exceptions, and a probe start is deliberately not a harness start: it
  takes no admission and consumes no run budget. The notify hook (ADR-0033)
  escapes the same walk by living in its own package; the probe does too,
  so the funnel check keeps its zero-exception shape.
* **How long it gets.** A fixed 30-second timeout (`DefaultHealthProbeTimeout`).
  A probe that hangs is itself unhealthy, and a hung probe must not hold the
  prober open; outrunning the timeout counts as one failure.
* **When it acts.** On the `health_failures`-th consecutive failure. After
  an action the streak resets and one `health_interval` of cooldown passes
  before the probe may act again; a healthy probe also clears the streak.
  The same streak cannot page twice per interval.
* **What it does.** `restart` restarts the harness through the normal verb
  path with source `health`; `notify` changes nothing. Either way the trip
  lands on the lifecycle bus as `harness_health`, which the notify Watcher
  turns into the new `health` notify event, and the daemon's log carries it.
  The probe's last ten results (time, exit code, output tail) stay in
  memory: `harness list` and `describe` show `health: ok|failing`, and
  `describe` additionally shows the last-N ring. Nothing about probes is
  persisted — like the session guard's flags, a daemon restart re-learns
  them.
* **A probe that never succeeds is not a failing harness** on its own
  account: it is a broken probe, and it still trips the action — systemd's
  watchdog makes no distinction either, and the alternative is a silent
  probe. The `health: failing` verdict plus describe's output ring is where
  "exit 127: file not found" reads as what it is.

## Alternatives Considered

* **The sidecar sweep, unchanged.** Rejected by the report's own argument:
  N operators, N copies of the loop, and the watcher itself is unwatched.
* **A global `[health]` table** with per-harness overrides. Rejected: the
  harness table is where every per-harness runtime key lives (ADR-0006);
  a second home would dodge the config census and the TUI edit round-trip
  guard the same way the budget keys almost did (#161).
* **Ledger lines per probe result.** Rejected: the ledger is run-scoped
  (SPEC-0021), and a probe that runs every five minutes is not a run. The
  in-memory ring is enough for `describe`, and the trip is the event worth
  keeping.
* **`sh -c health_cmd`.** Rejected: config values are already exec'd
  directly for `command` harnesses and the notify hook; a shell here buys
  nothing and turns a typo'd quoting detail into an attacker-shaped one if
  a repo ever carries a project-scoped `health_cmd`.

## Consequences

* Operators delete their liveness sidecars for the cases the env set can
  express; `HARNESS_LAST_SUCCESSFUL_CALL`-shaped probes compose with #959.
* One more operator-supplied program the daemon executes: same trust model
  as the notify hook — whoever can edit the global config could run anything
  anyway; a project-scoped `health_cmd` inherits the project file's existing
  trust assumptions and nothing new.
* The prober is one goroutine with a 5-second rescan tick: intervals are
  honored within five seconds, which is well under any interval worth
  configuring.
* `health: ok|failing` on `list --json` gives dashboards the verdict without
  the outputs; only `describe` pays for the ring.

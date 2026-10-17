---
status: proposed
date: 2026-10-17
decision-makers: [joestump]
extends: [ADR-0005, ADR-0006]
related: [ADR-0013, ADR-0020, ADR-0028]
---

# ADR-0045: `stall_timeout` — a progress-based watchdog for persistent harnesses that stay alive and stop making progress

> **Proposed.** This decision arrives as a design record alongside its
> implementation, because no maintainer decision on it exists yet: the shape
> below — what "progress" means, where the keys live, and what the watchdog is
> allowed to do to a running agent — is what needs reviewing. Nothing here is
> settled until this file reads `status: accepted`.

## Context and Problem Statement

A persistent harness is supervised for the wrong failure. The daemon notices
when a process **exits** and applies the `restart` policy
(ADR-0005, SPEC-0003 REQ "Restart On Exit"). It does not notice a process that
stays alive and stops doing anything: an agent blocked inside a tool call, a
worker waiting on an MCP server that never answers, a hung child, a connection
to a peer that went away. That is the failure issue #959 describes, and from the
outside it is the worst kind — `harness list` says `running`, the exit-driven
restart never fires, scrollback stops growing, and the only thing that tells the
operator is that nothing arrives.

Nothing else in the tree covers it:

* `restart` is an exit policy. A wedged process never exits, so the policy is
  never consulted.
* `timeout` (SPEC-0008) is a **per-run wall clock on a harness that fires**. On
  a resident worker it degenerates into a ceiling that kills healthy agents on a
  schedule, and left unset it never notices a wedge. Both readings are wrong for
  a process meant to live for days.
* The metrics already admit the gap: SPEC-0013 exports
  `harness_last_successful_call_timestamp` and the recommended alert rule is
  literally `time() - harness_last_successful_call_timestamp > 900` (ADR-0020).
  Prometheus can page about a stall. It cannot revive anything, and the daemon
  that owns the process does nothing with the same signal.
* The runaway tool-loop guard (issue #390) catches an agent that repeats one
  call with identical arguments. That is motion, not progress: a worker blocked
  on a single call produces no repeated calls at all, and looks calm.

The daemon has the pieces to close this: the observer already classifies what
the agent's sessions say it did, the Manager already owns the process and its
restart bookkeeping, the ledger already records why each process ended, and the
notify pipeline already reaches the operator. What is missing is a decision about
what counts as *making progress*, and how much violence a watchdog is allowed to
do to a running agent.

## Decision Drivers

* **Measure progress, not elapsed time.** A nine-hour task that calls tools for
  nine hours is healthy. A two-hour block is not. Any wall clock forces the
  operator to pick which of those two they are willing to kill.
* **A watchdog that is wrong more often than it is useful is worse than none.**
  Restarting an agent mid-thought destroys the thought. The default must be the
  quiet, non-destructive finding, and the destructive action must be one the
  operator asked for by name.
* **One source of truth for "did this agent do anything".** If the watchdog
  invents its own liveness signal it will disagree with the metric that already
  exports one, and an operator reading `harness list` and `/metrics` will get two
  answers.
* **The supervisor owns the process.** A watchdog that reaches past the
  supervisor to signal a PID forks the exit path into two places that decide what
  a death means — the ledger, the crash-loop latch and the restart policy all
  live in the supervisor.
* **A stall is not a failure and not a crash.** It says nothing about whether
  the harness works, so it must not feed the consecutive-failure counters,
  `harness_scheduled_runs_total{outcome}`, or an operator's belief that the
  agent is broken.
* **A recovery loop must be bounded.** A watchdog that restarts a worker that
  then wedges again, forever, is an infinite process churn with a log line each
  time. The existing crash-loop give-up cannot be that bound (below).
* **One `harness.toml`, no external timer.** The config file stays a complete
  description of what the daemon does (ADR-0006). No systemd watchdog, no cron
  job calling `harness restart`, no side process.

## Considered Options

### 1. How progress is measured

* **Option 1 — Observed successful model/tool calls.** The agent-event observer
  delivers a classified call for each thing the agent did that affected the
  world. This is the same event stream that already drives
  `harness_last_successful_call_timestamp` (ADR-0020), run state in the ledger
  (ADR-0028) and the loop guard (issue #390).
* **Option 2 — Scrollback or log growth.** File mtime or size.
* **Option 3 — A heartbeat the agent is told to write.** A new contract with
  every adapter.
* **Option 4 — CPU or system call activity of the PID.**

### 2. Where the watchdog lives

* **Option 1 — A subscriber guard beside the loop guard.** Its own package: it
  consumes the observer, samples the Manager on an interval, and hands a finding
  back to the Manager.
* **Option 2 — A timer inside the Supervisor**, armed at spawn like the run
  `timeout`, reset by a method the observer calls.

### 3. What happens when it trips

* **Option 1 — `notify` by default, `restart` on request, `none` for silence.**
  Three values on one key; the destructive one is opt-in.
* **Option 2 — Always restart.** The watchdog "fixes" the wedge with no config.
* **Option 3 — Always notify, never restart.** Nothing is ever killed; a wedged
  worker stays wedged until a person arrives.

### 4. What the killed resident run is recorded as

* **Option 1 — A new `stalled` outcome** in the ledger's outcome set
  (SPEC-0022 REQ-5), carrying no verdict.
* **Option 2 — Reuse `failed`** with an exit code.
* **Option 3 — Reuse `replaced`** (a run stopped so another could start).

### 5. What bounds a stall-restart loop

* **Option 1 — The supervisor's crash-loop latch** (`Policy.MaxRestarts`),
  which counts exits inside a short crash window.
* **Option 2 — A guard-side counter of consecutive stall actions per harness**,
  released by any observed progress.
* **Option 3 — Nothing**; the operator notices.

### 6. Which harnesses the keys are allowed on

* **Option 1 — Persistent harnesses only;** `stall_timeout` on a harness with a
  `schedule` or `triggers` is a config error.
* **Option 2 — Any harness;** the watchdog and the run timeout coexist.

## Decision Outcome

Chosen: **Option 1** for measurement, **Option 1** for placement, **Option 1**
for the action, **Option 1** for the outcome, **Option 2** for the loop bound,
**Option 1** for key placement.

### The schema

```toml
[harness.claude-src]
harness = "claude-code"
args = ["--remote-control", "--continue"]
enabled = true

stall_timeout = "45m"    # idle with no successful call before the watchdog acts
stall_action = "restart" # restart | notify (default) | none
```

`stall_timeout` is a duration, `0` or omitted meaning the watchdog is off.
`stall_action` names what a finding does and defaults to `notify`. It requires a
non-zero `stall_timeout`: an action with nothing to trip it is a key that lies
about what the daemon will do. Both keys are **refused at load on a harness with
a `schedule` or `triggers`** — see "Where the keys are refused" below.

### Progress is a model call, and only one thing is measured

A harness's stall clock runs from the later of:

1. the start of its current process, and
2. the last **observed successful call** the observer reported for it —
   the same `observe.KindTool` event that sets
   `harness_last_successful_call_timestamp` (SPEC-0013 REQ-3).

"Later of" is what keeps a worker that has not been asked to do anything from
being accused of stalling at boot: a harness that started ninety seconds ago has
not been idle for forty-five minutes.

A plain text turn with no call is not progress, and neither is output growth: a
model can narrate for twenty minutes without affecting anything, which is exactly
the interval an operator wants to hear about. The clock is deliberately the same
one the metric exports, so the alert rule and the watchdog cannot disagree about
whether the agent is doing anything.

The consequence is stated plainly because it is the sharp edge of this decision:
**a harness the observer never reports on has no progress signal at all.** That
is a `generic` or `command` harness with no `transcripts` binding — `sh -c "sleep
forever"` and its like. Its clock runs from process start alone, which is a
wall-clock restart period by another name. `RestartLimit` exists for it, and
the documentation says so where the key is documented.

### It runs beside the supervisor, not inside it

`internal/stallguard` is a Guard in the shape of `internal/loopguard`: it
subscribes to the observer, samples `Manager.Snapshots()` on an interval
(`DefaultInterval`, one minute), and for every **running, non-triggered** harness
whose `stall_timeout` is armed, measures the idle span. One goroutine, no lock
held across a sample, and nothing added to the spawn or exit path.

Sampling makes the interval the blind window. One minute is invisible against a
timeout measured in tens of minutes and costs one snapshot pass per minute; a
timer per harness buys a faster finding nobody waits for and gives a second
implementation of "when did this process start" to keep in sync.

### An episode is acted on once per process

A finding is acted on the first time it is measured past the timeout, and not
again until either the process is replaced (a different start time) or progress
resumes. Without that latch a `notify` harness would page every minute and a
`restart` harness would be killed again the moment the guard next looked. A
stall is therefore one log line, one post-mortem, one notification and at most
one restart — for that process, that episode.

The start time is the token throughout: the guard samples it, `ReportStall`
carries it to the supervisor's actor goroutine, and the supervisor drops the
finding if it no longer names the current process. That check is the difference
between a watchdog and a race condition — the guard's clock runs on its own
goroutine, so without it a finding about a worker that died and was replaced
eight seconds ago would kill the replacement.

### `restart` goes through the ordinary exit path, whatever `restart` says

`stall_action = "restart"` calls `Manager.ReportStall`, which hands the finding
to the harness's own actor goroutine. That goroutine kills the process group the
way a graceful stop does — SIGTERM, then SIGKILL after the stop grace — and hands
the exit to `onProcessGone`, the same handler every other exit goes through, with
one flag set:

* the exit line and the `EventExited` the log readers and `harness logs` expect;
* the resident run closed `stalled`, with the exit code it actually had;
* the crash-window bookkeeping, `consecFailures`, flapping backoff and the
  give-up all see a normal exit;
* and the respawn happens **even when `restart = "no"`**.

That last point is the deliberate exception, and the one most likely to be
argued with. `restart` answers "what do you do when the process exits on its
own". An operator who writes `stall_action = "restart"` has answered a different
question — "what do you do when it stops making progress" — with *revive it*. A
harness with `restart = "no"` and `stall_action = "restart"` is a coherent
request: never respawn me when I finish, but pull me out of a wedge. Suppressing
the revive on the policy would make the one action the operator opted into
silently do nothing, which is the failure mode this whole ADR exists to remove.

Killing the process group rather than the whole supervisor, and reusing the exit
path rather than inventing one, is what keeps a stall from drifting into
different ledger, log and restart semantics than every other death.

### The outcome is `stalled`, and it carries no verdict

`stalled` joins the ledger's outcome set (SPEC-0022 REQ-5) beside `timed_out` and
`quota_parked`, and `harness runs --outcome stalled` filters on it. It is not
`failed`: the worker was blocked, which says nothing about whether the harness
works. Not `replaced`: nothing was waiting to start. And like every other
non-verdict outcome it counts as neither success nor failure in
`harness_scheduled_runs_total{outcome}` and in the consecutive-failure count, so
a stall cannot make a healthy harness look broken in the counters an operator
reads.

The same reasoning is why the supervisor does not treat a stall exit as a crash
for the *crash-window* purpose of escalating to `failed`: an exit that follows
six hours of good work is not a crash, and `resetCrashState` on a healthy run
already keeps it that way.

### The loop bound the crash latch cannot provide, and the one it still does

Two latches bound a stall-restart loop, and both are needed because they see
different loops.

**The supervisor's crash-loop latch is unchanged, and handles the fast loop.** A
worker that comes up, wedges for its 45 minutes, is restarted, and wedges again
is not exiting inside `CrashWindow`, so the existing give-up never fires. But a
worker whose restart fails immediately — a bad `workdir`, a binary that no longer
exists — exits fast, and the ordinary crash-loop give-up catches it exactly the
way it catches any other crash loop. This decision adds nothing there.

**The guard counts consecutive stalls with no progress between them**
(`DefaultRestartLimit`, five, matching `Policy.MaxRestarts`' "we have tried this
many times"). Reaching it flips the harness to report-only: the finding is still
logged, still notified, still says `GaveUp`, and the next restart does not
happen. Any observed call resets the counter, because a worker that called a tool
since its last stall is making progress however it got there.

Why the guard needs its own counter at all: for a harness with no transcript
binding there is **no progress signal to reset it**, so a `generic` worker that
exits, starts, and blocks before its first call would be revived forever — five
processes an hour, each with a confident log line saying the watchdog fixed it.
The counter is the bound that structural blindness forces. The give-up is the
right terminal state for it: a worker that wedges before it ever calls a tool is
not being fixed by another restart, and the honest message is "a person now".

Rejected as the bound: the crash-loop latch alone (blind to a slow stall), and
capping stalls per day (a clock again, on a feature whose premise is that a
clock is the wrong question).

### The post-mortem is written before the fix

A stall dumps the harness's process tree (`internal/proctree`, via `ps`) into the
daemon log, the harness log and the notification: root first, nearest children
next, capped at twelve lines. A wedged agent is waiting on *something* — the MCP
server, the `git` child, the browser — and the tree is the shortest description
of what. It is captured before the restart because afterwards there is nothing
left to ask, and it is read from the process table rather than from anything the
daemon maintains: the daemon deliberately does not know what a harness is doing
inside (ADR-0005), so the post-mortem is allowed no more insight than `ps`
gives. Where `ps` is missing the tree is simply absent and the finding stands on
its own.

### Where the keys are refused

`stall_timeout` and `stall_action` are refused at load on a harness with a
`schedule` or `triggers`. Such a harness already bounds every run with `timeout`
(SPEC-0008 gives it a default), a wedged one-shot is killed by that, and its next
firing is the retry. A progress watchdog on top would restart the same firing a
second time, and a run that takes longer than it should on a cron harness is a
`timed_out` record, not a stall. Refusing the keys is also what keeps the two
watchdogs from describing different behavior on the same harness: the config file
is the complete description (ADR-0006), and a key that the supervisor would
ignore is a lie in it. The same rule already exists for `timeout`,
`on_overlap` and `keep_runs`, so the parser shape is not new.

So: `stall_timeout` is the watchdog for a process that never exits, and
`timeout` is the one for a run that should have finished. Each harness kind gets
exactly one.

### What an operator sees

| Surface | With a stall |
| --- | --- |
| daemon log | `WARN harness stalled` with state, idle, `stall_timeout`, action, restart count, `gave_up`, then the process tree line |
| harness log | a lifecycle line naming the cause and what was done, including `harness restart NAME` when the guard has stopped reviving it |
| run ledger | the resident run closes `outcome=stalled` with its real exit code |
| `harness runs` | the record, filterable with `--outcome stalled` |
| notify hook | event `stalled`, in the default event set, with `restarts`; the message carries the idle span, the timeout, the action and the tree |
| `/metrics` | unchanged: `harness_last_successful_call_timestamp` was already the signal, and is what the guard read |

`stalled` opens an alert only on `GaveUp`. An ordinary `restart` stall is fixed
by itself, and an alert thread that closes minutes later on a worker that never
stopped is noise; the give-up is the moment a person is actually needed, and
that is the one a `recovered` should eventually answer.

### Consequences

* A persistent harness can now be recovered from a wedge without an operator, and
  without a wall clock that kills healthy long tasks.
* `stall_action` defaults to `notify`: upgrading changes nothing about any
  process, because the watchdog is off until a `stall_timeout` is written.
* Two new config keys, one new outcome, one new notify event, two new packages
  (`internal/stallguard`, `internal/proctree`), one new Manager method
  (`ReportStall`), and four fields on `Supervisor`. No new state on disk, and
  nothing on the spawn path.
* A harness with no transcript binding gets a wall-clock restart period from the
  same key. The documentation has to say so, or the key reads as progress-based
  where it is not.
* The run ledger's outcome row for `stalled` (SPEC-0022 REQ-5) and the
  `stalled` event in SPEC-0003 REQ "Operator Notification" ship with this code;
  accepting this decision is what ratifies them, since until then they describe
  a proposed design.

## Pros and Cons of the Options

### 1. How progress is measured

* **Option 1 — Observed model calls.** Used.
  * Good: one liveness signal shared with the metric, the ledger and the loop
    guard; says nothing about which agent runs inside; already plumbed.
  * Bad: blind to a harness the observer never reports on, which is why the
    consecutive-stall bound exists; a legitimately long single tool call (a
    twelve-minute build) reads as idle, which is what `stall_timeout` is for the
    operator to size.
* **Option 2 — Scrollback growth.** Rejected: measures output, not effect. An
  agent narrating, retrying or stuck in a loop that prints is not making
  progress, and a quiet agent running a four-minute command is.
* **Option 3 — Agent heartbeats.** Rejected: a contract with every adapter, and
  an agent that is wedged is exactly the thing that stops writing its own
  heartbeat.
* **Option 4 — Process CPU.** Rejected: a blocked process looks identical to a
  healthy one, and it would break the adapter-agnostic rule the supervisor is
  built on.

### 2. Where the watchdog lives

* **Option 1 — Subscriber guard.** Used. Sits beside the loop guard, reads the
  progress signal at its source, and holds all of its state off the hot paths.
  * Bad: a sampling interval rather than an exact timer (a bounded blind window),
    and a second consumer of `Snapshots()` to keep honest about generations —
    which is what `LastStarted` as the token is for.
* **Option 2 — Supervisor timer.** Rejected: the supervisor would have to be
  notified of progress by the observer, arming and disarming a timer on every
  call. That is the same signal arriving through a longer path, plus a timer per
  harness, plus the observer now knowing the supervisor's internals. The reset
  belongs where the events are.

### 3. What happens when it trips

* **Option 1 — Default notify, restart on request.** Used. Wrong findings cost a
  message; the destructive action is written by hand.
* **Option 2 — Always restart.** Rejected: a false positive then destroys an
  agent's in-flight context, on a daemon-wide default, for someone who never
  asked for a watchdog.
* **Option 3 — Never restart.** Rejected: leaves the failure as loud-but-unfixed
  for the exact harnesses that need to survive an unattended night.

### 4. What the run is recorded as

* **Option 1 — New `stalled` outcome.** Used, because a stall is neither a
  failure nor a replacement, and both fakes would feed a verdict into counters
  that mean "the harness is broken".
* **Option 2 — `failed`.** Rejected: breaks SPEC-0022's consecutive-failure and
  `harness_scheduled_runs_total{outcome}` semantics on a harness that is fine.
* **Option 3 — `replaced`.** Rejected: says an operator replaced the run, which
  is a false statement about who did what.

### 5. What bounds the loop

* **Option 1 — Crash latch only.** Rejected: structurally blind to a stall that
  follows hours of good work, and correct to be blind — an exit after six hours
  is not a crash.
* **Option 2 — Guard-side consecutive counter.** Used: reset by progress, so it
  never counts across good work, and it is the only bound that works for a
  harness with no observable progress at all.
* **Option 3 — Nothing.** Rejected: an unattended restart loop is a process
  churn with a confident log line.

### 6. Where the keys are allowed

* **Option 1 — Persistent only, refused on a triggered harness.** Used: each kind
  of harness then has exactly one watchdog, and no key sits in the file inert.
* **Option 2 — Anywhere.** Rejected: two watchdogs on one run, an operator unable
  to tell which one killed a run, and `run_failed` and `stalled` competing for
  the same death.

## More Information

* Issue #959 is the report. Issue #958 (put the last-call activity on
  `harness list`/`describe`) is the read side of the same signal and is
  deliberately not implemented here: the watchdog needs `Snapshots()` and the
  observer, not a new CLI column.
* "Was there pending work?" — gating a stall finding on the harness actually
  having been asked to do something — is out of scope. The daemon does not know
  what a persistent harness has been told to do, and every available proxy for
  it (recent input, an open session, a recent non-idle state) is a clock in
  disguise. The "later of process start and last call" rule is the whole of what
  this decision can honestly say about idle-with-nothing-to-do.
* A Prometheus counter for stall findings is a follow-up; the existing last-call
  gauge already supports SPEC-0013's alert rule, so a counter is polish, not a
  gap.
* The guard's `RestartLimit` and interval are Go defaults, not config keys.
  Nothing in the tree makes the loop bound configurable, and a key that lets an
  operator set it to a large number is a footgun with a knob on it. A
  `harness describe` surface for the guard's stall state and consecutive-stall
  count would make the give-up visible without reading the log; it is a follow-up.

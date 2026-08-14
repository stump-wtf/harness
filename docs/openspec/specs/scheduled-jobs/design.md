# Design: Scheduled One-Shot Runs

## Context

Harness supervises resident processes: SPEC-0003 respawns a harness on exit —
including a clean one — while its restart policy permits, because "agents and
watchers are meant to be long-lived." The other half of the workload is the exact
inverse: nightly sweeps, weekly grooming, daily syncs that are *supposed* to
finish. That work lived outside Harness in launchd plists and ad-hoc scheduled
tasks.

ADR-0013 puts the clock inside the daemon and expresses a scheduled unit as a
`schedule` key on `[harness.*]`, fired against an ADR-0011 prompt one-shot.
Governing spec: SPEC-0008. Related: SPEC-0002 (control plane and attach),
SPEC-0003 (the lifecycle machine reused unchanged), SPEC-0004 (project-scoped
registration), ADR-0003 (owned PTY), ADR-0006 (config-of-record), ADR-0007 (state
and logs), ADR-0008 (secrets and file modes), ADR-0011 (prompt harness).

> **Revision note (2026-08-13).** This document originally described the
> `[job.*]` design proposed on 2026-07-26. It has been rewritten to describe what
> shipped in #122. The capability it previously specified but that was not built
> — run history, per-run logs, timeouts, catch-up, timezones, queue/replace
> overlap, wall-clock polling — is tracked in ADR-0013's *Deferred* section and
> SPEC-0008's *Out Of Scope*.

## Goals / Non-Goals

### Goals

- A supervised unit whose exits are **terminal**, reusing the existing harness
  path rather than special-casing it.
- One init unit still (ADR-0005) — no systemd `.timer`s, no launchd calendar
  plists, no `systemctl`-versus-`launchctl` branching returning to the codebase.
- The smallest schema change that lets a `[harness.*]` block replace a launchd
  plist: exactly one key.
- Every ambiguous combination of that key with the existing schema is a located
  parse error, not a silently-ignored setting.
- Surviving the config rewrite cadence: schedules must not lose their phase when
  chezmoi or `czu` rewrites `harness.toml` without changing it.
- Attaching to an in-flight run — the "hop" interaction applied to scheduled
  work — falls out of reusing the PTY path rather than being built twice.

### Non-Goals

- **Notification delivery.** No Signal, no webhooks, no `on_failure` exec hook.
  The daemon knowing how to reach an external service contradicts the one thing
  it is designed not to know. Events are the seam; a notifier is a client.
- **Retry within a window.** A failed run is a failed run; the next firing is the
  next attempt.
- **Backoff between firings.** The schedule *is* the rate limiter.
- **Agent-awareness.** The daemon does not know a sweep from a `sleep`.
- **Distributed or multi-host scheduling.** One daemon, one host, one clock.
- **Run history and answerability after the fact.** Deliberately deferred, and
  the largest known gap — see Risks.

## Decisions

### The restart policy already existed; `schedule` did not

**Choice**: Do not introduce a job-versus-harness restart axis. Use the existing
`restart` key, whose prompt-harness default is already `no`.

**Why**: ADR-0013's original draft proposed `always`/`never` as a property
derived from the table kind. Between the draft and the implementation, `restart`
shipped as a real user-facing key (`no` / `always` / `unless-stopped` /
`on-failure`), and ADR-0011 introduced the prompt harness with `restart = "no"`
as its default. "Terminal by design" was therefore already expressible, and
SPEC-0003 REQ "Restart On Exit" was already conditional on policy. No spec
amendment is needed.

The residue is a validation rule rather than a type: `schedule` rejects
`always` and `unless-stopped`, because either would respawn the one-shot after a
clean exit and make the schedule meaningless.

### `schedule` is a key on `[harness.*]`, not a table kind

**Choice**: One key on the existing table; six exclusions enforced at parse time.

**Why**: See ADR-0013 "Why 2B, given 2A's argument". In short — the separate
`[job.*]` table was motivated by lifecycle distinctness that `restart` and the
prompt harness now supply, and by the parser's ability to reject nonsense, which
validation on one table achieves at a fraction of the downstream cost. What a
table kind would still have bought is type-based dispatch in consumers; that is
the accepted cost, tracked as
[#160](https://gitea.stump.rocks/stump.wtf/harness/issues/160).

The exclusions are the load-bearing part of this choice and are enumerated in
SPEC-0008 REQ "Schedule Exclusions". Notably `enabled` is **not** redefined to
mean "armed" — it keeps its SPEC-0003 meaning and is simply forbidden alongside
`schedule`, which is what keeps the two intents from blurring.

### Validate the cron expression with the scheduler's own parser

**Choice**: `internal/config` parses the expression at load time using
`cron.ParseStandard`, the same parser backing the scheduler's `cron.New`.

**Why**: A typo must fail the load with a located error rather than silently
never firing. Sharing one parser means config and scheduler cannot disagree about
what is valid — a config that loads is a config every entry can register. The
scheduler still handles a registration error defensively, but it is unreachable
through the config path.

### Reconcile incrementally; never rebuild

**Choice**: One long-lived `cron.Cron`. `Apply(cfg)` diffs the desired set
against the registered set: unchanged specs keep their `cron.EntryID`, changed
specs are removed and re-added, vanished ones are removed.

**Why**: This is a correctness requirement, not an optimization. `robfig/cron`
computes an `@every` schedule's next fire as `now + interval` at registration
time, so rebuilding entries restarts that countdown. `harness.toml` is rewritten
on a timer by chezmoi and `czu` regardless of whether its contents changed, and
each rewrite triggers the fsnotify watcher. A rebuild-on-reload scheduler
therefore lets an `@every 6h` sweep reset its countdown more often than it fires
— it never fires at all. The test asserts `EntryID` identity across a no-change
re-apply precisely to pin this.

### One reload choke point

**Choice**: `Manager.SetReloadHook` registers a single callback invoked after
every successful `Reload`. The daemon sets it once at boot, before any reload
source exists.

**Why**: There are three reload paths — SIGHUP, the fsnotify config watcher, and
the `reload` control op — and they all funnel through `Manager.Reload`. Hooking
the individual sources instead means a path can be missed: an earlier iteration
wired SIGHUP and the watcher but not the control op, so with
`watch_config = false` a `harness reload` left stale cron entries firing until
the next SIGHUP or restart. Hooking the choke point makes that class of miss
impossible.

Registering the hook before `srv.Listen`, `watcher.Start`, `go srv.Serve`, and
`signal.Notify` is what makes the unsynchronized write safe: every reader
goroutine is created after the write, so goroutine creation supplies the
happens-before edge. The hook is documented as set-once and is not safe to
replace later.

### Overlap is fixed at skip

**Choice**: A firing is dropped when the harness is in any active state —
`starting`, `running`, `degraded`, or `stopping`.

**Why**: Skipping is the right default for a nightly job that occasionally runs
long, and it is the only policy with no queueing machinery behind it. Including
`stopping` matters for a reason beyond overlap: a firing landing during a
graceful stop would otherwise call `Start` and restore enabled intent, silently
undoing an operator's `harness stop`.

`degraded` and `restarting` are both post-exit backoff waits with no live
process. `restarting` fires and `degraded` does not; this asymmetry is
conservative rather than principled, and either behavior is harmless because a
harness in those states will respawn itself.

### Panic recovery is mandatory, not decorative

**Choice**: `cron.WithChain(cron.Recover(logger))`.

**Why**: `robfig/cron` v3 runs each firing in a bare goroutine with an empty
default chain. A panic in the firing callback would therefore take down the whole
daemon and every harness it supervises. The package documentation is misleading
on this point, so the wrapper is explicit and commented.

## Architecture

### Where the pieces live

| Concern | Location |
| --- | --- |
| `Schedule` field | `internal/core.Harness` |
| Parse + validate + exclusions | `internal/config/config.go` (`registerHarness`), `internal/config/project.go` (project rejection) |
| Profile-membership exclusion | `internal/config/config.go` (`Parse`, after harness registration) |
| Cron ownership and reconciliation | `internal/scheduler` |
| Reload choke point | `internal/supervisor.Manager.Reload` + `SetReloadHook` |
| Firing guard and wiring | `cmd/harness/daemon.go` |
| Config round-trip through the TUI | `internal/tui/form.go` |

### Firing path

```
cron entry due
  └─ firing callback (recovered)
       ├─ Snapshot(name)
       │    ├─ starting/running/degraded/stopping → log, skip
       │    └─ stopped/failed/restarting          → continue
       └─ Manager.Start(name)
            └─ ordinary supervised prompt spawn (PTY, env_file, workdir)
                 └─ exit is terminal for this firing
                      └─ restart policy applies only to abnormal exit
```

### Reconciliation path

```
SIGHUP ─┐
watcher ─┼─→ Manager.Reload ─→ reloadHook ─→ Scheduler.Apply(cfg)
control ─┘        │                              ├─ spec unchanged → keep EntryID (phase preserved)
                  │                              ├─ spec changed   → Remove + AddFunc
             (failed parse                       └─ gone/cleared   → Remove
              short-circuits
              before the hook)
```

### Lifecycle

`Scheduler.New` builds the cron; `Apply` may be called before or after `Start`;
`Close` stops the cron and waits for in-flight firings. The daemon calls `Apply`
once at boot, `Start` immediately after, and `Close` during shutdown ahead of
`srv.Close()` and `mgr.Close()`.

## Risks / Trade-offs

- **No run history.** The question this feature exists to answer — *did last
  night's run pass?* — is answerable only from the harness's single continuous
  scrollback and its last exit code. This is the largest gap between ADR-0013's
  stated motivation and what ships.
- **A missed window is invisible.** If the daemon is down at the fire time,
  nothing happens and nothing is recorded. System cron would have fired. There is
  no `missed` outcome to render.
- **Timing across suspend is unverified.** ADR-0013's draft mandated re-evaluating
  against the wall clock on a short tick rather than arming long timers, on the
  grounds that a laptop sleeping through 03:00 must reach a decision that is ours
  rather than the OS's. The shipped scheduler delegates to `robfig/cron`, which
  arms a timer to the next due entry. Whether Go's timer behavior across a darwin
  suspend satisfies the original requirement has not been tested.
- **A hung run owns its schedule.** With no `timeout`, an agent CLI that waits
  forever on input keeps the harness `running`, so every subsequent firing skips.
- **Enabled intent leaks.** A firing goes through `Manager.Start`, which persists
  `enabled = true`; an unclean daemon exit mid-run can autostart the one-shot
  off-schedule on the next boot
  ([#159](https://gitea.stump.rocks/stump.wtf/harness/issues/159)).
- **Consumers branch on a key.** Every surface wanting to render a scheduled
  harness distinctly tests `Schedule != ""`, and none do yet
  ([#160](https://gitea.stump.rocks/stump.wtf/harness/issues/160)).
- **Config writers must be kept in sync.** Because the TUI form rewrites a whole
  `[harness.*]` table, any schema key it does not carry is deleted on save. This
  bit `schedule` before release and still affects `tmux_socket`
  ([#161](https://gitea.stump.rocks/stump.wtf/harness/issues/161)). A
  table-driven test asserting every `core.Harness` key survives an edit
  round-trip would close the class.

## Migration Plan

No migration. `schedule` is additive and optional; every existing config parses
and behaves identically. Adopting it means moving a launchd plist or systemd
timer into a `[harness.*]` block and deleting the OS unit.

## Open Questions

- Does `robfig/cron`'s timer survive a laptop suspend well enough, or is the
  original wall-clock-polling requirement still needed?
- Should run history land as an extension of ADR-0007's state file, or as its own
  store? It is the prerequisite for a `harness run --wait` verb being worth
  adding (ADR-0013 option 1A).
- Is the `degraded`-skips / `restarting`-fires asymmetry worth resolving, and in
  which direction?

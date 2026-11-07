---
status: proposed
date: 2026-10-24
decision-makers: []
extends: [ADR-0013, ADR-0021, ADR-0023, ADR-0033]
related: [ADR-0006, ADR-0008, ADR-0046]
---

# ADR-0051: Run lifecycle hooks and per-run work directories — a run starts from a known state, and its outcome is checkable

> **Not yet accepted.** Proposed in the PR for stump.wtf/harness#980 by the
> m lane. The issue records the incident that motivated it; no maintainer
> decision exists yet, and the open questions below are the parts of the
> design nobody has decided.

## Context and Problem Statement

Lane workdirs on the fleet are long-lived and never cleaned. On 2026-10-23 a
one-shot run read stale state another run had left in the same directory
months earlier — old plan files, another run's uncommitted edits — acted on a
task that did not exist, and reported to the operator pushes and merges that
had never happened. Three monitors share one directory the same way, which
also makes per-run disk usage invisible.

Two gaps make this reachable, and neither is closed today:

- A run cannot be given a directory of its own. `workdir` is a fixed path, so
  every run of a harness inherits whatever its last run left.
- Nothing runs around a run. There is no `pre_run` to put a run in a known
  state and no `post_run` to check mechanically what the run produced; the
  only daemon-side hook is `[notify]`, which runs after the fact and can
  change nothing (ADR-0033). `success_check` (#493) is the designed verifier
  and is unbuilt, so nothing between "the process exited 0" and "the work got
  done" is checked.

`command` one-shots already template their argv (ADR-0023), so the syntax for
"a value the daemon fills in per run" exists; wrapping an agent in a shell
script does not, because it loses the transcript observer (loopguard, stall
detection, attestation).

## Decision

**Two argv hooks on a one-shot harness.** `pre_run = [...]` runs after the
run's record is open and before the harness process is spawned;
`post_run = [...]` runs after the process is reaped and before the record
closes. Each element is a SPEC-0017 template rendered against the run, exec'd
directly and never through a shell, with a `DefaultRunHookTimeout` of ten
minutes. A hook is a daemon-side hook like `health_cmd` and `[notify]`
(ADR-0033): no admission, no PTY, no run record of its own, no restart policy —
which is why the exec lives outside `internal/supervisor`.

A hook may name `harness.name`, `harness.workdir`, `model`, `package.dir`, the
`run.*` identifiers and dates, and `run.datadir`; `post_run` additionally
`run.outcome` and `run.exit_code`. Event paths and untrusted text are refused
in both tiers: where a hook needs a payload it is given `{{event.file}}` and
reads the envelope itself, so text an outside party wrote cannot become an
argument (SPEC-0017 REQ-10).

**A failed `pre_run` stops the spawn.** The run closes `failed` with reason
`pre_run_failed` and nothing is exec'd. A hook exists to put the run in a state
the agent can be trusted with; spawning anyway is precisely the failure the
hook was written to prevent. **`post_run` can demote, never promote:** a
nonzero exit turns a `success` into a `failed` with reason `post_run_failed`
and leaves every other outcome exactly as the process earned it. A run whose
`pre_run` never succeeded is never asked, because it has no verdict to check.

**Hooks inherit the daemon's environment, not `env_file`.** `env_file` is the
harness process's environment, and a hook is not that process; this is the
choice `health_cmd` already makes. A hook therefore sees
`HARNESS_NAME`, `HARNESS_WORKDIR`, `HARNESS_RUN_DATADIR`, the reserved
`HARNESS_RUN_*` variables, and for `post_run` `HARNESS_RUN_OUTCOME` and
`HARNESS_RUN_EXIT_CODE`.

**A templated `workdir` makes a run's directory per-run.**
`workdir = "~/sweeps/{{harness.name}}/runs/{{run.id}}"` renders at run start,
and the daemon creates it `0700` before the spawn (ADR-0008). Only identifiers
and dates may appear — never an event or untrusted path, and never
`harness.workdir` or `run.datadir`, which are the directory the template is
deciding. A templated workdir requires a one-shot: a resident harness is one
process, not one directory per run. With no run in flight (the control plane,
skill-root resolution, `describe`) a template resolves to the directory
HOLDING the run directories, which is the only answer that is not a lie.

**`workdir_retain = N` caps how many are kept.** Pruning is derived from the
template, so the config requires the varying element to be the LAST element:
`~/src/{{harness.name}}` plus a retain would otherwise point pruning at
directories that predate harness. Even then a directory is removed only when it
is a real directory (not a symlink), matches the template's literal shape, is
not the run in flight, and carries the daemon's own `.harness-run-dir` mark —
the daemon's claim that it created the directory for a run. Anything unmarked
survives, including directories an earlier harness version created.

**Every run gets an agent data directory.** `{{run.datadir}}` names a fresh
`0700` directory inside the run's workdir, and `HARNESS_RUN_DATADIR` names it
to the hooks. Under a shared workdir it is nested by run id
(`<workdir>/.harness-run/<run.id>`) and bounded by the harness's existing
`keep_runs`, so the two retention settings cannot disagree about how much
history exists. It is deliberately NOT exported to the agent process as
`XDG_DATA_HOME` or the like: an agent's own configuration and credentials are
not the daemon's to relocate, and a run that loses them fails in a way that
looks like a broken agent rather than a clean run.

## Consequences

**Positive.** A run starts from a directory nothing earlier left behind; a run
that did not do its work stops being recorded as a success; a clone, a mount
check or a workspace reset becomes config instead of a wrapper script that
throws away the transcript observer; per-run disk usage becomes a number with a
name instead of whatever accumulates.

**Negative.** Two more keys per harness, and a hook that fails is a new reason
a run is `failed`, so an operator's flaky setup script now reads as a failed
run — which is the point, but it needs the reason in the record (it is there:
`pre_run`, `post_run`, `workdir`). Retention is bounded by the mark, so a
pre-existing directory tree is never pruned and must be cleaned by hand once.

**Neutral.** `pre_run` runs on every start of the run, including a crash
restart of a one-shot, so a hook must be idempotent — the same contract
`health_cmd` inherits.

## Open questions for the maintainer

1. Is `pre_run`/`post_run` the right spelling next to `[notify]`'s
   `command`, or should hooks move to one `[hooks]` table shared with notify?
2. Should `post_run` also demote a `timed_out` run whose check passes, or is
   only a `success` ever negotiable, as implemented here?
3. `success_check` (#493) is the designed post-run verifier. It should own the
   verdict-with-a-report and the retry semantics; the integration point here is
   that `post_run` runs while the record is still open, so #493 can annotate
   the same record. Does it want the same slot or a sibling key?

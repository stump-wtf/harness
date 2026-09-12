---
status: draft
date: 2026-07-27
implements: [ADR-0011, ADR-0018]
requires: [SPEC-0002, SPEC-0004, SPEC-0005]
---

# SPEC-0006: Agent Adapters (prompt source, skill paths, projection, trajectory discovery)

> **Partially implemented.** Only trajectory discovery has shipped; skill paths and projection are design-stage. See harness issue #3.

## Overview

A per-harness adapter that answers three questions about the tool running inside
it — where its skills come from, where they must be placed, and where it writes
its trajectory — plus the configured-path scheme and ordered merge that feed it.
See **ADR-0011**.

The adapter is selected by an `agent` field that mirrors the existing `backend`
precedent: the daemon holds a registry it dispatches on without understanding
any entry. Skills are merged from an ordered set of configured roots and
**copied** into the adapter's target directory immediately before exec.
Trajectories are located by the adapter and exposed **read-only** through the
SPEC-0005 facade. Project roots and provenance are reused from SPEC-0004 rather
than reinvented.

The same adapter supplies the agent argv for a one-shot run, whose instruction
comes from either a literal `prompt` or a `prompt_file` path read at spawn. See
**ADR-0018**.

## Requirements

### Requirement: Adapter Selection

Each harness SHALL resolve to exactly one adapter. The harness schema SHALL
accept an optional `agent` key naming the adapter; when absent, the daemon SHALL
infer it from `cmd`. When inference finds no match, the harness SHALL resolve to
the `generic` adapter. Naming an adapter that does not exist SHALL fail config
validation with an error identifying the harness and the unknown adapter.

#### Scenario: Adapter inferred from cmd

- **WHEN** a harness declares `cmd = "claude"` with no `agent` key
- **THEN** it resolves to the `claude-code` adapter

#### Scenario: Explicit agent overrides inference

- **WHEN** a harness declares `cmd = "my-wrapper"` and `agent = "claude-code"`
- **THEN** it resolves to the `claude-code` adapter despite the unrecognized
  command

#### Scenario: Unknown tool degrades to generic

- **WHEN** a harness declares a `cmd` matching no adapter and sets no `agent`
- **THEN** it resolves to `generic`, starts normally, receives no projection,
  and reports no native trajectory (its trajectory source is the SPEC-0002
  scrollback fallback)

#### Scenario: Unknown adapter name is rejected

- **WHEN** a harness sets `agent = "nonexistent"`
- **THEN** config validation fails naming the harness and the unknown adapter,
  and the daemon keeps its last-good configuration

### Requirement: Prompt Source

A harness SHALL declare an agent one-shot with either a `prompt` string or a
`prompt_file` path, and the two SHALL be mutually exclusive. Declaring both
SHALL fail config validation with a located error naming the harness. Either
key SHALL satisfy the prompt predicate the `model`, `auto_accept`, `max_turns`,
`quiet` and `schedule` keys are validated against, and either SHALL be mutually
exclusive with `args`.

`prompt` SHALL be stored verbatim: never placeholder-expanded, never
`{workdir}`-substituted, and passed to the adapter's prompt synthesis as-is.

`prompt_file` SHALL be resolved at parse time by the existing config path rules
— a leading `~` expands to the home directory, and a relative path resolves
against the directory holding the config file, or against the project root in a
project `harness.toml`. The **resolved path** SHALL be stored on the harness;
the file's contents MUST NOT be stored on the harness, placed on the wire, or
written to the persisted state file, so that config writers round-trip the path
and never inline the document.

Config load SHALL verify that a `prompt_file` names an existing, readable,
non-empty file, failing with a located error otherwise. This is deliberately
stricter than `env_file`, where a missing file is tolerated: a harness with no
prompt has nothing to run.

The daemon SHALL read `prompt_file` at spawn time, immediately before exec, and
use its contents as the prompt for that run. A read failure at spawn SHALL fail
the start with an error naming the harness and the path; the daemon MUST NOT
launch the agent with an empty prompt. Because the read happens per spawn,
editing the referenced file SHALL change the next run without a config reload.

#### Scenario: Prompt file supplies the agent instruction

- **WHEN** a harness declares `prompt_file` naming a readable file and starts
- **THEN** the synthesized agent argv carries that file's contents as the
  prompt, and the harness's stored configuration still carries the path

#### Scenario: Prompt and prompt file are mutually exclusive

- **WHEN** a harness declares both `prompt` and `prompt_file`
- **THEN** config validation fails with a located error naming the harness, and
  the daemon keeps its last-good configuration

#### Scenario: Missing prompt file fails the load

- **WHEN** a harness declares a `prompt_file` that does not resolve to an
  existing, readable, non-empty file
- **THEN** config validation fails with a located error naming the harness and
  the resolved path

#### Scenario: Prompt file satisfies the prompt-dependent keys

- **WHEN** a harness declares `prompt_file` together with `schedule`, `model`,
  `auto_accept`, `max_turns`, or `quiet`, and no `prompt`
- **THEN** the config parses successfully and the harness defaults to
  `restart = "no"` like any other one-shot

#### Scenario: Relative prompt file resolves against its config

- **WHEN** a project `harness.toml` declares `prompt_file = "./prompts/sweep.md"`
- **THEN** the path resolves against the project root

#### Scenario: Writers round-trip the path, not the contents

- **WHEN** a harness declaring `prompt_file` is edited through a config writer
  and an unrelated field is changed
- **THEN** the written file still declares `prompt_file` as a path, and the
  file's contents appear nowhere in the written configuration

#### Scenario: Prompt file deleted between load and spawn

- **WHEN** a `prompt_file` that validated at load is unreadable at spawn
- **THEN** the start fails with an error naming the harness and the path, and no
  agent is launched with an empty prompt

#### Scenario: Edited prompt file takes effect without a reload

- **WHEN** the contents of a referenced `prompt_file` change and the harness
  starts again
- **THEN** the new contents are used, with no config reload in between

### Requirement: Skill Path Configuration

The harness schema SHALL accept an optional `skill_paths` list of directories
and an optional `use_default_skill_paths` boolean defaulting to `true`.
`skill_paths` SHALL be **additive** to the adapter's default roots unless
`use_default_skill_paths` is `false`, in which case adapter defaults SHALL be
omitted entirely. Both keys SHALL be legal in the global configuration file and
in a project `harness.toml`; unlike `[server]` and `[profile.*]`, a project file
carrying them MUST NOT be rejected. Relative paths in a project file SHALL
resolve against the project root.

#### Scenario: Configured paths add to defaults

- **WHEN** a harness sets `skill_paths = ["~/work/team-skills"]`
- **THEN** skills resolve from the adapter's default roots *and* that directory

#### Scenario: Defaults can be dropped

- **WHEN** a harness sets `use_default_skill_paths = false` alongside
  `skill_paths`
- **THEN** only the configured paths contribute and the adapter's defaults are
  ignored

#### Scenario: Project files may carry skill paths

- **WHEN** a project `harness.toml` declares `skill_paths = ["./skills"]`
- **THEN** the file parses successfully and the path resolves against the
  project root

### Requirement: Ordered Merge and Shadowing

Skills SHALL be merged from all contributing roots in the fixed precedence
order: adapter defaults (lowest), global `skill_paths`, project `skill_paths`,
then project-local directories (highest). When two roots supply a skill of the
same name, the higher-precedence copy SHALL win. Shadowed copies MUST remain
enumerable through a diagnostic surface so the winning copy is attributable.

#### Scenario: Nearer root wins a name collision

- **WHEN** a skill named `playbook` exists in both a global path and a
  project path
- **THEN** the project copy is projected and the global copy is recorded as
  shadowed

#### Scenario: Shadowed copies are attributable

- **WHEN** the operator inspects the resolved skill set for a harness
- **THEN** each name reports the winning source path and every shadowed source
  path

### Requirement: Spawn-Time Projection

Before executing a harness's command, the daemon SHALL materialize the merged
skill set into the adapter's target directory. The target directory MUST NOT
itself be treated as a contributing source root; the merged set is drawn from
the remaining roots. Projection SHALL copy file contents; it MUST NOT create
symbolic links into a configured source root, so a harness writing into its
own skill directory cannot mutate a shared source. The learned skill tier
(SPEC-0007) SHALL be excluded from projection unconditionally. Projection
SHALL occur on every start and restart. When an adapter declares no target
directory, projection SHALL be skipped and the harness SHALL start normally.

#### Scenario: Merged set lands before exec

- **WHEN** a harness with resolvable skills starts
- **THEN** the adapter's target directory contains the merged set before the
  command executes

#### Scenario: Projection is by copy, not link

- **WHEN** a running harness modifies a file inside its projected skill
  directory
- **THEN** the corresponding file in the configured source root is unchanged

#### Scenario: Generic adapter skips projection

- **WHEN** a harness resolving to `generic` starts
- **THEN** no projection occurs and the harness runs normally

### Requirement: Trajectory Discovery

Each adapter SHALL report the location of its tool's native trajectory for a
given harness, or report that none exists. When an adapter reports no
trajectory, the daemon SHALL fall back to the SPEC-0002 scrollback record
(ADR-0007). The
daemon SHALL expose trajectories through the SPEC-0005 facade as read-only
operations (`list_trajectories`, `get_trajectory`) and MUST NOT write to, alter,
or delete a tool's trajectory.

**Implementation note:** Trajectory parsing is delegated to
[agent-trace](https://github.com/stump-wtf/agent-trace) (`tail` package),
which ships per-agent JSONL parsers for Claude Code, Codex, Crush, OpenCode,
and Pi. Harness maps its adapter identities onto agent-trace's adapters;
`generic` reports no native trajectory regardless of agent-trace's coverage.
The `classify` package provides structured action taxonomy for downstream
consumers (SPEC-0007 distillation, OTel export).

#### Scenario: Native transcript is located

- **WHEN** a `claude-code` harness has written a session transcript
- **THEN** `list_trajectories` reports it for that harness and `get_trajectory`
  returns its contents

#### Scenario: Fallback to scrollback

- **WHEN** a harness's adapter reports no native trajectory path
- **THEN** `list_trajectories` reports the scrollback record as the trajectory
  source for that harness

#### Scenario: Trajectories are never mutated

- **WHEN** any trajectory operation is invoked
- **THEN** the underlying transcript is read only, and no daemon code path
  writes to it

### Requirement: Run Correlation

A tool's trajectory store is machine-global: it holds sessions from every
harness running that tool, from sibling harnesses sharing a working directory,
and from the operator's own interactive runs. The daemon SHALL attribute a
native trajectory session to a run of a harness only when **all** of the
following hold, and SHALL NOT attribute it otherwise (issue #89):

1. **Adapter and store.** The session was discovered in a store the harness's
   own tool instance writes to:
   - `crush` — when the harness's configuration names a data directory, that
     store **alone**: `--data-dir`/`-D` in its `args`, else
     `options.data_directory` in the crush config the instance loads (an
     absolute path as-is, a relative one against the workdir — crush's own
     rule). A named store belongs to one harness, so the shared stores below
     SHALL NOT be consulted alongside it: both are reachable by every harness
     in the working directory, which is the ambiguity a named store exists to
     resolve.

     Otherwise the store is inferred and both are read —
     `<workdir>/.crush/crush.db`, and the `projects.json` registry under the
     harness's `CRUSH_GLOBAL_DATA` (else `XDG_DATA_HOME/crush`, else
     `~/.local/share/crush` — crush's own resolution order). The project store
     is read directly because the registry is not reliable alone: crush
     rewrites it without a cross-process lock, and it has been observed corrupt
     and weeks stale on a host running several instances.
   - `claude-code` — `$CLAUDE_CONFIG_DIR/projects` (default `~/.claude/projects`).
   - `codex` — `$CODEX_HOME/sessions` (default `~/.codex/sessions`).
   - `generic` — none; nothing is attributed to a generic harness.

   These keys SHALL be resolved from the harness's `env_file` layered over the
   daemon's environment — what the process actually sees — and no other
   `env_file` value SHALL enter the correlation path (ADR-0008). From a
   harness's `args`, only the flags that name a store SHALL be read; no other
   argument enters that path either.
2. **Working directory.** The session's recorded cwd equals the harness's
   resolved workdir exactly. A subdirectory is a different project to every
   supported tool. A harness with no workdir has nothing attributed.
3. **Time window.** The session started within `[start − 2s, end + 2s]` of the
   run (an in-flight run's end is now). The two-second slack covers
   second-truncated storage, nothing more: every timestamp involved comes from
   the same machine's clock.
4. **No other claimant.** No other harness shares the workdir (compared after
   resolving symlinks; a harness with no workdir shares the daemon's), could
   have written that kind of session (the same adapter, or `generic`, which may
   run any tool), and either has a known run covering the session's start or
   has no record of its runs that far back.

   A harness whose store is named, and differs from the inspected harness's
   named store, SHALL NOT be a claimant: two harnesses each keeping their
   sessions somewhere of their own cannot have written each other's, however
   much else they share. This is positive evidence only — an inferred store
   rules nothing out, because a registry may point anywhere — so a shared
   working directory remains decisive wherever the configuration does not say
   better. A consumer attributing a session it discovered (rather than
   inspecting one run) MAY likewise use the store the session came from, which
   its own record names, to the same effect and under the same limit.

   A session with any other claimant SHALL be excluded for every claimant, and
   the exclusion SHALL be reported — session, start time, claimants — rather
   than dropped silently.

**Run windows.** The inspected run's window SHALL come from the supervisor's
own record (`last_started`, and `last_exit_at` when it closes that run; both
persisted in the state file and restored across a daemon restart) or from a
window the caller supplies — the seam a run selector resolves a run record
into. Only when the supervisor has no record at all MAY the window be recovered
from the lifecycle lines in the harness's durable log. Other harnesses' windows
SHALL include their supervisor record plus every run their durable logs record.
Lifecycle lines share a file with program output, so a log-derived window SHALL
only ever add claimants; it never widens what an inspected run is credited with.

A spawn that fails is still the latest run and SHALL record a start, so its exit
never closes an earlier run's window. A run with no recorded end that began
before the current daemon started SHALL be closed at that daemon's start: a
harness is not supervised past the daemon that spawned it, and an open window
would credit it with every later session in its workdir. A consumer that holds
only each harness's latest run — the chatroom, from `list` — SHALL treat a
harness's history before that run, or before an exit with no recorded start, as
unknown, so a session older than a sibling's latest start is credited to no one.

**Event scope.** Only the events of attributed sessions whose own timestamps fall
inside the window are reported, so a session that continues past its run — for
example one a later run resumes — reports none of that later activity under the
run that started it. The later run is not credited with the resumed session
either, because the session started outside its window; that activity is not
shown by this view.

**Threat model and residual gaps.** No supported transcript records the process
that wrote it (the `HARNESS_RUN_ID` environment-stamping idea has nothing to
match against), so correlation is a heuristic, and it fails closed: an
ambiguous session is hidden, because under-reporting is recoverable and putting
another agent's — or the operator's — transcript on a harness is not. It cannot
distinguish:

- a session started by hand, or by any unsupervised instance of the tool, in the
  harness's workdir during the run;
- a peer harness's run older than that peer's durable-log retention, which
  therefore adds no claimant;
- a store relocated by a project config above the working directory — crush
  walks up to find one and correlation does not — and not recorded in the
  instance's registry;
- a harness since removed from the configuration or renamed, whose past runs
  therefore add no claimant;
- a peer's log-derived window after the daemon's time zone changes, or in the
  repeated hour when clocks fall back: lifecycle stamps are zoneless local wall
  time, read in the daemon's current zone.

Attribution is not an exposure control. Harvest Opt-In below remains the gate for
the facade; this requirement decides what `harness logs` and the chatroom credit
to a harness.

#### Scenario: A session started during the run is attributed

- **WHEN** a session whose cwd is the harness's workdir starts after the run spawned
- **THEN** it is attributed to that run

#### Scenario: Sessions outside the run or the workdir are not attributed

- **WHEN** a session in the workdir started before the run, or a session started
  during the run is in a different working directory
- **THEN** it is not attributed

#### Scenario: Overlapping runs in a shared workdir exclude the session

- **WHEN** two harnesses of the same adapter share a workdir and a store, and a
  session starts while both are running (for example two crush agents with
  different `CRUSH_GLOBAL_DATA` whose registries point at the same project store)
- **THEN** neither harness is credited with it, and each reports the exclusion
  naming both claimants

#### Scenario: Named stores separate harnesses that share a workdir

- **WHEN** several crush harnesses run in one working directory and each is
  configured with a data directory of its own (`--data-dir`, or
  `options.data_directory`)
- **THEN** each run is attributed the sessions from its own store, and no
  sibling is a claimant — even where their runs overlap and they share one
  registry

#### Scenario: A named store is not widened by the shared ones

- **WHEN** a harness names a data directory, and the project store
  `<workdir>/.crush` or the instance's registry also holds sessions for that
  working directory
- **THEN** only the named store is read, so a sibling's sessions are never
  offered to it

#### Scenario: An inferred store rules nothing out

- **WHEN** one harness names a data directory and a sibling sharing its
  working directory names none
- **THEN** the sibling remains a possible author, because its registry may
  point anywhere, and a session either could have written is still excluded

#### Scenario: Staggered runs in a shared workdir are separated by time

- **WHEN** scheduled harnesses share a workdir and store but their runs do not
  overlap
- **THEN** each run is attributed only the sessions that started inside it

#### Scenario: A relocated registry is followed

- **WHEN** a harness's `env_file` sets `CRUSH_GLOBAL_DATA`
- **THEN** discovery reads that instance's registry, not the default one

#### Scenario: Post-mortem after a daemon restart

- **WHEN** a harness's last run ended and the daemon has since restarted
- **THEN** the run's window is restored from the state file and its sessions
  are still attributed

#### Scenario: A run the daemon died with does not stay open

- **WHEN** a harness's last run has no recorded end and began before the
  current daemon started
- **THEN** its window closes at the daemon's start, and the reply says so

#### Scenario: A latest-run view does not guess across a restarted sibling

- **WHEN** a consumer holding only each harness's latest run sees a session that
  started before a same-workdir sibling's latest run began
- **THEN** the session is credited to no harness

### Requirement: Harvest Opt-In

Trajectory exposure SHALL be opt-in per harness via a configuration key
defaulting to disabled. A harness that has not opted in MUST NOT have its
trajectory listed or returned by any facade operation, because a trajectory may
contain secrets the harnessed program printed itself (ADR-0008).

#### Scenario: Opt-out is the default

- **WHEN** a harness has not enabled trajectory harvesting
- **THEN** `list_trajectories` omits it entirely and `get_trajectory` refuses
  with a structured error

#### Scenario: Opt-in exposes the trajectory

- **WHEN** a harness enables trajectory harvesting and the config is reloaded
- **THEN** its trajectory appears in `list_trajectories` without restarting the
  harness

### Requirement: Error Handling Standards

All error-producing operations MUST follow structured error handling:

- Errors MUST be wrapped with contextual information at each layer boundary
  (e.g., "adapter claude-code: project skills: read ./skills: permission
  denied")
- Sentinel errors MUST be defined for domain-specific failure modes callers need
  to distinguish programmatically — at minimum: unknown adapter, unreadable
  skill root, and projection target not writable
- Silent error swallowing MUST NOT occur — every error MUST be returned to the
  caller, logged with sufficient context, or explicitly handled with a
  documented reason for suppression
- Structured logging MUST be used for error reporting (key-value pairs, not
  string interpolation)

#### Scenario: An unreadable skill root does not block startup

- **WHEN** one configured skill root cannot be read at spawn
- **THEN** the harness still starts with the remaining roots merged, and a
  warning names the unreadable path and its cause

#### Scenario: A failed projection is attributable

- **WHEN** projection cannot write to the adapter's target directory
- **THEN** the resulting error identifies the adapter, the target path, and the
  underlying cause

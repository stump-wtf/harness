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

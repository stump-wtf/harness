---
status: draft
date: 2026-07-23
implements: [ADR-0009]
requires: [SPEC-0002, SPEC-0003]
---

# SPEC-0004: Project Compose (`harness up` / `down`)

## Overview

A per-project, repo-root `harness.toml` plus a Compose-style verb vocabulary
(`up`, `down`, `ps`, `logs`, `start`, `stop`, `restart`) that brings a project's
harnesses up under the running daemon without touching the global config. See
**ADR-0009**. The project file reuses the ADR-0006 `[harness.*]` schema; the
daemon registers a project's harnesses under a `<project>/<harness>` namespace as
an **ephemeral** set that lives and dies with `up`/`down`. Lifecycle, PTYs, and
scrollback remain the daemon's job (SPEC-0003); the new work here is discovery,
the `[project]` header, namespacing, two new control operations, and the verb
semantics.

## Requirements

### Requirement: Project File Discovery

`harness up` (and the other project verbs when invoked without an explicit path)
SHALL locate the project file by walking **upward** from the current working
directory, testing each ancestor directory for a `harness.toml`, and stopping at
the first one found. The directory containing that file is the **project root**.
If no `harness.toml` is found before reaching the filesystem root or the user's
home directory, the command SHALL fail with a clear "no harness.toml found"
error. The discovery walk MUST NOT adopt the daemon's own global config file
(`config.DefaultPath()`, i.e. `$XDG_CONFIG_HOME/harness/harness.toml` or the
`~/.config` fallback) as a project file, even if `cwd` is inside that directory.

#### Scenario: Found in an ancestor directory

- **WHEN** `harness up` runs in `~/src/reduit/internal/foo` and a `harness.toml`
  exists at `~/src/reduit/harness.toml`
- **THEN** the project root resolves to `~/src/reduit` and that file is used

#### Scenario: No project file present

- **WHEN** `harness up` runs in a directory tree containing no `harness.toml`
- **THEN** the command exits non-zero with a message telling the user no
  `harness.toml` was found and nothing is sent to the daemon

#### Scenario: Global config is never treated as a project

- **WHEN** discovery walks through `$XDG_CONFIG_HOME/harness/`
- **THEN** the global `harness.toml` there is skipped and not registered as a
  project

### Requirement: Project File Schema

A project `harness.toml` SHALL reuse the ADR-0006 `[harness.*]` table schema
verbatim (`cmd`, `args`, `workdir`, `env_file`, `restart_delay`, `backend`,
`description`, `enabled`) and MAY include an optional `[project]` table whose
`name` key sets the project name. Relative `workdir` values SHALL resolve
against the project root, not against the daemon's working directory. A project
file MUST NOT contain `[server]` or `[profile.*]` tables; the parser SHALL reject
such a file with a validation error identifying the offending table, because
those are global-only concerns.

#### Scenario: Reuses the harness schema

- **WHEN** a project file defines `[harness.agent]` with `cmd` and `args`
- **THEN** it parses into the same `core` harness type a global `[harness.*]`
  table produces, with identical field meanings

#### Scenario: Relative workdir resolves against project root

- **WHEN** a project harness sets `workdir = "."` and the project root is
  `~/src/reduit`
- **THEN** the harness runs with working directory `~/src/reduit`

#### Scenario: Server or profile table rejected

- **WHEN** a project `harness.toml` contains a `[server]` or any `[profile.*]`
  table
- **THEN** parsing fails with a validation error naming that table and the
  project is not registered

### Requirement: Project Naming And Namespacing

Each project SHALL have a name: the value of `[project].name` if present,
otherwise the sanitized basename of the project-root directory. Every harness a
project registers SHALL be exposed daemon-wide under the name
`<project>/<harness>` (e.g. `reduit/agent`). Project names SHALL be validated to
not collide with an existing bare (global) harness name at registration time.
Two distinct projects that each define a harness of the same local name SHALL be
able to be registered simultaneously because their fully-qualified names differ
by project prefix.

#### Scenario: Default name from directory

- **WHEN** a project at `~/src/reduit` has no `[project].name`
- **THEN** its project name is `reduit` and its harnesses register as
  `reduit/<harness>`

#### Scenario: Two projects, same local harness name

- **WHEN** project `reduit` and project `spotter` each define `[harness.agent]`
  and both are brought up
- **THEN** the daemon supervises `reduit/agent` and `spotter/agent` concurrently
  without collision

#### Scenario: Name collides with a global harness

- **WHEN** a project name would shadow an existing bare global harness name
- **THEN** `up` fails with a collision error and registers nothing

### Requirement: Bring Up (`harness up`)

`harness up` SHALL require a running daemon, parse the discovered project file,
and send a `project_up` control request (SPEC-0002) carrying the project name and
its harness definitions. The daemon SHALL register the project's harnesses under
the project namespace and start each one (transitioning it to `starting` per
SPEC-0003). `up` SHALL run **detached**: after issuing the request it SHALL print
a one-shot status table of the project's harnesses and their states and return to
the shell. `up` SHALL be idempotent — re-running it on an already-registered
project SHALL reconcile: newly-added harnesses are registered and started,
removed harnesses are deregistered and stopped, and changed definitions are
flagged to apply on next restart per SPEC-0003 (never silently bounced).

#### Scenario: First up in a project

- **WHEN** `harness up` runs in a project with agent harnesses `agent` and
  `reviewer`
- **THEN** the daemon registers `reduit/agent` and `reduit/reviewer`, starts both,
  and the command prints their states then exits

#### Scenario: Detached, not attached

- **WHEN** `harness up` completes
- **THEN** control returns to the shell with the harnesses running in the
  background under the daemon (viewing is via `harness` TUI or `harness attach`)

#### Scenario: Re-up reconciles

- **WHEN** the project file gains a new harness and `harness up` is run again
- **THEN** the new harness is registered and started while the others are
  untouched, and no duplicate registration occurs

#### Scenario: Daemon not running

- **WHEN** `harness up` runs and no daemon is reachable on the socket
- **THEN** the command fails with the same daemon-unreachable error as other
  client verbs and registers nothing

### Requirement: Tear Down (`harness down`)

`harness down` SHALL send a `project_down` control request for the discovered
project. The daemon SHALL stop every harness registered under that project's
namespace and then **deregister** them, so the daemon retains no record of the
project afterward. `down` SHALL be destructive by design — distinct from
non-destructive profile hopping (ADR-0006) — and SHALL leave the global config
file byte-for-byte unchanged.

#### Scenario: Down stops and forgets

- **WHEN** `harness down` runs for project `reduit`
- **THEN** `reduit/agent` and `reduit/reviewer` are stopped and removed from the
  daemon's registry, and a subsequent `harness ps` for that project lists nothing

#### Scenario: Global config untouched

- **WHEN** `harness up` then `harness down` are run for a project
- **THEN** `$XDG_CONFIG_HOME/harness/harness.toml` is byte-identical to its
  contents before `up`

### Requirement: Project-Scoped Verbs

`harness ps`, `harness logs [name]`, `harness start [name]`, `harness stop
[name]`, and `harness restart [name]` SHALL, when run inside a project, operate
on that project's harnesses via the existing SPEC-0002 control operations
filtered to the project namespace. `ps` SHALL list only the project's harnesses
and their states. `start`, `stop`, and `restart` SHALL be non-destructive:
deregistration SHALL remain exclusive to `down`. A bare `name` argument to these
verbs SHALL refer to the project-local name and be resolved to
`<project>/<name>`.

#### Scenario: ps is project-scoped

- **WHEN** `harness ps` runs in project `reduit` while other global harnesses and
  other projects are also registered
- **THEN** the output lists only `reduit/*` harnesses

#### Scenario: stop does not deregister

- **WHEN** `harness stop agent` runs in project `reduit`
- **THEN** `reduit/agent` transitions to `stopped` but remains registered, so
  `harness start agent` can bring it back without re-running `up`

#### Scenario: Local name resolution

- **WHEN** `harness restart agent` runs in project `reduit`
- **THEN** the daemon restarts `reduit/agent`

### Requirement: Project Control Operations

The daemon control plane (SPEC-0002) SHALL add two operations: `project_up
{ name, harnesses }`, which registers and starts a project's harnesses under the
project namespace, and `project_down { name }`, which stops and deregisters
them. Both SHALL return structured `ERROR` frames on failure (unknown project for
`down`, name collision or invalid definition for `up`) with a machine code and a
human message the CLI can surface verbatim. `project_up` SHALL be idempotent in
the reconcile sense defined by the Bring Up requirement.

#### Scenario: project_up round-trip

- **WHEN** a client sends `project_up { name: "reduit", harnesses: [...] }`
- **THEN** the daemon registers each harness as `reduit/<harness>`, starts them,
  and replies success

#### Scenario: project_down on unknown project

- **WHEN** a client sends `project_down { name: "nope" }` for a project the
  daemon has no record of
- **THEN** the daemon replies with a structured `ERROR` and changes no state

### Requirement: Error Handling Standards

All error-producing operations (file discovery, TOML parsing, project
registration, and control-plane exchange) MUST follow structured error handling:

- Errors MUST be wrapped with contextual information at each layer boundary
  (e.g., "harness up: parse ./harness.toml: unknown table [server] at line 12").
- Sentinel errors MUST be defined for domain-specific failure modes callers need
  to distinguish programmatically (no project file found, project-name collision,
  unknown project on `down`, forbidden `[server]`/`[profile.*]` table).
- Silent error swallowing MUST NOT occur — every error MUST be returned to the
  caller, logged with sufficient context, or explicitly handled with a
  documented reason.
- Structured logging MUST be used for error reporting (key-value pairs, not
  string interpolation).
- TOML validation errors SHALL carry a source line so the CLI/TUI can point the
  user at the offending table, consistent with the ADR-0006 reload path.

#### Scenario: Parse error names the table and line

- **WHEN** a project file contains a forbidden `[profile.default]` table on
  line 12
- **THEN** the surfaced error identifies both the table and the source line, and
  no partial registration occurs

#### Scenario: No silent swallow on registration failure

- **WHEN** `project_up` fails partway (e.g. a name collision on the third
  harness)
- **THEN** the daemon reports a structured error and does not leave a partially
  registered project behind

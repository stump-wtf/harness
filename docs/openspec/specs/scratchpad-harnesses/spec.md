---
status: draft
date: 2026-08-19
implements: [ADR-0017]
requires: [SPEC-0002, SPEC-0003, SPEC-0004]
---

# SPEC-0011: Ephemeral Scratchpad Harnesses (`harness run`)

## Overview

`harness run` creates an ad-hoc, throwaway supervised session — the
screen/tmux/shpool replacement — with no config file and no name ceremony: `harness
run claude opus-5` mints `claude-opus-5-x4yx`, starts it under the daemon, and
attaches to it, exactly like `tmux new-session`. Scratchpads are the
deliberately ephemeral counterpart to projects (SPEC-0004): they are never
persisted, vanish on daemon restart, and are torn down with `harness rm`. See
**ADR-0017**.

## Requirements

### Requirement: Scratchpad Creation (`harness run`)

`harness run [--workdir DIR] [--name SLUG] [--kind KIND] [--detach] ARG
[ARG...]` SHALL require a running daemon, map its positionals onto a harness
definition (first positional selects the harness kind per ADR-0011's enum —
`crush`, `claude-code`, `codex`, `generic` — through a small alias table
(`claude` → `claude-code`; the word people type, not the enum value) and
remaining positionals are its args; a first positional that is not a known
kind or alias SHALL be treated as a `generic` command with all positionals as
its argv, overridable by `--kind`), and send a `scratch_run` control request
(SPEC-0002) carrying the definition. The daemon SHALL register the
supervisor, start it, and reply with the minted name and fresh state; the
client SHALL print the name. `--workdir` SHALL resolve relative to the
caller's cwd (the client expands it to an absolute path before sending).

#### Scenario: Agent scratchpad

- **WHEN** `harness run claude opus-5` runs
- **THEN** the daemon registers and starts a scratchpad named
  `claude-opus-5-<suffix>` (4 random base36 characters) with `scratch`
  provenance, and the command prints the name

#### Scenario: Project named `scratch` refused

- **WHEN** `harness project up` targets a project whose name is `scratch`
  (e.g. a project rooted at `~/src/scratch`)
- **THEN** it is refused with the reserved-name validation error, because
  `scratch` is the provenance sentinel and must never name a real project

#### Scenario: Generic command scratchpad

- **WHEN** `harness run htop` runs
- **THEN** the scratchpad is a `generic` harness running `htop` with no args

#### Scenario: Kind collision disambiguated

- **WHEN** `harness run --kind generic crush` runs
- **THEN** the scratchpad runs the literal command `crush` rather than the
  crush agent

#### Scenario: Daemon not running

- **WHEN** `harness run` runs with no daemon on the socket
- **THEN** it fails with the standard daemon-unreachable error and nothing is
  registered

### Requirement: Attach By Default

After a successful `scratch_run`, the client SHALL attach to the minted
scratchpad exactly as `harness attach NAME` would (Requirement "Supervisor
Parity"), the same gesture as `tmux new-session` dropping the caller straight
into the new session — the entire point of a throwaway pad is to start typing
into it immediately, not to run a second command to get there. This SHALL be
skipped, leaving the scratchpad running and the client returning immediately
after printing the name (the pre-attach behavior), when: `--detach` is given
(the `tmux new-session -d` equivalent); `--json` is given (a machine
consumer has no terminal to attach to); or stdout is not a terminal (piped or
redirected — there is nothing to attach). Detecting a non-terminal stdout
SHALL use the same per-writer TTY check the animated `--all` lifecycle view
uses (SPEC-0002), so the two verbs agree on what counts as interactive.

#### Scenario: Interactive run attaches automatically

- **WHEN** `harness run claude opus-5` runs at an interactive terminal with
  neither `--detach` nor `--json`
- **THEN** the client prints the minted name and then attaches to it, exactly
  as a subsequent `harness attach claude-opus-5-<suffix>` would

#### Scenario: `--detach` leaves it running in the background

- **WHEN** `harness run --detach claude opus-5` runs at an interactive
  terminal
- **THEN** the client prints the minted name and exits without attaching; the
  scratchpad keeps running and is reachable via `harness attach` or `harness
  logs`

#### Scenario: `--json` never attaches

- **WHEN** `harness run --json claude opus-5` runs, interactive terminal or
  not
- **THEN** the client prints the `scratch_run` reply as JSON and exits
  without attaching

#### Scenario: Piped stdout never attaches

- **WHEN** `harness run claude opus-5` runs with stdout redirected to a file
  or pipe
- **THEN** the client prints the minted name and exits without attaching,
  identically to `--detach`

### Requirement: Name Minting

The daemon SHALL mint each scratchpad name as `<slug>-<suffix>`: the slug is
the sanitized (lowercase, non-alphanumerics to `-`, collapsed, trimmed)
invocation — kind or command base plus arg words — capped at a sane length,
overridable by `--name`; the suffix is 4 random base36 characters. Minting
SHALL retry on collision with any registered name and SHALL happen under the
registry lock so two concurrent `run` invocations can never mint the same name.

#### Scenario: No collision between identical invocations

- **WHEN** `harness run claude opus-5` is issued twice in quick succession
- **THEN** two distinct scratchpads exist with distinct names

#### Scenario: Explicit slug still suffixed

- **WHEN** `harness run --name mypad claude` runs
- **THEN** the name is `mypad-<suffix>`

### Requirement: Ephemerality (No Persistence)

A scratchpad registration SHALL NOT be persisted: state.json SHALL contain no
scratchpad definitions or runtime entries at any point in a scratchpad's life,
and on daemon start no scratchpad SHALL be restored. This is the invariant that
separates scratchpads from projects (SPEC-0004 REQ "Registration Persistence")
and SHALL hold under the same save/debounce path as every other state write.

#### Scenario: state.json stays clean

- **WHEN** a scratchpad is created, stopped, started, and removed
- **THEN** state.json never contains its name at any observable point

#### Scenario: Daemon restart discards scratchpads

- **WHEN** the daemon restarts while scratchpads are registered
- **THEN** none of them are re-registered afterward

### Requirement: Supervisor Parity

A scratchpad SHALL be a full supervisor: lifecycle ops, PTY, attach, scrollback,
logs, and list/describe projection SHALL work identically to a configured
harness, with provenance projected as `scratch` so clients can distinguish it.
The default restart policy for a scratchpad SHALL be `no` (session semantics):
an exited scratchpad SHALL remain registered with its exit state until removed,
not respawned.

#### Scenario: Attach to a scratchpad

- **WHEN** `harness attach claude-opus-5-x4yx` runs
- **THEN** it attaches to the scratchpad's terminal exactly as to any harness

#### Scenario: Exited scratchpad stays inspectable

- **WHEN** a scratchpad's process exits
- **THEN** `harness list` still shows it with its exit state until `harness rm`
  removes it

### Requirement: Teardown (`harness rm`)

`harness rm NAME` (SPEC-0004 REQ "Remove") SHALL accept a scratchpad name: the
daemon stops it, deregisters it, releases its attach and log resources, and —
being never persisted — there is no persisted registration to delete. Unknown
and global-config names SHALL be refused with `not_removable` exactly as for
project harnesses.

#### Scenario: Remove a scratchpad

- **WHEN** `harness rm claude-opus-5-x4yx` runs
- **THEN** the scratchpad is stopped, deregistered, and absent from `list`

### Requirement: Control Operation

The daemon control plane (SPEC-0002) SHALL add `scratch_run { harness, args,
workdir, name? }`: it validates the definition (same rules as a project_up
payload; an empty argv is invalid), mints the name, registers with `scratch`
provenance, starts, and replies `{ name, harness_info }`. Failures SHALL be
structured `ERROR` frames (`invalid_project`-class validation code reused for
an invalid definition).

#### Scenario: scratch_run round-trip

- **WHEN** a client sends `scratch_run { harness: "claude-code", args: ["opus-5"] }`
- **THEN** the daemon replies success with the minted name and the harness's
  fresh state

#### Scenario: Empty argv rejected

- **WHEN** a client sends `scratch_run` with no harness and no args
- **THEN** the daemon replies with a structured validation error and registers
  nothing

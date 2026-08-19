---
status: draft
date: 2026-08-19
implements: [ADR-0016]
extends: [SPEC-0003]
requires: [SPEC-0002]
---

# SPEC-0010: CLI Command Tree and Environment Configuration

## Overview

Harness resolves its **process settings** — socket path, config path, log level,
log file, scrollback depth, SSH server enable and listen address, config
watching, and JSON output — from four sources ranked in one fixed order:
an explicit command-line flag, a `HARNESS_*` environment variable, the TOML
file, then the compiled default.

The command tree is built with Cobra; the precedence ladder is resolved by Viper
bound to Cobra's flags. The **domain tables** (`[harness.*]`, `[profile.*]`)
continue to be parsed by the existing `internal/config` loader over
`BurntSushi/toml`, because their validation errors carry source line numbers
that SPEC-0001 REQ "Zero And Error States" renders in the reload banner.

See ADR-0016 for the decision, including why the environment layer deliberately
stops at scalars and does not extend to harness or profile definitions.

This spec **extends SPEC-0003**, which owns the ADR-0006 config surface. It adds
a resolution layer above the file for process settings and changes nothing about
what the file means.

## Requirements

### Requirement: Environment Variable Namespace

Harness SHALL read process settings from environment variables in the
`HARNESS_` namespace. The variable name SHALL be `HARNESS_` followed by the
corresponding flag name uppercased with hyphens replaced by underscores.

The following variables SHALL be recognized:

| Variable | Flag | Type | Default |
|---|---|---|---|
| `HARNESS_SOCKET` | `--socket` | path | `$XDG_RUNTIME_DIR/harness.sock` |
| `HARNESS_CONFIG` | `--config` | path | `$XDG_CONFIG_HOME/harness/harness.toml` |
| `HARNESS_JSON` | `--json` | bool | `false` |
| `HARNESS_LOG_LEVEL` | `--log-level` | enum | `info` |
| `HARNESS_LOG_FILE` | `--log-file` | path | *(stderr)* |
| `HARNESS_SCROLLBACK` | `--scrollback` | int | `attach.DefaultRingLines` |
| `HARNESS_SSH` | `--ssh` | bool | `false` |
| `HARNESS_SSH_LISTEN` | `--ssh-listen` | host:port | *(unset)* |
| `HARNESS_WATCH_CONFIG` | *(none)* | bool | `true` |

Harness MUST NOT read harness or profile definitions from the environment. No
`HARNESS_*` variable SHALL define, modify, or remove a `[harness.*]` or
`[profile.*]` table.

#### Scenario: Environment supplies the socket

- **WHEN** `HARNESS_SOCKET=/tmp/x.sock` is set and no `--socket` flag is given
- **THEN** the client SHALL dial `/tmp/x.sock`

#### Scenario: Unset variable falls through

- **WHEN** `HARNESS_LOG_LEVEL` is unset and no `--log-level` flag is given and
  no `[daemon]` table supplies a level
- **THEN** the daemon SHALL start at the compiled default level `info`

#### Scenario: Empty variable is not a value

- **WHEN** `HARNESS_SOCKET=""` is set (present but empty)
- **THEN** the setting SHALL fall through to the next source in precedence, and
  the empty string SHALL NOT be used as a socket path

#### Scenario: Harness definitions are file-only

- **WHEN** an operator sets `HARNESS_HARNESS_TICKER_CMD=sleep` and no
  `[harness.ticker]` table exists in the config file
- **THEN** no harness named `ticker` SHALL be registered, and the variable SHALL
  be ignored without error

### Requirement: Precedence Order

Harness SHALL resolve each process setting from the first source that supplies
it, in this order: **explicit flag**, then **environment variable**, then
**config file**, then **compiled default**.

A flag SHALL be treated as explicit only when the user supplied it on the
command line — determined by the flag's `Changed` state, not by whether it holds
a non-zero value. A flag left at its declared default MUST NOT suppress an
environment variable.

#### Scenario: Flag beats environment

- **WHEN** `HARNESS_SOCKET=/tmp/x.sock` is set and the user runs
  `harness --socket /tmp/y.sock ls`
- **THEN** the client SHALL dial `/tmp/y.sock`

#### Scenario: Environment beats file

- **WHEN** the config file sets `[daemon]` `watch_config = false` and
  `HARNESS_WATCH_CONFIG=1` is set, with no flag given
- **THEN** the daemon SHALL watch the config file

#### Scenario: File beats default

- **WHEN** the config file sets `[server]` `listen = "127.0.0.1:2222"`, no
  `HARNESS_SSH_LISTEN` is set, and no `--ssh-listen` flag is given
- **THEN** the SSH server SHALL bind `127.0.0.1:2222`

#### Scenario: Unchanged flag does not mask the environment

- **WHEN** `HARNESS_LOG_LEVEL=debug` is set and the user runs
  `harness daemon run` with no `--log-level` flag, where `--log-level` declares
  a default of `info`
- **THEN** the daemon SHALL start at `debug`

### Requirement: Fileless Operation

The daemon SHALL start and serve when no config file exists at the resolved
config path, provided every required process setting is available from a flag,
an environment variable, or a default.

A missing config file SHALL NOT be treated as an error. A config file that
exists but cannot be parsed SHALL remain an error, per SPEC-0003.

#### Scenario: Container with no TOML

- **WHEN** the daemon is started with `HARNESS_SOCKET`, `HARNESS_LOG_LEVEL`, and
  `HARNESS_SSH_LISTEN` set, and no file exists at the resolved config path
- **THEN** the daemon SHALL bind its socket, start the SSH server, and report
  zero configured harnesses

#### Scenario: Malformed file is still fatal

- **WHEN** a file exists at the resolved config path and fails to parse
- **THEN** the daemon SHALL report the parse error with its source line and
  SHALL NOT silently fall back to an empty configuration

### Requirement: Environment Value Validation

Harness SHALL validate environment-supplied values with the same rules applied
to the corresponding flag. A malformed value SHALL produce an error naming the
variable, the offending value, and the accepted form.

An invalid environment value MUST NOT be silently coerced, ignored, or replaced
with the default.

Booleans SHALL accept `1`, `0`, `true`, `false`, `yes`, `no`, `on`, and `off`,
case-insensitively.

#### Scenario: Non-numeric scrollback

- **WHEN** `HARNESS_SCROLLBACK=lots` is set
- **THEN** startup SHALL fail with an error naming `HARNESS_SCROLLBACK`, the
  value `lots`, and that an integer is required

#### Scenario: Unknown log level

- **WHEN** `HARNESS_LOG_LEVEL=verbose` is set
- **THEN** startup SHALL fail with an error naming the variable and listing the
  accepted levels `debug, info, warn, error`

#### Scenario: Boolean spellings

- **WHEN** `HARNESS_SSH=on` is set
- **THEN** the SSH server SHALL be enabled, identically to `HARNESS_SSH=1`

### Requirement: Source Attribution

`harness doctor` SHALL report, for each process setting, its resolved value and
which source supplied it (`flag`, `env`, `file`, or `default`).

Values SHALL be reported in full except where the setting is a path that does
not exist, which SHALL additionally be flagged.

#### Scenario: Doctor names the winning source

- **WHEN** `HARNESS_SOCKET=/tmp/x.sock` is set and the user runs `harness doctor`
- **THEN** the report SHALL show the socket setting with value `/tmp/x.sock` and
  source `env`

#### Scenario: Doctor JSON carries the source

- **WHEN** the user runs `harness doctor --json`
- **THEN** each process setting SHALL appear with both its value and its source
  as machine-readable fields

### Requirement: Command Tree

The CLI SHALL be built as a Cobra command tree. Every verb available before this
spec SHALL remain available with the same name, the same flags, and the same
output contract.

Flags SHALL be scoped as they are today. `--socket`, `--config`, and `--json`
are global and SHALL be accepted before the verb; `--json` SHALL additionally be
accepted after it. Per-verb flags (`--lines`, `--follow`, `--ro`, `--all`) are
local to their verb and MUST NOT be accepted before it.

After the verb, a per-verb flag and the single positional argument SHALL be
accepted in either order, so `harness logs ticker --lines 3` and
`harness logs --lines 3 ticker` behave identically.

`harness` with no verb SHALL open the TUI. `harness daemon` with no subcommand
SHALL behave as `harness daemon run`.

#### Scenario: Interleaved flag and positional after the verb

- **WHEN** the user runs `harness logs ticker --lines 3`
- **THEN** the client SHALL request 3 trailing lines for the harness `ticker`
- **WHEN** the user runs `harness logs --lines 3 ticker`
- **THEN** the client SHALL make the identical request

#### Scenario: Verb-local flag before the verb is rejected

- **WHEN** the user runs `harness --lines 3 logs ticker`
- **THEN** the CLI SHALL exit non-zero with a flag-parse error, because
  `--lines` is local to `logs`

#### Scenario: Global flag placement

- **WHEN** the user runs `harness --json list` or `harness list --json`
- **THEN** both SHALL produce machine-readable output

#### Scenario: Bare daemon verb

- **WHEN** the user runs `harness daemon` with no subcommand
- **THEN** the daemon SHALL start, identically to `harness daemon run`

#### Scenario: Unknown verb

- **WHEN** the user runs `harness frobnicate`
- **THEN** the CLI SHALL exit non-zero with an error naming the unknown verb and
  pointing at `harness --help`

### Requirement: Secrets Exclusion

The `HARNESS_*` namespace SHALL NOT carry credentials. No variable in this spec
accepts a token, key, password, or credential value.

Per-harness secrets SHALL continue to be delivered through `env_file` as ADR-0008
requires. Documentation for this capability SHALL state this explicitly.

#### Scenario: No credential variables exist

- **WHEN** the recognized-variable table is enumerated
- **THEN** every entry SHALL be a path, boolean, integer, enum, or host:port,
  and none SHALL be a credential

### Requirement: Error Handling Standards

All error-producing operations in configuration resolution MUST follow
structured error handling:

- Errors MUST be wrapped with contextual information at each layer boundary, so
  a failure names both the setting and the source that supplied the bad value
- Sentinel errors MUST be defined for failure modes callers distinguish
  programmatically, including "config file absent" (non-fatal) as distinct from
  "config file unparseable" (fatal)
- Silent error swallowing MUST NOT occur — an invalid environment value MUST be
  returned to the caller, never coerced to a default
- Structured logging MUST be used for error reporting, with the variable name
  and source as key-value fields rather than interpolated prose

#### Scenario: Error names its source

- **WHEN** an invalid value arrives from `HARNESS_SCROLLBACK`
- **THEN** the resulting error SHALL identify the environment variable as the
  source, distinguishing it from the same invalid value arriving via `--scrollback`

#### Scenario: Absent file is distinguishable

- **WHEN** a caller loads configuration and no file exists at the resolved path
- **THEN** the caller SHALL be able to distinguish "absent" from "unparseable"
  programmatically, without string-matching the error text

## Design

This spec is paired with `design.md`, which covers the architecture, the
library split, and the staged migration.

---
status: draft
date: 2026-07-27
implements: [ADR-0010]
requires: [SPEC-0002, SPEC-0003, SPEC-0004]
---

# SPEC-0005: Local MCP Surface (facade, broker, prompts)

> **Partly implemented.** The facade's trajectory handlers and its read/write capability classes exist in `internal/facade`. No MCP endpoint, broker, prompt side-loading or caller identity is built yet, and the `[mcp.*]` tables are a config parse error. Tracked by the SPEC-0005 epic in the Harness issue tracker.

## Overview

An MCP server exposed by the daemon that does three things: mirrors the
SPEC-0002 control operations as MCP tools (the **facade**), supervises upstream
MCP servers declared in `harness.toml` and fans one multiplexed endpoint into
every harness (the **broker**), and serves markdown files from configured
directories as MCP prompts (**side-loading**). See **ADR-0010**.

The facade introduces no authority the CLI does not already have; it is a thin
client under ADR-0002. The broker is a new *supervised process class* — stdio
servers with no PTY — sharing the ADR-0005 restart machinery with harnesses.
Write operations are gated per harness by `mcp_allow` — per-caller authorization
scoping analogous to the per-SSH-key scoping ADR-0008 deferred (which remains
open for remote keys). Per-harness scoping needs to know which harness is
calling, so each spawn carries a daemon-minted token that the `harness mcp`
bridge presents on every connection, and adapters wire that bridge into their
tool at launch without touching the tool's own configuration.

## Requirements

### Requirement: Facade Tool Surface

The daemon SHALL expose MCP tools mirroring the control operations enumerated
here, and those tools SHALL delegate to the same handlers the CLI and TUI
invoke rather than reimplementing the operation. Read tools (`harness_list`,
`harness_describe`, `harness_logs`) SHALL be available to every harness. Write
tools (`harness_start`, `harness_stop`, `harness_restart`) SHALL be available
only where `mcp_allow` grants `write`. The remaining SPEC-0002 operations
(`project_up`, `project_down`, `profiles`, `use_profile`, `reload`,
`daemon_info`) are not part of the initial facade; exposing one later requires
classifying it here as read or write. Additional read-only facade tools are
defined by SPEC-0006 (trajectories) and SPEC-0007 (skill search) and
SHALL be classified as read operations under `mcp_allow`; SPEC-0007
additionally defines MCP resources for active skills, which the facade serves
alongside its tools. A facade tool MUST
NOT expose data the corresponding control operation does not already return.

#### Scenario: Facade mirrors the control operation

- **WHEN** a harness calls `harness_list` through the broker endpoint
- **THEN** the response carries the same harness set and states a `list` control
  request would return, including project provenance

#### Scenario: Unknown harness surfaces the structured error

- **WHEN** `harness_describe` names a harness the daemon has no record of
- **THEN** the tool returns an error carrying the `unknown_harness` code, not a
  generic failure

### Requirement: Broker Configuration Schema

The global `harness.toml` SHALL accept `[mcp.<key>]` tables declaring upstream
MCP servers, reusing the `cmd`, `args`, `env_file`, and `restart_delay` field
meanings from the `[harness.*]` schema. `prompts` is a reserved key that
declares no upstream server: a `[mcp.prompts]` table with a `paths` list SHALL
declare directories scanned for prompt markdown. Project files MAY declare
`[mcp.*]` tables, which SHALL apply only to that project's harnesses; this
extends the SPEC-0004 project-file schema, which must admit `[mcp.*]`
alongside `[harness.*]`. The daemon SHALL reject an `[mcp.*]` table other than
`[mcp.prompts]` whose `cmd` is empty with a validation error naming the
offending key.

#### Scenario: Upstream server declared once, started once

- **WHEN** `[mcp.gitea]` is declared and six harnesses are running
- **THEN** exactly one `gitea-mcp` process exists, and all six harnesses reach
  it through their own endpoint

#### Scenario: Invalid upstream rejected at load

- **WHEN** an `[mcp.broken]` table omits `cmd`
- **THEN** config load fails with a validation error naming `mcp.broken`, and
  the daemon keeps its last-good configuration

### Requirement: Tool Namespacing

Tools proxied from an upstream server SHALL be exposed under a name derived from
that server's config key, so two upstreams offering the same tool name coexist
without collision. Facade tools SHALL occupy a reserved namespace that an
upstream key MUST NOT shadow; the daemon SHALL reject a config whose `[mcp.*]`
key would shadow the reserved namespace.

#### Scenario: Two upstreams offering the same tool name

- **WHEN** `[mcp.alpha]` and `[mcp.beta]` both expose a tool named `search`
- **THEN** both are reachable under distinct namespaced names and neither
  shadows the other

#### Scenario: Reserved namespace is protected

- **WHEN** a config declares an `[mcp.*]` key that would collide with the facade
  namespace
- **THEN** config load fails with a validation error and nothing is registered

### Requirement: Broker Lifecycle

Upstream MCP servers SHALL be supervised under the SPEC-0003 lifecycle and the
ADR-0005 restart policy, including `restart_delay` and flapping detection. An
upstream crash MUST NOT terminate the daemon, MUST NOT terminate any harness,
and MUST NOT invalidate a harness's endpoint; calls to a down upstream SHALL
return a structured error until it is restored. Upstream servers SHALL receive
secrets by `env_file` exactly as harnesses do, and the daemon MUST NOT retain
those values in its own state, logs, or protocol frames.

#### Scenario: Upstream crash is isolated

- **WHEN** a supervised upstream server exits unexpectedly
- **THEN** the daemon restarts it per its `restart_delay`, every harness keeps
  its endpoint, and calls to that upstream's tools return a structured error
  while it is down

#### Scenario: Upstream secrets are not persisted

- **WHEN** an upstream declares an `env_file`
- **THEN** its values reach the child process environment, and the daemon's
  state file, logs, and protocol frames contain none of them

### Requirement: Prompt Side-Loading

The daemon SHALL scan each directory in `[mcp.prompts].paths` for markdown
files and expose each as an MCP prompt, deriving the prompt name from the
filename and its description from frontmatter when present. Prompts SHALL be
available to every harness that reaches the broker. A prompt file that fails to
parse SHALL be skipped with a logged warning rather than failing the scan.

#### Scenario: A prompt file becomes a fleet-wide slash command

- **WHEN** a markdown file is added to a configured prompts directory and the
  daemon reloads
- **THEN** every harness's prompt list includes it

#### Scenario: Malformed prompt is skipped, not fatal

- **WHEN** one file in a prompts directory cannot be parsed
- **THEN** the remaining files are still exposed and a warning names the skipped
  file

### Requirement: Caller Identity

At every spawn of a harness process, the daemon SHALL inject into its
environment `HARNESS_NAME`, the harness's qualified name, and
`HARNESS_MCP_TOKEN`, a token of at least 128 random bits minted for that spawn.
Both SHALL be applied after the harness's `env_file`, so an `env_file` cannot
override them, and an `env_file` that sets either SHALL produce a warning naming
the file.
The daemon SHALL hold tokens in memory only. A token MUST NOT be written to
`state.json`, run records, logs, or any output of `describe` or `logs`. It SHALL
be invalidated when its process exits, and a restart SHALL mint a new one.

Harnesses reach the facade and broker through `harness mcp`, a stdio MCP server
that the agent CLI launches. It connects to the daemon socket and presents the
token from its environment. The daemon SHALL attribute each call to the harness
that owns the presented token. `HARNESS_NAME` is informational, and a name that
does not match its token SHALL NOT be trusted. A connection with no valid token
is an **unattributed caller**. It SHALL be scoped as `mcp_allow = ["read"]`, and
it SHALL receive only data that is not scoped to a harness (for example, SPEC-0007
skill repos with `serve_to = ["*"]`).

Attribution is least-privilege scoping within the user's trust boundary
(ADR-0004), not authentication against the user's own processes. A local process
that can read another process's environment can present that process's token.

#### Scenario: Calls are attributed by token

- **WHEN** `harness mcp`, launched inside harness `reduit/agent`, calls
  `harness_list`
- **THEN** the daemon attributes the call to `reduit/agent`, and evaluates
  `mcp_allow` against that harness's configuration

#### Scenario: A claimed name without its token is not trusted

- **WHEN** a process sets `HARNESS_NAME=reduit/agent` but presents no token, or
  a token belonging to another harness
- **THEN** the call is unattributed, or attributed to the token's owner, and
  never to `reduit/agent`

#### Scenario: A restart rotates the token

- **WHEN** a harness restarts
- **THEN** the previous spawn's token is rejected, and the new process's token is
  accepted

#### Scenario: An env_file cannot forge an identity

- **WHEN** a harness's `env_file` sets `HARNESS_MCP_TOKEN=abc`
- **THEN** the process receives the daemon-minted token, and the load logs a
  warning naming the file

#### Scenario: Tokens never reach output

- **WHEN** an operator runs `harness describe --json` on a running harness
- **THEN** the output contains no `HARNESS_MCP_TOKEN` value

#### Scenario: The operator's own session is unattributed

- **WHEN** an interactive agent session started outside Harness runs
  `harness mcp`
- **THEN** its calls are scoped as read-only, and it receives only unscoped data

### Requirement: Endpoint Wiring

Each adapter (SPEC-0006) SHALL declare whether it can add the `harness mcp`
bridge to its tool at launch, and how. Where it can, the daemon SHALL add the
bridge when spawning an agent harness, unless the harness sets
`mcp_bridge = false`. Wiring MUST be additive, and MUST NOT modify any file the
tool owns: it SHALL use a launch flag or environment variable, pointing at a
Harness-owned file under the state directory where the tool requires a file. For
Claude Code this is `--mcp-config` without `--strict-mcp-config`, so the tool's
own servers still load. An adapter that cannot wire the bridge SHALL say so in
`harness doctor`. The operator may then add `harness mcp` to the tool's
configuration by hand, and attribution still works, because the identity
variables are injected regardless of wiring. Where the tool denies MCP tools in
non-interactive runs unless they are allowed, the adapter SHALL also allow the
bridge's tools when it wires the bridge. For Claude Code this means an
`--allowedTools` entry for them. The Generic adapter SHALL NOT be wired.

#### Scenario: Claude Code gets the bridge additively

- **WHEN** a `claude-code` harness with default settings starts
- **THEN** its argv carries `--mcp-config` naming a Harness-owned file that
  declares `harness mcp`, plus an `--allowedTools` entry for the bridge's tools,
  and the spawn writes no file outside Harness's state directory

#### Scenario: Unsupported adapters are reported, not faked

- **WHEN** a harness uses an adapter that cannot wire the bridge
- **THEN** `harness doctor` names the harness and the adapter, and the spawn still
  injects `HARNESS_NAME` and `HARNESS_MCP_TOKEN`

#### Scenario: Opting out

- **WHEN** a harness sets `mcp_bridge = false`
- **THEN** its argv carries no bridge wiring, and its environment still carries
  the identity variables

### Requirement: Capability Scoping

Each harness SHALL carry an `mcp_allow` list defaulting to `["read"]`.
`mcp_allow` SHALL be accepted only from the global configuration file: a
project `harness.toml` declaring it SHALL be rejected, joining `[server]` and
`[profile.*]` on the ADR-0009 global-only list — otherwise a cloned repository
could grant its own harnesses write authority over the fleet. A tool
classified as a write operation SHALL be refused for a harness whose
`mcp_allow` does not include `write`, and the refusal SHALL carry a structured
error distinguishing "not permitted" from "operation failed". Scoping SHALL be
evaluated per call against the configuration of the harness that the Caller
Identity requirement attributes the call to, not cached from handshake. An
unattributed caller SHALL be evaluated as `["read"]`.

#### Scenario: Default scoping refuses a write

- **WHEN** a harness with default `mcp_allow` calls `harness_stop`
- **THEN** the call is refused with a permission-denied error and no harness
  state changes

#### Scenario: Granted scoping permits a write

- **WHEN** a harness configured with `mcp_allow = ["read", "write"]` calls
  `harness_restart` on a sibling
- **THEN** the sibling restarts exactly as an equivalent CLI invocation would

#### Scenario: Scope change takes effect on reload

- **WHEN** `mcp_allow` is widened and the config is reloaded
- **THEN** the next call from that harness is evaluated against the new value
  without restarting the harness

### Requirement: Error Handling Standards

All error-producing operations MUST follow structured error handling:

- Errors MUST be wrapped with contextual information at each layer boundary
  (e.g., "broker: upstream gitea: start failed: executable not found")
- Sentinel errors MUST be defined for domain-specific failure modes callers need
  to distinguish programmatically — at minimum: upstream unavailable, tool not
  permitted, and unknown namespace
- Silent error swallowing MUST NOT occur — every error MUST be returned to the
  caller, logged with sufficient context, or explicitly handled with a
  documented reason for suppression
- Structured logging MUST be used for error reporting (key-value pairs, not
  string interpolation)

#### Scenario: Upstream failure is attributable

- **WHEN** a proxied tool call fails inside an upstream server
- **THEN** the error surfaced to the caller identifies which upstream failed and
  preserves the upstream's own message

### Requirement: Concurrency Safety

All concurrent operations MUST follow safe concurrency patterns:

- Context propagation MUST be used for cancellation and timeout signaling across
  every concurrent boundary, including per-call proxying to upstream servers
- Worker lifecycle MUST be explicitly managed — each supervised upstream MUST
  have a clean startup and a graceful shutdown sequence, and daemon shutdown
  MUST terminate every upstream it started
- Race safety MUST be ensured — shared mutable state (the upstream registry, the
  prompt set, per-harness scope) MUST be protected by appropriate
  synchronization primitives or eliminated via message passing
- Concurrent tests MUST be run with race detection enabled in CI

#### Scenario: Concurrent callers are isolated

- **WHEN** several harnesses call tools on the same upstream simultaneously
- **THEN** each receives its own correct response and no call blocks another
  beyond the upstream's own serialization

#### Scenario: Shutdown reaps upstreams

- **WHEN** the daemon shuts down with upstream servers running
- **THEN** each upstream is signaled and terminated, leaving no orphans

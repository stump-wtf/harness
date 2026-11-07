---
status: proposed
date: 2026-10-26
decision-makers: []
extends: [ADR-0035]
supersedes: []
governs: [SPEC-0005]
related: [ADR-0002, ADR-0007, ADR-0008, ADR-0010, ADR-0048]
---

# ADR-0054: The MCP stdio bridge ships before the broker — read tools on by default, control behind one startup flag

> **Not yet accepted.** Proposed in the PR for stump.wtf/harness#950 by the l
> lane. ADR-0035 already decided that Harness is the MCP gateway and that the
> facade tools (`harness_list`, `harness_describe`, `harness_logs`,
> `harness_start`, `harness_stop`, `harness_restart`) delegate to the same
> handlers the CLI uses. It did not decide what happens before the broker, the
> per-harness endpoints and the spawn tokens exist, which is the state the tree
> is in today and the state an agent operating a daemon needs help with now.

## Context and Problem Statement

Agents that drive a daemon shell out to the CLI: a compound command per check,
`--json` on some verbs and `--raw` on others, and a permission system that sees
one shell string doing both a read and a state change. Issue #950 asks for a
`harness mcp` subcommand that serves these operations as MCP tools over stdio.

ADR-0035 and SPEC-0005 describe the full surface: upstream servers supervised by
the daemon, one filtered endpoint per harness, calls attributed by a per-spawn
`HARNESS_MCP_TOKEN`, and `mcp_allow` granting `write` per harness. None of that
is built: there is no MCP server in the tree, and `[mcp.*]` tables are a config
parse error. Building the bridge is a day of work; building the broker first is a
quarter of one.

**How does an agent drive a daemon with tools today, without pre-empting the
gateway's design or pretending to have an authority the daemon cannot
attribute?**

## Decision

Ship `harness mcp` as a **bridge, not a gateway**: a stdio MCP server that is a
thin client of the daemon socket, exactly as every verb is (ADR-0002), with
nothing new listening and no new trust boundary. Whoever can reach the socket can
already do everything the read tools report.

1. **Names come from the reserved facade namespace.** The tools are
   `harness_list`, `harness_describe`, `harness_runs`, `harness_logs`,
   `harness_jobs`, `harness_triggers`, `harness_daemon_info`, `harness_trigger`,
   `harness_start`, `harness_stop`, `harness_restart`, and the trajectory pair
   already registered in `internal/facade`. When the broker lands, its endpoints
   serve the same names from the same registry; nothing here has to be renamed.
2. **SPEC-0005's tool list grows four read entries.** `harness_runs`,
   `harness_jobs`, `harness_triggers` and `harness_daemon_info` are classified
   `read`. SPEC-0005 excluded `daemon_info` from the initial facade and said
   exposing it later requires classifying it; #950 asks for "daemon status" as a
   first-class tool, and a monitor that cannot ask the daemon whether it is up
   is not useful. No tool returns anything its control operation does not
   already return.
3. **Read is on by default; control is opt-in at startup.** `harness mcp` lists
   and answers read tools only. With `--allow-control` it also lists and answers
   `harness_trigger`, `harness_start`, `harness_stop` and `harness_restart`.
   A write call against a read-only server is refused with `not_permitted`
   **before** the daemon is called, and the tool is absent from `tools/list`, so
   a monitoring agent's model never sees the option. Until the broker gives the
   daemon a caller to attribute, the startup flag is the whole grant, set by
   whoever launches the process; that is the operator's own authority, which is
   what ADR-0035 already assumes for the CLI.
4. **Attribution is deferred, and the bridge says so.** No `HARNESS_MCP_TOKEN`
   is minted or presented here, so every call is an unattributed caller in
   SPEC-0005's terms. This is safe only because control is off unless an
   operator turns it on for a specific process. When token attribution lands, it
   replaces the flag's authority, it does not add to it.
5. **Logs are polled, not followed.** `harness_logs` answers `summary` (ordered
   activity entries) or `raw` (the log tail). A summary carries `next_cursor`,
   the stable `LogEntry.ID` of its newest kept entry, and a `cursor` argument
   returns only newer entries; an aged-out cursor says so and returns the tail
   rather than nothing. A raw payload pages by byte `offset` with
   `next_offset`, and an offset past the returned tail repeats the whole tail
   with a `note`: the tail is a window, so an empty answer there would read as
   "caught up" to a poller that had in fact skipped output. Both are size-capped
   (256 KiB default) and say `truncated`. A cursor is an entry id, not an
   offset, because ids are stable across requests and offsets are not. Real
   subscriptions (`harness://<name>/runs/<id>/log`) stay with the
   broker, where the daemon can push.
6. **One control call at a time.** The MCP session answers requests concurrently
   so a followed trigger cannot stall the session, but the control connection is
   a single request/response stream with no lock of its own; the CLI wrapper
   therefore serializes calls per call, not per tool. This is a transport fact
   about `internal/client`, not a policy, and it belongs to whoever adds a
   connection pool.
7. **Failures are results, not transport errors.** A refusal or a daemon error
   comes back as a tool result with `isError` and a stable `code`
   (`unknown_harness`, `not_permitted`, `unknown_tool`, …), which is SPEC-0005
   REQ "Error Handling Standards" applied to a bridge: a caller branches on the
   code, and one bad call does not end the session.

## Explicit non-goal

Editing `harness.toml` stays a file edit plus `reload`. Supervising upstream
servers, per-harness filtered endpoints, prompt side-loading, the call log,
resources and subscriptions are ADR-0035's, not this bridge's. No redaction is
added to log output beyond what `harness logs` already does; the summary format
echoes no environment by construction, since it renders activity entries.

## Consequences

* Agents get structured, name-gated access to the daemon now, without waiting on
  the gateway, and without a second implementation of each verb to keep in step.
* A read-only `harness mcp` process is safe to hand to a monitoring agent, which
  is the deployment #950's reporter wants.
* `--allow-control` is coarser than `mcp_allow`: it is per process, not per
  harness. That is stated in the docs rather than hidden, and the ADR-0035
  sequence replaces it.
* The reserved-namespace check SPEC-0005 wants (an `[mcp.*]` key may not shadow a
  facade tool) has nothing to collide with yet, but the registry is one list, so
  the check will have one place to consult.
* Concurrency within one session costs a lock: many simultaneous tool calls queue
  on the daemon socket instead of opening more connections. If that ever hurts,
  the fix is a small pool in the CLI wrapper.

## Considered options

* **Bridge now, broker later** (chosen). One thin client, names and error codes
  already in the shape the gateway will use.
* **Wait for the broker.** Correct per ADR-0035's letter, but it leaves the
  reported pain untouched for a quarter and the tool surface unexercised.
* **Per-connection `mcp_allow` read/write with no identity.** Would let any
  process that opens the socket change state by default, which is a wider
  authority than the CLI's and than the flag's: it is not attributable to
  anyone's decision.
* **Control tools on by default, `--read-only` to opt out.** Ships the widest
  authority to the least informed caller; the monitor in #950 would need to
  remember to disable what it must not use.

## Open questions for the maintainer

* Is `harness_daemon_info` acceptable as a read tool, or should daemon status
  ride on `harness_list`? This PR classifies it read and says so in SPEC-0005's
  terms.
* Should `harness_trigger`'s `wait` follow the run inside the bridge (as here),
  or should the daemon grow a follow op the CLI and the bridge share?
* Does the gateway want this bridge to keep existing as the unattributed entry
  point, or to become the token-presenting `harness mcp` SPEC-0005 describes?

## Links

* Issue: https://gitea.stump.rocks/stump.wtf/harness/issues/950
* Upstream report: https://github.com/stump-wtf/harness/issues/25
* Extends: [ADR-0035](./adr-0035-mcp-gateway.md), which supersedes
  [ADR-0010](./adr-0010-local-mcp-surface.md)
* Depends on: [ADR-0002](./adr-0002-daemon-client-architecture.md) (the CLI is
  the supported programmatic surface)
* Code: `internal/mcp`, `cmd/harness/mcp.go`; tool classes in
  `internal/facade/facade.go`

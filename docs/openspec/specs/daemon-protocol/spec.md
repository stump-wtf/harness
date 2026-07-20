---
status: draft
date: 2026-07-18
implements: [ADR-0002, ADR-0004, ADR-0007, ADR-0008]
requires: [SPEC-0003]
---

# SPEC-0002: Daemon Protocol (Control Plane + Attach Stream)

## Overview

One framed, length-prefixed message protocol over the local Unix socket
(`$XDG_RUNTIME_DIR/harness.sock`) carrying two kinds of traffic multiplexed on
a single connection: **control** (structured request/response plus a
state-change subscription) and **attach** (opaque terminal byte streams).
Remote clients never speak a separate wire protocol — the Wish/SSH session runs
the TUI inside the daemon, and that TUI is a local client of this same contract
(ADR-0004). See `design.md` for framing details and rationale.

## Requirements

### Requirement: Message Framing

Every message SHALL be framed as `uint32 length` (big-endian) + `uint8 type` +
payload, with types `HELLO`, `CONTROL_REQ`, `CONTROL_RESP`, `EVENT`,
`ATTACH_OPEN`, `ATTACH_DATA`, `ATTACH_RESIZE`, `ATTACH_CLOSE`, `PING`/`PONG`,
and `ERROR`. Control payloads SHALL be JSON; attach payloads SHALL be raw bytes
tagged with a `session_id` so multiple attach sessions can share one
connection.

#### Scenario: Mixed traffic on one connection

- **WHEN** a client holds an attach session open and issues a control request
- **THEN** both flow over the same connection without corrupting either stream

### Requirement: Handshake And Versioning

A client SHALL open with `HELLO { proto_version, client_version, wants }` and
the daemon SHALL reply `HELLO { proto_version, daemon_version, capabilities }`.
The same `proto_version` major is REQUIRED; on mismatch the daemon SHALL return
a clear `ERROR` ("client too old/new; daemon proto vN") rather than garbling.

#### Scenario: Old client, upgraded daemon

- **WHEN** a client with an older proto major connects
- **THEN** the daemon responds with a structured version-mismatch ERROR and
  closes cleanly

### Requirement: Control Operations

The control plane SHALL mirror the CLI verbs and the TUI 1:1 (ADR-0002):
`list`, `describe`, `start`, `stop`, `restart`, `logs`, `profiles`,
`use_profile`, `reload`, and `daemon_info`. Operations SHALL be idempotent
where that makes sense (double-`start` is a no-op). Errors SHALL come back as
structured `ERROR` frames with a code and a human message.

#### Scenario: Idempotent start

- **WHEN** `start` is issued for a harness already `running`
- **THEN** the daemon replies success without disturbing the process

#### Scenario: Structured failure

- **WHEN** a control request references an unknown harness
- **THEN** the client receives an `ERROR` with a machine code and a human
  message the TUI/CLI can surface verbatim

### Requirement: Event Subscription

After a `HELLO` that includes `wants: ["events"]`, the daemon SHALL push
`EVENT` frames on state changes (`harness_state_changed`, `harness_exited`,
`harness_flapping`, `config_reloaded`, `profile_changed`) so the TUI re-renders
reactively without polling. One-shot CLI invocations MAY skip the
subscription entirely.

#### Scenario: Reactive dashboard

- **WHEN** a harness crashes while a subscribed TUI is on the dashboard
- **THEN** the TUI receives `harness_exited` and `harness_state_changed`
  without issuing any request

### Requirement: Attach Session

`ATTACH_OPEN { name, cols, rows, mode }` SHALL allocate a `session_id` and the
daemon SHALL then send, in order: a screen snapshot (repaint of the current
`x/vt` screen), a bounded tail of scrollback, and the live stream as
`ATTACH_DATA` frames. Client keystrokes flow back as `ATTACH_DATA` and SHALL be
ignored for `ro` sessions (ADR-0008). `ATTACH_RESIZE` SHALL apply the
smallest-attached-client-wins policy (ADR-0003). `ATTACH_CLOSE` from either
side SHALL tear down only that session — the harness and other attached
sessions are untouched.

#### Scenario: Instant repaint on attach

- **WHEN** a client attaches to a running harness
- **THEN** it receives a full screen snapshot first, so the terminal is
  correct before any live bytes arrive

#### Scenario: Read-only attach

- **WHEN** a session opened with `mode: "ro"` sends keystrokes
- **THEN** the daemon discards them and the PTY never sees the input

### Requirement: Backpressure Isolation

The daemon's PTY reader MUST NOT block on any client: output always reaches
the emulator (screen + ring) and the on-disk log. Each attach session SHALL
have a bounded outbound queue; when a slow client can't drain it, the daemon
SHALL coalesce by dropping that session's queued incremental frames and
sending a fresh snapshot instead. `PING`/`PONG` heartbeats SHALL detect dead
clients so their sessions get reaped.

#### Scenario: Slow SSH client

- **WHEN** a remote client stalls mid-stream
- **THEN** the harness and all other clients continue at full speed, and the
  slow client eventually receives a snapshot repaint instead of the backlog

### Requirement: Transport Bindings

Locally, the daemon SHALL serve this protocol on a Unix socket with `0600`
permissions (ADR-0008). Remotely, the daemon MAY run a Wish SSH server whose
sessions host the TUI in-process as a local client of the same protocol — no
separate remote protocol exists (ADR-0004). SSH provides auth, encryption, and
host-key verification.

#### Scenario: Remote parity

- **WHEN** a user attaches over SSH
- **THEN** snapshot-on-attach, backpressure, and resize policy behave
  identically to a local attach

## Non-goals (v1)

- A stable public API for third-party clients (the CLI is the supported
  programmatic surface).
- Session recording/replay as first-class protocol messages (ADR-0007 defers
  the full event store).
- Multi-daemon federation (one daemon per host).

---
id: SPEC-0002
title: Daemon protocol (control plane + attach stream)
status: draft
implements: [ADR-0002, ADR-0004, ADR-0007, ADR-0008]
requires: [SPEC-0003]
---

# Spec — Daemon protocol (control plane + attach stream)

- **Status:** Draft
- **Relates to:** ADR-0002 (thin clients), ADR-0004 (unix socket + Wish),
  ADR-0007 (scrollback/backpressure)

## Overview

One framed, length-prefixed message protocol over the local Unix socket
(`$XDG_RUNTIME_DIR/harnessd.sock`). It carries two logically distinct kinds of
traffic multiplexed over the single connection:

- **Control** — structured request/response + a state-change subscription.
- **Attach** — opaque terminal byte streams (keyboard → PTY, PTY → screen), one
  logical channel per attached harness.

Remote clients (Wish/SSH) don't speak this wire protocol themselves — the
daemon-hosted TUI they drive speaks it in-process/over the same socket (ADR-0004).
So this spec is the single client↔daemon contract, local or remote.

## Framing

- Every message: `uint32 length` (big-endian) + `uint8 type` + payload.
- **Types:** `HELLO`, `CONTROL_REQ`, `CONTROL_RESP`, `EVENT`, `ATTACH_OPEN`,
  `ATTACH_DATA`, `ATTACH_RESIZE`, `ATTACH_CLOSE`, `PING`/`PONG`, `ERROR`.
- Control payloads are **JSON**; attach payloads are **raw bytes** tagged with a
  `session_id` (so multiple attaches can share the connection).
- Rationale: JSON for the small structured stuff (debuggable, easy to evolve);
  raw bytes for the hot terminal path (no per-keystroke JSON overhead).

## Handshake & versioning

- Client opens → `HELLO { proto_version, client_version, wants: ["control","events"] }`.
- Daemon replies `HELLO { proto_version, daemon_version, capabilities: [...] }`.
- **Version policy:** same `proto_version` major required; on mismatch the daemon
  returns a clear `ERROR` ("client too old/new; daemon proto vN") rather than
  garbling. Because client and daemon ship in one binary (ADR-0001), the common
  case is exact match; the check exists for "old client, upgraded daemon."

## Control plane (request/response)

Mirrors the CLI verbs and the TUI 1:1 (ADR-0002/ spec-tui). Illustrative, not final:

| request | payload | response |
|---------|---------|----------|
| `list` | `{ profile? }` | `[{name, state, restarts, last_exit, uptime, backend, profiles[]}]` |
| `describe` | `{ name }` | full harness record (config + runtime) |
| `start` | `{ name }` | `{ ok, state }` |
| `stop` | `{ name }` | `{ ok, state }` |
| `restart` | `{ name }` | `{ ok, state }` |
| `logs` | `{ name, tail?, follow? }` | log lines (or an `EVENT` stream if `follow`) |
| `profiles` | `{}` | `[{name, description, members[], autostart}]` |
| `use_profile` | `{ name, start_stopped? }` | `{ ok, active }` |
| `reload` | `{}` | `{ ok, changed[], errors[] }` (re-read TOML — ADR-0006) |
| `daemon_info` | `{}` | `{ version, uptime, socket, server_enabled }` |

All are idempotent where it makes sense (double-`start` is a no-op, as today's
`harness-run` already guards). Errors come back as structured `ERROR` with a code
+ human message (surfaced in the TUI, printed by the CLI).

## Events (subscription)

- After `HELLO … wants:["events"]`, the daemon pushes `EVENT` frames on state
  changes: `harness_state_changed`, `harness_exited {code}`, `harness_flapping`,
  `config_reloaded {changed,errors}`, `profile_changed`.
- The TUI subscribes once and re-renders reactively (no polling). The scriptable
  CLI generally doesn't subscribe (one-shot request → print → exit).

## Attach (data plane)

The hot path — must feel instant (ADR-0004/0007).

1. Client → `ATTACH_OPEN { name, cols, rows, mode: "rw"|"ro" }`.
2. Daemon allocates a `session_id`, replies, then immediately sends:
   - a **screen snapshot** (repaint of the current `x/vt` screen), then
   - a **tail of scrollback** (bounded), then
   - the **live stream** as `ATTACH_DATA { session_id, bytes }` frames.
3. Client keystrokes → `ATTACH_DATA { session_id, bytes }` (ignored by the daemon
   if the session is `ro` — ADR-0008).
4. Client viewport change → `ATTACH_RESIZE { session_id, cols, rows }`; the daemon
   applies the resize policy (smallest attached wins — ADR-0003).
5. `ATTACH_CLOSE { session_id }` (either side) tears the session down. The harness
   is untouched; other attached sessions continue.

### Backpressure (normative — ADR-0007)

- The daemon's PTY reader **never blocks on a client.** Output always goes to the
  emulator (screen+ring) and the on-disk log.
- Each attach session has a **bounded outbound queue**. If a client (typically a
  slow/remote one) can't drain it, the daemon **coalesces**: it drops queued
  incremental frames for *that session* and sends a fresh screen snapshot instead.
  Correctness is preserved (the client repaints to the true current screen); only
  that one client's smoothness degrades. One slow SSH client can never stall a
  harness or another client.
- `PING`/`PONG` (idle heartbeat) detects dead clients so their sessions get reaped.

## Transport bindings

- **Local:** the framed protocol over the `0600` Unix socket (ADR-0008). Full
  control + attach.
- **Remote:** SSH via Wish. The SSH session runs the TUI **in the daemon**; that
  TUI is a local client of this same protocol. So remote inherits every guarantee
  here (snapshot-on-attach, backpressure, resize policy) with SSH providing auth +
  encryption + host-key verification (ADR-0004/0008). No separate remote protocol.

## Non-goals / deferred

- A stable *public* API for third-party clients (this is an internal contract in
  v1; the CLI is the supported programmatic surface).
- Session recording/replay as first-class protocol messages (ADR-0007 defers the
  full event store).
- Multi-daemon federation (one daemon per host; a client picks a host via the
  socket or an SSH target).

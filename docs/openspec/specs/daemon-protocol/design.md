# Design: Daemon Protocol (Control Plane + Attach Stream)

## Context

The daemon owns all PTYs, emulators, and state (ADR-0002); every client — the
TUI, the scriptable CLI, and remote SSH sessions — is thin and talks one
contract. Governing spec: SPEC-0002. Related: ADR-0004 (transport), ADR-0007
(scrollback/backpressure), ADR-0008 (socket permissions, read-only attach),
SPEC-0003 (the states and events this protocol transports).

## Goals / Non-Goals

### Goals

- One wire contract for local and remote, control and attach.
- The hot path (terminal bytes) stays cheap: no per-keystroke JSON.
- A slow client can never stall a harness or another client.

### Non-Goals

- Public/stable third-party API in v1.
- TLS/TCP listeners — local is the Unix socket, remote is SSH (Wish); there is
  no third transport.

## Decisions

### JSON control, raw attach bytes

**Choice**: JSON payloads for control frames; raw tagged bytes for attach.
**Rationale**: Control traffic is small and benefits from debuggability and
easy evolution; terminal streams are hot and byte-transparent.
**Alternatives considered**:
- gRPC/protobuf: heavier toolchain, no real win over a single socket we fully
  own on both ends (client and daemon ship in one binary, ADR-0001).
- JSON for everything: per-keystroke encode/decode overhead on the hot path.

### Remote runs the TUI in-daemon

**Choice**: Wish SSH sessions host the Bubble Tea TUI inside the daemon
process; that TUI is itself a local protocol client.
**Rationale**: One protocol to maintain; remote inherits every local guarantee
(snapshot-on-attach, backpressure, resize policy) for free (ADR-0004).

### Coalesce-to-snapshot backpressure

**Choice**: Bounded per-session outbound queues; overflow drops that session's
queued increments and substitutes a fresh screen snapshot.
**Rationale**: Correctness (client repaints to true screen) at the cost of one
slow client's smoothness — never the harness's or anyone else's (ADR-0007).

## Architecture

```mermaid
sequenceDiagram
    participant C as Client (TUI/CLI)
    participant D as harnessd
    participant H as Harness PTY
    C->>D: HELLO {proto, wants: control+events}
    D->>C: HELLO {proto, capabilities}
    C->>D: ATTACH_OPEN {name, cols, rows, mode}
    D->>C: screen snapshot (x/vt repaint)
    D->>C: scrollback tail (bounded)
    H-->>D: PTY output (never blocks on C)
    D-->>C: ATTACH_DATA (live, coalesced under backpressure)
    C->>D: ATTACH_DATA (keystrokes; dropped if ro)
    C->>D: ATTACH_RESIZE {cols, rows}
    Note over D: smallest attached client wins (ADR-0003)
    C->>D: ATTACH_CLOSE
    Note over D,H: harness untouched; other sessions continue
```

Frame layout: `uint32 length (BE) | uint8 type | payload`. Attach frames carry
`session_id` so several attaches multiplex one connection.

## Key files

Greenfield. Expected home: a `protocol` package (framing, types, codecs)
shared by daemon and client halves of the binary, plus a daemon-side session
manager owning attach queues and heartbeats.

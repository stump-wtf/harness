# ADR-0002 — Process model: long-lived daemon + thin TUI client

- **Status:** Proposed
- **Date:** 2026-07-18

## Context and problem statement

The product is explicitly *"a little daemon that runs and you can hop into
different configurations of AI harnesses."* Today there is no daemon of our own —
tmux is the resident process, systemd/launchd supervise `harness-run`, and the
zsh function is a stateless client that shells out. To get a real dashboard,
remote attach, scrollback that survives, and start/stop that doesn't depend on a
shell being open, we need to decide *where the long-lived state lives*.

## Decision drivers

- Harnesses must outlive any client — close the laptop lid, the agents keep
  running; reconnect later and everything's still there.
- One authoritative place that knows every harness's state (running/attached/
  crashed), owns every PTY, and owns scrollback.
- Multiple clients: local TUI, scriptable CLI, remote SSH session — possibly
  concurrently, possibly attached to the same harness.
- Keep the client cheap to start and cheap to kill (it's just a viewport onto the
  daemon's truth).

## Considered options

1. **Long-lived daemon owns everything; clients are thin views** (the classic
   `tmux server` / `dockerd` / `sshd` shape).
2. **No daemon; client drives tmux directly** (today's model, dressed up).
3. **Daemon-per-profile** (one supervisor process per "configuration").

## Decision outcome

**Chosen: one long-lived `harnessd` that owns all state; clients are thin.**

### Responsibilities

**`harnessd` (the daemon) owns:**

- The **harness registry** — parsed config + live state for every harness.
- **Supervision** — spawns each harness under a PTY, runs the restart loop,
  tracks exit codes, applies `restart_delay` (ADR-0005).
- **PTYs & terminal emulation** — one `x/xpty` master + one `x/vt` emulator per
  running harness; the emulator maintains screen + scrollback (ADR-0003, 0007).
- **The control plane** — a Unix-domain socket serving list/start/stop/status/
  attach RPCs (ADR-0004, spec-daemon-protocol).
- **The data plane** — bidirectional attach streams (bytes in from a client's
  keyboard → PTY; bytes out from PTY → all attached clients).
- **Profiles** — which "configuration" of harnesses is active (ADR-0006).

**The client (TUI or CLI) owns:** nothing durable. It connects, subscribes to
state, renders, forwards keystrokes when attached, and can die at any moment
without affecting a single harness.

### Why "hop between" works cleanly

Because the daemon is the terminal emulator, "attach" is not "take over a PTY" —
it's "subscribe to a screen." N clients can attach to the same harness and all
see the same live output; detaching is just closing the subscription. Switching
harnesses in the TUI is switching which subscription is on screen. The harness
never notices. This is the property tmux gives us via its server, made native and
ours.

### Fan-out / fan-in

```
                        ┌────────────────────────── harnessd (resident) ──────────────────────────┐
                        │                                                                          │
   local TUI  ─────────▶│  control plane (unix socket)   supervisor    registry+profiles          │
   scriptable CLI ─────▶│        list/start/stop/…          │             │                        │
   remote SSH (Wish) ──▶│  data plane (attach streams)      ▼             ▼                        │
                        │        ▲          ▲          ┌─ harness A ─┐  ┌─ harness B ─┐  …          │
                        │        │          │          │ xpty ⇄ proc │  │ xpty ⇄ proc │             │
                        │        └──────────┴───── vt screen+scrollback └── vt screen+scroll        │
                        └──────────────────────────────────────────────────────────────────────────┘
```

## Consequences

**Positive**

- Harnesses survive client churn, logout, and network drops — the core promise.
- Single source of truth; no reconciliation between "what tmux thinks" and "what
  the UI thinks."
- Concurrent + shared attach falls out naturally (subscribe to a screen).
- Clean seam for remote: a remote SSH session is just another thin client
  (ADR-0004).

**Negative / costs**

- We now own a daemon lifecycle: start-on-login, crash recovery of the daemon
  itself, versioning between client and daemon (protocol compatibility). ADR-0005
  and spec-daemon-protocol address these.
- If the daemon crashes, *all* harnesses die with it (unlike today, where each
  harness is an independent systemd unit). Mitigations: keep the daemon tiny and
  well-tested; let systemd/launchd restart it; consider optionally re-parenting
  harnesses to a per-harness reaper. Flagged as a real risk in ADR-0005.

**Rejected options**

- **No daemon (drive tmux)** — can't own scrollback/state independent of tmux,
  can't do a real network protocol, and forever inherits tmux's model instead of
  ours. This is precisely what we're graduating from.
- **Daemon-per-profile** — multiplies the supervision/lifecycle problem by the
  number of profiles for no real gain; a single daemon can hold many profiles
  (ADR-0006). Rejected as premature.

## Related

ADR-0003 (what the daemon does per harness), ADR-0004 (how clients reach it),
ADR-0005 (who supervises the daemon), ADR-0007 (scrollback ownership).

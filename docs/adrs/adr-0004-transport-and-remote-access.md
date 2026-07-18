# ADR-0004 — Transport: Unix-socket control plane + Wish/SSH data plane

- **Status:** Proposed
- **Date:** 2026-07-18

## Context and problem statement

Clients (local TUI, scriptable CLI, remote sessions) need to reach the daemon for
two very different kinds of traffic:

1. **Control plane** — request/response RPCs: list harnesses, start/stop/restart,
   subscribe to state changes, read config. Small, structured, latency-tolerant.
2. **Data plane** — attach: a high-throughput, bidirectional, byte-oriented stream
   (keystrokes in, terminal output out) that must feel instant.

And a headline requirement: *hop into harnesses from anywhere*, including a phone
or another machine — the current README already leans on "drive Claude Code
remotely." So **remote access is in scope**, and it should be secure without us
inventing auth.

## Decision drivers

- Local attach must be **zero-latency-feeling** and need no network config.
- Remote attach must be **secure by default** and reuse credentials people already
  have (SSH keys), not a bespoke auth system.
- Don't build two entire UIs — the remote experience should be the *same* TUI.
- Keep the local common case dependency-light (no need to run an SSH server just
  to attach to your own laptop).

## Considered options

- **A.** Everything over one **Unix domain socket** (control + attach); remote is
  "SSH into the box and run the client there."
- **B.** **gRPC over TCP** for everything, with our own TLS + token auth.
- **C.** **Unix socket for control + data locally**, and **Wish (SSH) as a remote
  front door** that runs the same TUI server-side and talks to the daemon over the
  local socket. ✅
- **D.** Custom TCP protocol for both, hand-rolled framing + auth.

## Decision outcome

**Chosen: Option C.**

- **Locally**, the daemon listens on a **Unix domain socket**
  (`$XDG_RUNTIME_DIR/harnessd.sock`, `0600`). Both the control RPCs and the attach
  byte-stream ride this socket (multiplexed by a small framed protocol —
  see spec-daemon-protocol.md). No network, no TLS, no ports; filesystem
  permissions are the access control. This is the fast, common path.

- **Remotely**, the daemon (optionally) runs a **[Wish](https://github.com/charmbracelet/wish)
  SSH server**. An incoming SSH session is handed a **Bubble Tea program — the
  same TUI** — running *inside the daemon process*, which talks to its own registry
  and emulators directly (or over the local socket). Wish wires the SSH PTY and
  resize events straight into that Bubble Tea program. So:
  - `ssh harness.host` → you're in the dashboard, attaching to harnesses, exactly
    as if you were local.
  - **Auth is SSH public keys** — an `authorized_keys`-style allowlist in daemon
    config. No new credential system, no passwords in our code.
  - It's *not* a shell: Wish apps expose only the TUI, so a client can never get a
    shell on the host through this door (beyond what attaching to a harness's own
    terminal inherently allows — see ADR-0008).

### Many hosts: `wishlist` as the directory

The remote story naturally extends to *multiple* machines each running a
`harnessd`. [`wishlist`](https://github.com/charmbracelet/wishlist) ("the SSH
directory") is a Charm-native menu of SSH endpoints — so a single launcher can
list every box's daemon and let you hop across hosts, which is the literal
"hop into harnesses **anywhere**" promise at the fleet level. v1 targets a single
daemon; wishlist is the sketched path to multi-host without new protocol work.
[`promwish`](https://github.com/charmbracelet/promwish) (Prometheus middleware for
Wish) can expose attach/session metrics into Joe's existing monitoring. Both are
noted as ◑/○ in [charm-ecosystem-map.md](#file-charm-ecosystem-map-md), not v1
commitments. [`soft-serve`](https://github.com/charmbracelet/soft-serve) is the
reference for how a real Wish-based multi-user SSH TUI daemon does auth + access
levels — worth reading before we build ours.

### Why this split

The control/data traffic wants a local socket (fast, simple, OS-enforced perms).
Remote wants auth + transport we don't have to build. Wish gives us remote **and**
reuses the *same TUI code* (ADR-0001/0003 make the TUI render a `vt` screen the
daemon already owns), so remote isn't a second product. The two planes meet at the
daemon, not at the wire.

```
LOCAL:   TUI/CLI ──unix socket (framed: control + attach)──▶ harnessd
REMOTE:  ssh client ──SSH (Wish, pubkey auth)──▶ [Bubble Tea TUI in daemon] ──▶ registry/emulators
```

### Protocol shape (summary; full detail in spec-daemon-protocol.md)

- One framed, length-prefixed message protocol over the Unix socket.
- Two channel types: **control** (JSON request/response + a state-change
  subscription) and **attach** (opaque binary frames, one logical stream per
  attached harness, tagged with a session id).
- Versioned handshake so client and daemon can refuse/inform on mismatch.

## Consequences

**Positive**

- Local path is fast and dependency-free; perms are the OS's job.
- Remote is secure-by-default via SSH keys and adds **zero** new auth code.
- Remote and local share one TUI — no second surface to design or maintain.
- Wish guarantees no incidental shell access.

**Negative / costs**

- Two listeners to manage (socket always; SSH optional/opt-in). SSH host key
  management, `authorized_keys` config, and a listen port to expose.
- Running the TUI *inside* the daemon for SSH sessions means the daemon links the
  UI code — the "thin client" is thin for local use but the daemon is fatter.
  (Acceptable: it's the same binary anyway per ADR-0001.)
- Attach throughput over the socket must be tuned (framing overhead, backpressure
  when a slow client can't keep up) — spec-daemon-protocol.md defines a
  drop/coalesce policy so one slow client can't stall a harness.

**Rejected options**

- **A (Unix socket only, remote = ssh+run client there)** — works, but the remote
  client would try to open *its own* local socket on the remote box; you'd be
  attaching to the wrong daemon. Doesn't actually give remote access to *your*
  harnesses without extra plumbing. Wish solves this directly.
- **B / D (gRPC-TCP or custom TCP with our own auth)** — reinvents transport
  security we get free from SSH, and still leaves us building the remote UI story.
  More code, more attack surface, weaker default security.

## Related

ADR-0002 (clients are thin views), ADR-0003 (remote renders the same `vt`
screen), ADR-0008 (SSH keys, socket perms, secrets), spec-daemon-protocol.md.

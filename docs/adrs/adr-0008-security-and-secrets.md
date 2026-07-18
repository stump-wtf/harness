# ADR-0008 — Security, auth & secrets

- **Status:** Proposed
- **Date:** 2026-07-18

## Context and problem statement

A resident daemon that (a) can spawn arbitrary commands, (b) holds live terminals
into agent CLIs with broad permissions (`--dangerously-skip-permissions`,
`--yolo`), and (c) is *attachable over the network* (ADR-0004) is a juicy target.
We need a security model that's strong-by-default without inventing crypto, and a
secrets story at least as safe as today's `env_file`.

## Decision drivers

- Local access control with no config (just work, safely).
- Remote access must be authenticated and auditable, reusing existing credentials.
- Secrets (API keys in `env_file`) must not leak into daemon state, logs we write,
  scrollback exports, or the protocol.
- The daemon's power (spawn + attach) must not be reachable by other local users or
  unauthenticated network peers.

## Decision outcome

### Local access — filesystem permissions

- Control/data socket at `$XDG_RUNTIME_DIR/harnessd.sock`, mode **`0600`**, owned
  by the user. On systems without a per-user runtime dir, fall back to
  `$XDG_STATE_HOME/harnessd/harnessd.sock` in a `0700` dir. Only the owning user
  can talk to the daemon. No auth beyond OS perms is needed locally — same trust
  model as the tmux socket today.
- State/log files under `$XDG_STATE_HOME/harnessd/` are `0600`/`0700`.

### Remote access — SSH public keys via Wish (opt-in)

- The Wish SSH server is **off by default**; enabling it is a deliberate config
  step (bind address + `authorized_keys`).
- **Auth = SSH public keys** only. An allowlist in daemon config
  (`[server] authorized_keys = [...]` or a path to an `authorized_keys` file). No
  passwords, no bearer tokens of our design.
- The daemon has a **stable host key** (generated on first run via
  [`keygen`](https://github.com/charmbracelet/keygen), `0600`), so clients get real
  host-key verification — no TOFU-blind connections. Optionally, back that key up
  as seed words with [`melt`](https://github.com/charmbracelet/melt) so a
  re-provisioned host keeps its identity and clients don't trip
  host-key-changed warnings.
- Wish apps are **not shells**: a remote peer gets the TUI, not `/bin/sh`.
- **Bind narrowly by default** (loopback / explicit address); document that
  exposing it wants a firewall or, better, reaching it over Tailscale/WireGuard/an
  SSH tunnel rather than the public internet.
- *(Optional, later)* per-key authorization scoping — e.g. a key that may attach
  read-only but not start/stop. Noted, not v1.

### The irreducible risk: attach == terminal access

Attaching to a harness *is* getting that harness's terminal — and many harnesses
are agent CLIs running with skip-permissions. So **anyone who can attach can drive
the agent**. That's inherent to the product, not a bug. Consequences:

- Treat "can attach to this daemon" as equivalent to "can act as these agents."
  Guard the socket (local perms) and the key allowlist (remote) accordingly.
- Consider a per-harness **read-only attach** mode (stream output, ignore input)
  for "watch it work" without handing over the keyboard — pairs with the optional
  per-key scoping above. Useful and cheap; propose for v1.

### Secrets

- Secrets stay **exactly where they are today: in `env_file`**, sourced into the
  child process's environment at spawn. The daemon reads the file, sets the child
  env, and **does not** retain the values in its own long-lived state.
- **Never persist secrets in things we control:** state.json, our rotating log
  files, scrollback *exports*, or protocol frames must never include `env_file`
  contents. (The daemon can't stop a *harnessed program* from printing its own
  secrets to its terminal — that output lands in scrollback/logs like any other
  output. We document this; optionally offer per-harness "don't persist scrollback
  to disk" for sensitive ones — ADR-0007.)
- `env_file` path and file perms are the user's responsibility (as today); we can
  **warn** in the TUI if an `env_file` is group/world-readable.
- Fits Joe's setup: `env_file` already points at Vault/OpenBao-rendered static env
  files; the daemon never needs to know about the secret backend.

## Consequences

**Positive**

- Local: zero-config, OS-enforced, same trust model as tmux today.
- Remote: authenticated by SSH keys with host-key verification, opt-in, non-shell,
  no home-grown auth.
- Secrets handling is no worse than today and explicitly fenced out of everything
  the daemon persists.
- Read-only attach + (later) per-key scoping give a real least-privilege story.

**Negative / costs**

- The daemon is a high-value target by nature (spawn + agent terminals); a socket
  or key-allowlist misconfig is serious. Mitigate with safe defaults (remote off,
  bind loopback, `0600`) and TUI warnings for risky config.
- We inherit SSH host-key + `authorized_keys` management ergonomics for the remote
  path (documented, not automated away).
- We can't prevent a harnessed program from leaking its own secrets into its own
  output; only limit what *we* persist and offer opt-out.

## Related

ADR-0004 (Wish/SSH transport), ADR-0006 (`env_file` in config), ADR-0007 (what
persists, scrollback opt-out), spec-daemon-protocol.md (no secrets in frames).

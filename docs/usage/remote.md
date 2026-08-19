---
title: "Remote access"
sidebar_position: 7
---

# Remote access

Harness can expose its cockpit over SSH, so you can drive your harnesses from
your phone or another machine. A remote session is just another thin client
onto the same daemon — the same dashboard, the same `hop`.

This is implemented with [Wish](https://github.com/charmbracelet/wish)
(ADR-0004) and is strictly **public-key only**: there is no password auth path
(ADR-0008).

## Enable it

In the global `harness.toml`:

```toml
[server]
enabled = true
listen = "0.0.0.0:23234"
authorized_keys_file = "~/.ssh/harness_authorized_keys"
```

Then restart (or reload) the daemon. You can also force it on / override the
bind at daemon start:

```sh
harness daemon --ssh --ssh-listen 0.0.0.0:23234
```

## Connect

Put the public key of the device you'll connect from in
`authorized_keys_file` (or list it inline under `[[server.key]]`), then:

```sh
ssh -p 23234 host
```

There is no user involved — the server never inspects the SSH username, and
auth is the key alone. (Any username the client sends is accepted alongside a
valid key; omit it and ssh simply defaults to your local one.)

You land in the full TUI dashboard, attached to the daemon over the network.

## Read-only keys

Per-key read-only scoping lets a key attach and watch without ever typing
(ADR-0008) — useful for a read-only monitoring device:

```toml
[[server.key]]
key = "ssh-ed25519 AAAA…"
read_only = true
```

A read-only session behaves exactly like `harness attach --ro`: live output and
scrollback, keystrokes ignored, with a read-only badge in the status bar.

## Security notes

- **No secrets travel this path** — only SSH public keys and a persisted host
  key. There is no password authentication and no credential prompt.
- Only keys you list (or that are in `authorized_keys_file`) can connect.
- The remote surface stays optional and off by default. Flip `enabled = true`
  explicitly and bind to a port you deliberately expose.
- See [ADR-0008](/decisions/adr-0008-security-and-secrets) for the full
  security model.

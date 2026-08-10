---
title: "Configuration"
sidebar_position: 3
---

# Configuration

Harness reads a single TOML file. The default location is
`~/.config/harness/harness.toml` (honoring `$XDG_CONFIG_HOME`), overridable on
every verb with `--config`. A repo can also carry its own project-scoped
`harness.toml` — see [Projects](./projects).

A copy of the example lives in the repo as `harness.toml.example`.

## Harnesses

Each `[harness.<name>]` table defines one supervised process.

```toml
[harness.my-agent]
cmd = "claude"
args = ["--foo", "bar"]
workdir = "~/src/my-project"
description = "long-running agent in ~/src/my-project"
enabled = false
```

| Field | Meaning |
|-------|---------|
| `cmd` | the executable to run (**required** unless `prompt` is set; can be a bare command found on `$PATH`) |
| `args` | argument list passed to `cmd` |
| `workdir` | working directory (**required** for most commands) |
| `env_file` | optional `KEY=VALUE` file sourced before launch (secrets stay here, out of the config) |
| `description` | free-text shown in the dashboard |
| `enabled` | autostart on daemon boot / after a daemon restart (`true`) |
| `restart` | restart policy on exit — see [Restart policy](#restart-policy) |
| `restart_delay` | seconds to wait before restarting a harness that exited |
| `backend` | hosting strategy: `native` (default) or `tmux` |
| `tmux_socket` | tmux socket name; only used when `backend = "tmux"` |

An **agent one-shot** replaces `cmd`/`args` with a `prompt` — see the next
section.

A bare `[name]` table is accepted for backward compatibility, but the
`[harness.<name>]` form is preferred (ADR-0006).

## Agent one-shot harnesses

Give a harness a `prompt` instead of a `cmd`, and the daemon synthesizes the
agent invocation at spawn time (currently `crush run --quiet …`). This turns a
harness into a one-shot agent run:

```toml
[harness.deploy-check]
prompt = "check the deployments and report anything unhealthy"
model = "claude-opus-5"        # optional model (requires prompt)
auto_accept = true             # optional: unattended/yolo mode (requires prompt)
max_turns = 20                 # optional: turn budget, 0 = unlimited (requires prompt)
# the one-shot is headless by default; set quiet = false to stream output
# quiet = false
workdir = "~/src/my-project"
# one-shot runs default to restart = "no" (a completed run must not respawn);
# set restart explicitly to opt back into supervision
```

| Field | Meaning |
|-------|---------|
| `prompt` | the agent instruction. Mutually exclusive with `cmd`/`args`; stored verbatim (never placeholder-expanded) and synthesized into the agent argv at spawn |
| `model` | which model the agent runs, e.g. `claude-opus-5`. Requires `prompt`; folded into the synthesized argv |
| `auto_accept` | run unattended, bypassing the agent's permission prompts (the vendor's yolo flag). Requires `prompt`; fold into the synthesized argv |
| `max_turns` | cap on how many iterations the agent may run before stopping. Requires `prompt`; 0 or omitted means unlimited |
| `quiet` | run headless (suppress the agent's interactive output). Defaults to `true` for a prompt one-shot; set `false` to stream output to whoever attaches |

These agent fields are **config truth only** — they are never written into
`args` (that would corrupt the edit-form round-trip). The daemon folds them
into the synthesized agent argv at spawn time, and they **require `prompt`**:
there is no vendor-agnostic place to inject a flag into an arbitrary `cmd`'s
argv, so a `cmd` harness passes its tool's flags through `args` itself.

⚠️ `auto_accept` bypasses **ALL** of the agent's permission prompts. Only enable
it on trusted, headless runs.

## Restart policy

The `restart` key mirrors Docker Compose's directive and controls whether a
harness is restarted when it exits:

| Value | Behavior |
|-------|----------|
| `"no"` | never restart automatically |
| `"always"` | always restart (this is the default when the key is omitted) |
| `"unless-stopped"` | always restart, unless explicitly stopped |
| `"on-failure"` | restart only on a non-zero exit code |

Retry limits under a crash loop come from the daemon's crash-loop policy, which
escalates backoff — see [Supervision](./supervision).

## Profiles

Profiles are named groups of harnesses that switch together

```toml
[profile.default]
description = "everyday set"
harnesses = ["heartbeat"]
autostart = true

[profile.full]
description = "all agents"
harnesses = ["heartbeat", "my-agent"]
```

- Only one profile is active at a time.
- `autostart = true` brings that profile's harnesses up when the daemon starts
  (and on the active profile). The active profile is what `harness list` flags
  with `*`.
- Switch with `harness use-profile <name>`.

Profiles are a global concern — they are not allowed in a project file (see
[Projects](./projects)).

## Remote access (`[server]`)

The optional SSH front door exposes the same dashboard over the network:

```toml
[server]
enabled = true
listen = "0.0.0.0:23234"
authorized_keys_file = "~/.ssh/harness_authorized_keys"
```

Only listed SSH public keys can connect — there is **no password auth path**.
Per-key read-only scoping lets a key attach without typing:

```toml
[[server.key]]
key = "ssh-ed25519 AAAA…"
read_only = true
```

See [Remote access](./remote) for setup and security notes.

## After editing

`harness reload` re-reads the config and reconciles running harnesses without a
daemon restart. `harness doctor` verifies the file parses and the daemon agrees
with it.

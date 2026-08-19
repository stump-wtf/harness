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
harness = "claude-code"
args = ["--foo", "bar"]
workdir = "~/src/my-project"
description = "long-running agent in ~/src/my-project"
enabled = false
```

| Field | Meaning |
|-------|---------|
| `harness` | **required** — the harness kind, an enum: `crush`, `claude-code`, `codex`, `generic`. There is no default; every harness says what it runs. It selects the adapter, which owns the executable a long-running harness runs — `args` are appended after it. `generic` runs `sh`, so its `args` are **sh's** args: use `args = ["-c", "<command line>"]` to run an arbitrary command |
| `args` | argument list appended after the adapter's executable |
| `workdir` | working directory (**required** for most commands) |
| `env_file` | optional `KEY=VALUE` file sourced before launch (secrets stay here, out of the config) |
| `description` | free-text shown in the dashboard |
| `enabled` | autostart on daemon boot / after a daemon restart (`true`) |
| `restart` | restart policy on exit — see [Restart policy](#restart-policy) |
| `restart_delay` | seconds to wait before restarting a harness that exited |
| `backend` | hosting strategy: `native` (default) or `tmux` |
| `tmux_socket` | tmux socket name; only used when `backend = "tmux"` |

An **agent one-shot** replaces `args` with a `prompt` — see the next
section.

A bare `[name]` table is accepted for backward compatibility, but the
`[harness.<name>]` form is preferred (ADR-0006).

## Agent one-shot harnesses

Give a harness a `prompt`, and the daemon synthesizes the
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
| `prompt` | the agent instruction. Mutually exclusive with `args`; stored verbatim (never placeholder-expanded) and synthesized into the agent argv at spawn from the same `harness` adapter |
| `model` | which model the agent runs, e.g. `claude-opus-5`. Requires `prompt`; folded into the synthesized argv |
| `auto_accept` | run unattended, bypassing the agent's permission prompts (the vendor's yolo flag). Requires `prompt`; fold into the synthesized argv |
| `max_turns` | cap on how many iterations the agent may run before stopping. Requires `prompt`; 0 or omitted means unlimited |
| `quiet` | run headless (suppress the agent's interactive output). Defaults to `true` for a prompt one-shot; set `false` to stream output to whoever attaches |

These agent fields are **config truth only** — they are never written into
`args` (that would corrupt the edit-form round-trip). The daemon folds them
into the synthesized agent argv at spawn time, and they **require `prompt`**:
there is no vendor-agnostic place to inject a flag into an arbitrary
argv, so a long-running harness passes its tool's flags through `args` itself.

⚠️ `auto_accept` bypasses **ALL** of the agent's permission prompts. Only enable
it on trusted, headless runs.

## Scheduled one-shots

Give an agent one-shot a `schedule` (a standard cron expression, validated at
config load) and the daemon fires it on that cadence — ADR-0013's replacement
for the original SPEC-0008 timer design:

```toml
[harness.stumpcloud-sweep]
prompt = "check all StumpCloud services and report anything unhealthy"
auto_accept = true
schedule = "0 */6 * * *"   # every 6 hours
description = "scheduled sweep (every 6 hours)"
```

Rules:

- Requires `prompt` — only agent one-shots can be scheduled.
- At each firing the harness starts if it is not already running; **overlapping
  firings are skipped, not stacked**.
- The run exiting is terminal for that firing; the restart policy applies only
  to abnormal exit, so only `restart = "no"` (the prompt default) and
  `"on-failure"` are accepted here.
- Mutually exclusive with `enabled = true` and with profile membership.
- Global config only — project files reject the key.

`harness list` marks a scheduled harness inline — a clock glyph in place of the
state glyph, and the next firing appended to its description (`· in 4h3m`).
`harness describe` adds the cron spec and the absolute next-run time.

## Agent adapters

The `harness` key is a **required** enum selecting the adapter (ADR-0011,
SPEC-0006): `crush`, `claude-code`, `codex`, `generic`. It has no default —
what a harness runs is the most consequential thing it declares, so a table
that omits the key is a config error rather than an agent nobody asked for:

```
harness "web": missing required key "harness" (want one of: crush, claude-code,
codex, generic — use "generic" with args = ["-c", "…"] for an arbitrary command)
```
 The
adapter owns both the tool-specific behaviour (trajectory discovery) and the
executable a long-running harness runs; it also synthesizes the CLI-specific
argv for prompt one-shots. `generic` means "none of the above" — its executable is `sh`, so an
arbitrary command is expressed as `args = ["-c", "<command line>"]`, and it
reports no native trajectory (scrollback-only). Note that `args` are handed to
`sh` itself: a bare `args = ["/usr/local/bin/thing"]` asks sh to *interpret*
that file as a shell script, which fails on a compiled binary.

```toml
[harness.my-agent]
harness = "claude-code"
```

## Trajectory harvesting & facade scope

- `harvest_trajectory = true` (default `false`) exposes this harness's session
  transcripts read-only through the MCP facade (`list_trajectories` /
  `get_trajectory`). Opt-in because a transcript may contain secrets the
  harnessed program printed itself (ADR-0008).
- `mcp_allow` (default `["read"]`) lists the operations this harness may
  invoke through the MCP facade; include `"write"` to permit
  `harness_start/stop/restart` through the facade. **Global config only** —
  project files reject the key so a cloned repository cannot grant itself write
  authority over the fleet.

## Daemon settings (`[daemon]`)

```toml
[daemon]
watch_config = true                   # auto-reload on config file changes (default true)
otel_endpoint = "https://cairn.stump.wtf"   # OTLP/HTTP trace export (optional)
```

`otel_endpoint` is an OTLP/HTTP URL the daemon ships agent traces to. Any
OTLP-compatible endpoint works (Honeycomb, Tempo, Jaeger, Grafana, or a Cairn
instance exposing OTLP): the daemon builds OTel traces from harvested sessions
(`harvest_trajectory = true`) and POSTs standard OTLP JSON to
`<endpoint>/v1/traces`.

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

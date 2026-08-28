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

An **agent one-shot** replaces `args` with a `prompt` (or a `prompt_file`) —
see the next section.

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
| `prompt` | the agent instruction. Mutually exclusive with `args` and with `prompt_file`; stored verbatim (never placeholder-expanded) and synthesized into the agent argv at spawn from the same `harness` adapter |
| `prompt_file` | path to a file whose contents are the instruction — the alternative to an inline `prompt` for anything too long for one TOML line. See below |
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

### Prompts that live in a file

A TOML basic string carries no raw newline, so a prompt of any real length ends
up as one unreadable line. `prompt_file` names a file instead:

```toml
[harness.blog-sweep]
harness = "claude-code"
prompt_file = "~/.config/prompts/blog-sweep.md"
model = "claude-opus-5"
schedule = "0 9 * * 1"
```

- **Mutually exclusive with `prompt`**, and either one satisfies the "requires
  `prompt`" rule for `model`, `auto_accept`, `max_turns`, `quiet` and
  `schedule`.
- **The path is resolved at load; the file is read at spawn.** `harness.toml`
  stores the path, so editing the prompt takes effect on the next run with no
  reload — and config writers (the TUI edit form) round-trip the path rather
  than inlining the document.
- **A leading `~` expands, and a relative path resolves against the file that
  declared it** — the config file's own directory, the `harness_d` file's
  directory, or the project root in a project `harness.toml`. This is stricter
  than `workdir`/`env_file` on purpose: the daemon runs with an arbitrary
  working directory, so a cwd-relative prompt would mean different things to
  the CLI and the daemon.
- **A missing, unreadable, or empty file fails the config load** with a located
  error, and is re-checked at spawn. Unlike `env_file`, it is not optional: a
  harness with no instruction has nothing to run, and a scheduled one firing
  into an empty prompt would look like a successful no-op.

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

- Requires `prompt` or `prompt_file` — only agent one-shots can be scheduled.
- At each firing the harness starts if it is not already running; **overlapping
  firings are skipped, not stacked**.
- The run exiting is terminal for that firing; the restart policy applies only
  to abnormal exit, so only `restart = "no"` (the prompt default) and
  `"on-failure"` are accepted here.
- Mutually exclusive with `enabled = true` and with profile membership.
- Global config only — project files reject the key.

`harness list` marks a scheduled harness inline — a clock glyph in place of the
state glyph, and the next firing appended to its description (`· in 4h3m`).
`harness describe` adds the cron spec and the absolute next-run time. The
[cockpit](./tui#the-dashboard) tags the row `(scheduled)` with the same
countdown, and carries the cadence on the row's sub-line.

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

## Drop-in harness files (`harness_d`)

Instead of editing one growing `harness.toml`, point at a directory and add or
remove a harness one file at a time:

```toml
[server]
harness_d = "~/.config/harness/harness.d"
```

Then drop files like `~/.config/harness/harness.d/backup-daily.toml`:

```toml
[harness.backup-daily]
harness = "crush"
prompt = "run the daily backup"
schedule = "0 2 * * *"
```

- Only `*.toml` files are read; anything else in the directory is ignored.
- Files are merged in lexicographic order, so a numeric prefix (`10-`, `20-`)
  pins the order if you care about it.
- A drop-in may contain **`[harness.*]` tables only**. `[server]`,
  `[profile.*]`, `[daemon]`, and bare `[name]` tables are rejected with the
  offending file and line.
- Duplicate harness names — between two drop-ins, or with the main file — are
  rejected rather than silently overwritten.
- A leading `~` expands to your home directory, and a relative path resolves
  against the directory holding `harness.toml` (not the daemon's working
  directory, which under systemd is not the same thing).
- The directory must exist. A missing `harness_d` fails the config load rather
  than being treated as empty, so a typo cannot silently drop every drop-in.
- `[profile.*]` tables in the main file may reference drop-in harnesses.

Drop-in files are **not** watched for changes. Auto-reload (`watch_config`)
watches `harness.toml` only, so after adding or removing a drop-in run
`harness reload` (or touch the main config).

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

## Environment variables

Process-level settings — where the socket lives, how loud the log is, whether
the SSH server is on — can come from the environment instead of a flag or this
file. That is what makes Harness deployable as a container or a systemd unit
without baking in a config file.

| Variable | Flag | Type | Default |
|---|---|---|---|
| `HARNESS_SOCKET` | `--socket` | path | `$XDG_RUNTIME_DIR/harness.sock` |
| `HARNESS_CONFIG` | `--config` | path | `$XDG_CONFIG_HOME/harness/harness.toml` |
| `HARNESS_JSON` | `--json` | bool | `false` |
| `HARNESS_LOG_LEVEL` | `--log-level` | `debug`/`info`/`warn`/`error` | `info` |
| `HARNESS_LOG_FILE` | `--log-file` | path | stderr |
| `HARNESS_SCROLLBACK` | `--scrollback` | int | 10000 |
| `HARNESS_SSH` | `--ssh` | bool | `false` |
| `HARNESS_SSH_LISTEN` | `--ssh-listen` | `host:port` | unset |
| `HARNESS_WATCH_CONFIG` | — | bool | `true` |

Booleans accept `1`, `0`, `true`, `false`, `yes`, `no`, `on`, `off`.

### Precedence

An explicit flag beats an environment variable, which beats this file, which
beats the compiled default:

```
--socket /tmp/y.sock   >   HARNESS_SOCKET=/tmp/x.sock   >   harness.toml   >   default
```

"Explicit" means you actually typed the flag. Leaving a flag alone does not
suppress an environment variable, so `HARNESS_LOG_LEVEL=debug harness daemon run`
starts at debug even though `--log-level` has a default of `info`.

An exported-but-empty variable is treated as unset, because
`export HARNESS_SOCKET=$SOME_UNSET_VAR` is a shell accident rather than a request
for an empty socket path.

A bad value is a hard failure, never a silent fallback:

```
HARNESS_SCROLLBACK=lots harness daemon run
# HARNESS_SCROLLBACK: invalid value "lots": expected an integer
```

### Which source won?

`harness doctor` reports every setting with the source that supplied it, so you
never have to guess:

```bash
harness doctor
```

```
SETTING       SOURCE    VALUE
socket        env       /run/harness.sock
config        default   /home/you/.config/harness/harness.toml
log-level     flag      debug
ssh           file      true
```

`harness doctor --json` carries the same data under `.settings` for scripts.

### Harnesses stay in the file

There is no environment variable that defines a harness or a profile. Those are
collections, and mangling them into variable names is not reliably reversible —
`HARNESS_HARNESS_CLAUDE_SRC_WORKDIR` cannot tell you whether the harness is
`claude-src` or `claude_src`. So:

**A container can run a fully-configured daemon with no `harness.toml` at all,
but it cannot define a harness without one.** A missing file is not an error;
the daemon comes up and reports zero harnesses. A file that exists but does not
parse is still an error.

### Secrets do not go here

No `HARNESS_*` variable takes a token, key, or password. Per-harness secrets
belong in `env_file`, which is read by the harness rather than by Harness
itself. See [Remote access](./remote) for the SSH key handling.

`HARNESS_DETACH_READY_FD` is reserved: it is internal plumbing between
`harness daemon --detach` and the process it forks, not a setting.

### systemd

```ini
[Service]
Environment=HARNESS_SOCKET=/run/harness/harness.sock
Environment=HARNESS_LOG_LEVEL=info
Environment=HARNESS_CONFIG=/etc/harness/harness.toml
ExecStart=/usr/local/bin/harness daemon run
```

## After editing

`harness reload` re-reads the config and reconciles running harnesses without a
daemon restart. `harness doctor` verifies the file parses and the daemon agrees
with it.

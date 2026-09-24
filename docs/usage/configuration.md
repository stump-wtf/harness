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
| `export_telemetry` | publish this harness's agent activity to the [telemetry](#telemetry-export-telemetry) destinations: `true` opts in, `false` opts out (and beats `export_all`), unset follows `[telemetry] export_all`. A project file may set only `false`. Applies on reload |

An **agent one-shot** replaces `args` with a `prompt` (or a `prompt_file`) —
see the next section.

A bare `[name]` table is accepted for backward compatibility, but the
`[harness.<name>]` form is preferred (ADR-0006).

## Agent one-shot harnesses

Give a harness a `prompt`, and the daemon synthesizes the agent invocation at
spawn time from the harness's adapter (see the table below). This turns a
harness into a one-shot agent run:

```toml
[harness.deploy-check]
harness = "claude-code"
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

Each adapter maps those fields onto its own CLI. A field the CLI has no flag for
is dropped, not emulated:

| `harness` | Synthesized command | Ignored fields |
|-----------|---------------------|----------------|
| `crush` | `crush [--yolo] run [--quiet] [--model M] <prompt>` | `max_turns` (Crush has no turn cap) |
| `claude-code` | `claude -p [--dangerously-skip-permissions] [--model M] [--max-turns N] --verbose --output-format stream-json <prompt>` | `quiet` (`-p` is already headless) |
| `codex` | `codex exec [--model M] [--full-auto] <prompt>` | `quiet`, `max_turns` |
| `generic` | same as `crush` | same as `crush` |

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
[harness.fleet-sweep]
harness = "crush"
prompt = "check all services and report anything unhealthy"
auto_accept = true
schedule = "CRON_TZ=UTC 0 */6 * * *"   # every 6 hours
description = "fleet health sweep"
```

For a full walkthrough — prompt design, run history, and reading outcomes — see
the [Scheduled sweeps guide](/guides/scheduled-sweeps).

Rules:

- Requires `prompt` or `prompt_file` — only agent one-shots can be scheduled.
- At each firing the harness starts if no run is in flight; a firing that lands
  mid-run follows `on_overlap` (below), and never stacks a second process.
- The run exiting is terminal for that firing; the restart policy applies only
  to abnormal exit, so only `restart = "no"` (the prompt default) and
  `"on-failure"` are accepted here.
- Mutually exclusive with `enabled = true` and with profile membership.
- Global config only — project files reject the key.

### Time zones

By default an expression runs in the daemon's local zone. Prefix it with
`CRON_TZ=<zone>` (or `TZ=<zone>`) to pin it to an IANA zone regardless of where
the daemon runs:

```toml
schedule = "CRON_TZ=UTC 0 9 * * *"   # 09:00 UTC on every host
```

Across daylight-saving changes, a schedule that names a time of day runs once
per day: a time the clock skips (02:30 on spring-forward day) runs just after
the jump, and a time the clock repeats runs only the first time. A schedule that
fires every hour, or an `@every` interval, keeps running once per real hour.

### Sleep, outages, and `catch_up`

The daemon checks the wall clock every second rather than setting a timer, so a
laptop that sleeps through a window notices the moment it wakes, and a daemon
that starts after an outage notices at boot. A window noticed more than a minute
late is **missed**, and `catch_up` decides what happens:

```toml
[harness.nightly-sweep]
harness = "crush"
prompt = "…"
schedule = "CRON_TZ=UTC 0 3 * * *"
catch_up = true   # default false
```

- `catch_up = true` — run **once** on wake or boot, however many windows were
  missed.
- `catch_up = false` (default) — run nothing and log a `scheduled run MISSED`
  warning in the daemon log naming the harness, the first and last missed
  windows, and how many there were. The next window fires normally.

`catch_up` requires `schedule`. The daemon records the last window it decided in
`state.json`, so a restart never runs the same window twice. A missed window is
also a `missed` entry in the harness's run history.

### Runs: history, logs, timeout, overlap

Every run of a scheduled harness gets a numbered record and a log of its own:

```toml
[harness.nightly-sweep]
harness = "crush"
prompt = "…"
schedule = "CRON_TZ=UTC 0 3 * * *"
timeout = "45m"        # default "1h"; "0" = no limit
on_overlap = "queue"   # default "skip"; or "replace"
keep_runs = 30         # default 20
```

- **History** lives in `state.json`: run id, trigger (`schedule`, `manual`,
  `catch_up`), start, end, exit code, and an outcome — `success`, `failed`,
  `timed_out`, `skipped`, `replaced`, `missed`, `cancelled`, or `interrupted`.
  Firings that start nothing (skipped, missed) are recorded too. Run ids never
  repeat, and a run the daemon crashed under reads `interrupted` on the next
  boot.
- **Logs** are at `$XDG_STATE_HOME/harness/jobs/<name>/<run_id>.log` — the run's
  output history and lifecycle lines, alongside the usual harness log. The oldest
  records beyond `keep_runs`, and their logs, are pruned together.
- **`timeout`** stops a run that goes on too long: SIGTERM, then SIGKILL after
  the stop grace. The run is `timed_out` and the harness shows `failed`.
- **`on_overlap`** decides a firing that lands while a run is still going:
  `skip` records it and does nothing; `queue` runs it once the current run
  finishes (holding at most one); `replace` stops the current run and starts the
  new one.

All three keys require `schedule`. Read history and per-run logs from the CLI
with `harness jobs`, `harness runs <name>` and `harness logs <name> --run N`, and
run a job now with `harness trigger <name>` — see
[CLI → Scheduled jobs](./cli#scheduled-jobs).

`harness list` gives the schedule its own columns — `SCHEDULE` (the cadence) and
`NEXT` (the countdown) — and reads the state as `⏱ armed`, since a cron job
between firings is waiting rather than switched off. An unscheduled harness shows
an em dash in both columns.
`harness describe` adds an `armed` row, the cron spec and the absolute next-run
time. A
zone-prefixed schedule's cadence carries the zone (`daily 09:00 UTC`). The
[cockpit](./tui#the-dashboard) tags the row `(scheduled)` with the same
countdown, and carries the cadence on the row's sub-line.

## Operating hours

`operating_hours` gives a **resident** harness a weekly window it is allowed
to run in — ADR-0019's daemon-owned alternative to an external launchd/cron
timer calling `harness start`/`harness stop`. Outside its windows the gate
holds the harness down (stops the process, `enabled` untouched) and starts it
again when a window opens:

```toml
[harness.night-owl]
harness = "claude-code"
args = ["--remote-control", "--continue"]
enabled = true
operating_hours = "TZ=America/Los_Angeles Mon-Fri 09:00-13:00"
hours_shutdown = "graceful"          # default; or "immediate"
hours_shutdown_timeout = "15m"       # default; the most a graceful close may overrun
```

Rules:

- Global config only — project files reject the key, like `schedule`.
- Mutually exclusive with `schedule`: a scheduled one-shot is already
  time-gated by its own cron expression, and two time gates on one harness
  would disagree.
- `enabled` still means "the operator wants this running". Hours sit beside
  it, never overwrite it: a held harness stays `enabled = true`, and
  `harness stop` always stops the harness and clears `enabled`, whatever
  state it is in.

### Grammar and time zones

`operating_hours` is one string: an optional `TZ=<zone>` or `CRON_TZ=<zone>`
prefix (same resolution as `schedule`'s — defaults to the daemon's own zone
when omitted), then one or more `;`-separated windows of
`[day-spec] HH:MM-HH:MM`:

| Written | Means |
| --- | --- |
| `09:00-13:00` | every day, 09:00 up to (not including) 13:00 |
| `Mon-Fri 09:00-13:00` | weekdays |
| `Mon-Fri 09:00-12:00; Mon-Fri 13:00-17:00` | a lunch break |
| `Sat,Sun 10:00-12:00` | a day list |
| `Sun-Thu 22:00-02:00` | overnight — an end at or before its start runs into the next day |
| `Mon-Sun 00:00-24:00` | always in hours (valid, and `harness doctor` warns that it gates nothing) |

A blank value, an unknown day or zone, or a window whose start equals its end
fails config load with an error naming the harness and the key.

### Closing: graceful by default

`hours_shutdown = "graceful"` (the default) lets the harness finish its
current turn before stopping it, capped by `hours_shutdown_timeout` (default
`15m`); `"immediate"` stops it at the close without waiting. Either way the
stop is not a crash: no restart, no restart-count increment, no flap, and
`enabled` survives. Graceful shutdown depends on the daemon reading turn-end
markers from the harness's own agent-trace; a harness with nothing
attributable to it (a `generic` adapter, or no `workdir`) always closes
immediately, and `harness doctor` warns when `hours_shutdown = "graceful"` is
set on one anyway.

### Overrides: an after-hours lease

`harness start <name>` outside a gated harness's hours starts it under a
bounded **after-hours lease** — one hour by default, `harness start <name>
--for 3h` for a chosen length. The gate stops it at the lease's end exactly as
it would at a close; if hours open first, the lease simply ends and the
harness keeps running as an ordinary in-hours one. See
[CLI → Operating hours](./cli#operating-hours) for how a gated harness's
state, lease and close in flight show on every listing surface (`off-hours`,
`closing`, `lease until …`) — no extra column, the existing STATE/SCHEDULE/NEXT
columns carry it.

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

## Model routing and provider failover

When you pin a harness to a single model on a single provider, a quota wall or
provider outage stops every harness sharing it — simultaneously. The restart
policy does not help: the process is healthy, and the API is refusing every
call.

### Where it shows up

Not in `harness list` — a harness whose provider has hit a quota limit shows
`running`. The failure is visible in:

- `harness logs <name>` — repeated provider errors in the output stream
- The provider's dashboard — quota exhaustion or rate limits

Supervision cannot see upstream refusals; it only knows whether the process
restarted.

### Mitigation: an OpenAI-compatible gateway

A gateway in front of your providers (LiteLLM is one example) lets multiple
deployments share a single `model_name`, with failover and retries between them:

```yaml
# Example: LiteLLM proxy config
model_list:
  - model_name: glm-5.3-balanced         # deployment 1
    litellm_params:
      model: openai/glm-5.3
      api_base: https://provider-a.example.com/v1
      api_key: os.environ/A_KEY

  - model_name: glm-5.3-balanced         # deployment 2
    litellm_params:
      model: openai/glm-5.3
      api_base: https://provider-b.example.com/v1
      api_key: os.environ/B_KEY

router_settings:
  routing_strategy: simple-shuffle
  num_retries: 2
  allowed_fails: 2
  cooldown_time: 300  # re-probe after 5 minutes
  fallbacks:
    - glm-5.3-balanced: ["<cheap-tier-model>"]
```

The harness pins `model = "litellm/glm-5.3-balanced"` and knows nothing about
providers. Failover happens inside each request, and `cooldown_time` automatically
retries the benched provider after its quota resets.

### Per-request vs per-worker failover

Running one worker per provider is **not** equivalent:

- Per-worker failover burns the restart budget on each dead worker until it
  hits the `failed` state (terminal, needs a human).
- Capacity halves while one worker recovers.
- A gateway retries the sibling deployment inside the same request and keeps
  every worker productive.

### The trade

A gateway is a dependency on the hot path. Failing over from a
subscription-metered provider to a pay-per-token one **removes a fail-closed
spend cap**. That is the right trade for an always-on agent and the wrong one
for an unattended cron job that nobody is watching.

### Cost and observability

One gateway also means one set of spend and latency metrics instead of
per-provider guesswork — useful even when quota exhaustion is not your primary
risk.

## Trajectory harvesting & facade scope

:::note Reserved, not yet active

The daemon does not run the MCP facade (ADR-0010) today. These keys are
validated and kept in config — the TUI edit form round-trips them — but setting
them changes nothing at runtime yet. They are **not** the telemetry opt-in:
exporting to a collector is a different audience, gated by `export_telemetry`
(see [Telemetry export](#telemetry-export-telemetry)).

:::

- `harvest_trajectory = true` (default `false`) is the opt-in for exposing this
  harness's session transcripts read-only through the planned MCP facade
  (`list_trajectories` / `get_trajectory`). Opt-in because a transcript may
  contain secrets the harnessed program printed itself (ADR-0008).
- `mcp_allow` (default `["read"]`) lists the operations this harness will be
  permitted to invoke through that facade; `"write"` would permit
  `harness_start/stop/restart`. **Global config only** — project files already
  reject the key, so a cloned repository cannot grant itself write authority
  over the fleet.

## Daemon settings (`[daemon]`)

```toml
[daemon]
watch_config = true   # auto-reload on config file changes (default true)
```

`watch_config` is the only daemon setting.

`otel_endpoint` has been **removed**: it was accepted but never exported
anything. A config that still sets it fails to load, with an error pointing
here. To export agent telemetry, add a [`[telemetry]`](#telemetry-export-telemetry)
table with `traces = true` (and/or `logs = true`) and set `endpoint` there or
`OTEL_EXPORTER_OTLP_ENDPOINT` in the daemon's environment. It is deliberately
not an alias: an inert line in an old config must not start publishing agent
transcripts on upgrade.

## Telemetry export (`[telemetry]`)

The daemon can export what its supervised agents do — every tool call and every
mark (user message, compaction, subagent launch, provider error) — as **OTLP
logs**, **OTLP traces**, and a **local JSONL events file**. The
[production observability guide](./production-observability) shows collector,
Grafana (Loki/Tempo), Honeycomb and Vector/Fluent Bit setups; this is the key
reference (ADR-0022, SPEC-0015).

Nothing is exported without **two** consents: a destination in this table, and a
harness that opted in (`export_telemetry = true` on the harness, or
`export_all = true` here). Environment variables alone never enable export — an
`OTEL_EXPORTER_OTLP_ENDPOINT` inherited from your shell is only mentioned in the
daemon log. With no destination configured the daemon opens no socket, creates
no file and subscribes to nothing.

```toml
[telemetry]
logs   = true                     # OTLP logs: one record per tool call or mark
traces = true                     # OTLP traces: one trace per agent session
events_file = "~/.local/state/harness/events.jsonl"  # local JSONL; empty = off

export_all   = false              # every harness without export_telemetry contributes
omit_prompts = false              # replace prompt text with "[prompt omitted]"

endpoint    = "http://127.0.0.1:4318"   # OTLP/HTTP base; /v1/logs and /v1/traces are appended
env_file    = "/etc/harness/otel.env"   # OTEL_EXPORTER_OTLP_* (headers/credentials go here)
compression = "none"              # "none" | "gzip"
timeout     = "10s"               # per request

queue_size       = 2048           # per signal (records, spans or lines); oldest dropped when full
batch_size       = 512            # max per request; must not exceed queue_size
batch_interval   = "5s"           # flush a partial batch after this long
idle_flush       = "5m"           # export a session's trace after this long without items
shutdown_timeout = "5s"           # total budget for the shutdown flush
events_file_max_mb = 100          # rotate the events file (by rename) at this size
events_file_keep   = 5            # rotated files kept: events.jsonl.1 … .5
```

| Key | Default | Meaning |
|-----|---------|---------|
| `logs` | `false` | enable the OTLP logs signal |
| `traces` | `false` | enable the OTLP traces signal |
| `events_file` | `""` (off) | path of the JSONL sink; `~` expands, relative paths resolve against the config file's directory. File `0600`, created directories `0700` |
| `export_all` | `false` | harnesses with no `export_telemetry` contribute |
| `omit_prompts` | `false` | replace user-message text with `[prompt omitted]` and drop the session title |
| `endpoint` | `""` | absolute `http`/`https` base URL with a host and **no userinfo** |
| `env_file` | `""` | file supplying the `OTEL_EXPORTER_OTLP_*` variables below; only those keys are read, and none are put into the daemon's environment, so supervised harnesses never inherit the credential. Missing or unreadable fails startup when an OTLP signal is on; group/world-readable is a warning |
| `compression` | `"none"` | `none` or `gzip` |
| `timeout` | `"10s"` | per-request timeout |
| `queue_size` | `2048` | per-signal bounded queue; at most `16384` |
| `batch_size` | `512` | units per request (or per events-file write); at most `4096` and at most `queue_size` |
| `batch_interval` | `"5s"` | partial-batch flush interval |
| `idle_flush` | `"5m"` | trace export after a quiet session; at least `batch_interval` |
| `shutdown_timeout` | `"5s"` | shutdown flush budget |
| `events_file_max_mb` | `100` | rotation size; at most `10240` |
| `events_file_keep` | `5` | rotated files kept; at most `100` |

Durations must be positive; integers must be positive and under their ceiling —
the queues are allocated at startup, so the ceiling is what stops a typo from
exhausting memory before the first item arrives. A **`headers` key is
rejected** — OTLP headers carry credentials, and `harness.toml` carries none
(ADR-0008). Put them in `OTEL_EXPORTER_OTLP_HEADERS`, preferably via `env_file`.

**Scope.** The table belongs to the daemon's global `harness.toml` only: a
project `harness.toml` and a `harness_d` drop-in both reject it. A project file
may set `export_telemetry = false` on its harnesses but not `true` — a cloned
repository does not get to publish its transcripts to your collector.

**Changes need a restart.** The exporters hold queues, connections and an open
file, so an edited `[telemetry]` table takes effect on the next daemon start; a
reload logs a warning saying so. Per-harness `export_telemetry` changes apply on
reload, from the next item.

### The OpenTelemetry environment

The standard OTLP exporter variables are honoured, from the daemon's process
environment first and then from `env_file`:

| Variable | Meaning |
|---|---|
| `OTEL_EXPORTER_OTLP_ENDPOINT` | base URL; `/v1/logs` and `/v1/traces` are appended |
| `OTEL_EXPORTER_OTLP_LOGS_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` | full signal URL, used as-is |
| `OTEL_EXPORTER_OTLP_HEADERS` (+ `_LOGS_` / `_TRACES_`) | `name=value,name2=value2`, values percent-decoded; merged, signal-specific wins per name |
| `OTEL_EXPORTER_OTLP_COMPRESSION` (+ `_LOGS_` / `_TRACES_`) | `none` or `gzip` |
| `OTEL_EXPORTER_OTLP_TIMEOUT` (+ `_LOGS_` / `_TRACES_`) | milliseconds |
| `OTEL_RESOURCE_ATTRIBUTES` | extra resource attributes (cannot override `service.name`) |
| `OTEL_SDK_DISABLED=true` | turns both OTLP signals off |

Precedence, per setting and per signal: **signal-specific variable > generic
variable > `[telemetry]` > default**. An exported-but-empty variable counts as
unset. `OTEL_EXPORTER_OTLP_PROTOCOL` set to anything but `http/json` is ignored
with a warning: Harness speaks only OTLP/HTTP JSON. An enabled OTLP signal with
no endpoint anywhere refuses to start.

At startup the daemon logs one line per signal with the resolved endpoint, the
source of each setting (`env`, `env_file`, `file`, `default`), and each header
as its name plus a SHA-256 fingerprint — never a value. `harness doctor` reports
the same under a `TELEMETRY` section (`.telemetry` in `--json`), resolved in the
shell you run it from: a daemon started by systemd with its own
`EnvironmentFile`, or as a user who can read an `env_file` you cannot, may
resolve differently, and the daemon's startup line is the authority.

### What is exported

Every string is redacted with the same rules as `harness logs` before it is
queued, and capped (a record body at 4 KiB, a span name at 256 bytes, other
attributes at 1 KiB, at most 32 targets). Tool output and file contents are
never read, so never exported. A failed tool call is `WARN`; a provider or model
error mark is `ERROR`, so an alert on `ERROR` means a turn failed. Log records
and spans for the same item share `traceId`/`spanId`, and every item carries
`agent.item.id` as a deduplication key.

A dead collector costs counted, dropped telemetry and nothing else: failed
requests retry with backoff (429/502/503/504 and network errors, honouring
`Retry-After`) for up to five minutes per batch, queues drop their oldest units
when full, and failures reach the daemon log at most once a minute per signal.

## Merge train (`[mergetrain]`)

The merge train lands approved pull requests one at a time. For each one it
builds `train/<pr>` (`main` plus a squash of the PR), waits for CI on that exact
commit, squash-merges, and then verifies that the tree that landed is the tree
that was tested. It is off unless you turn it on, and even then it starts in
`report` mode, which builds and tests trains but writes nothing to any pull
request. See ADR-0032 and SPEC-0025.

```toml
[mergetrain]
enabled = true                               # default false
mode = "report"                              # or "merge"; default "report"
repos = ["stump.wtf/harness"]                # required when enabled
base_branch = "main"                         # default "main"
poll_interval = "60s"                        # default 60s, minimum 5s
ci_timeout = "30m"                           # default 30m
forge_base_url = "https://gitea.stump.rocks" # required when enabled
forge_token_env = "HARNESS_MERGETRAIN_TOKEN" # the variable's NAME, required when enabled
```

- **The token is never in this file.** `forge_token_env` names an environment
  variable in the daemon's environment, and a `token` key is refused. The
  daemon refuses to start if the train is enabled and that variable is empty.
- **Global only.** A project `harness.toml` or a `harness_d` drop-in that
  contains `[mergetrain]` is refused.
- **One train per repo.** Each repo's driver holds a lock under
  `$XDG_STATE_HOME/harness/mergetrain/`, so a second daemon on the same host
  skips that repo rather than racing it.
- **Restart to apply.** A change to `[mergetrain]` takes effect at the next
  daemon restart.

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

### Metrics listener

The daemon serves Prometheus metrics on `127.0.0.1:10229` by default,
independent of `enabled` above. The two keys below live in `[server]`:

```toml
[server]
# Changing either key needs a daemon restart: `harness reload` re-reads
# harness definitions and never rebinds a listener.
metrics_listen = "0.0.0.0:10229"                          # default 127.0.0.1:10229; "off" disables
metrics_token_file = "~/.config/harness/metrics.token"    # required off loopback
```

| Key | Default | Meaning |
|---|---|---|
| `metrics_listen` | `127.0.0.1:10229` | `host:port` for `GET /metrics`, or `"off"`. A non-loopback address without a token makes the daemon refuse to start. |
| `metrics_token_file` | none | A file holding the bearer token scrapers must send. harness.toml holds its path, never the token (ADR-0008). |

See [Metrics](./metrics) for the series, alert rules and scrape config.

### Webhook listener

`[webhook.*]` sources are served on an HTTP listener of their own, separate
from the SSH front door and the metrics listener. It is **off** unless an
address names it; declaring a `[webhook.*]` table does not open a port.

```toml
[server]
webhook_listen = "127.0.0.1:9080"   # or --webhook-listen / HARNESS_WEBHOOK_LISTEN
# Both or neither; with both set the listener serves HTTPS only.
# webhook_tls_cert_file = "/etc/harness/webhook.crt"
# webhook_tls_key_file  = "/etc/harness/webhook.key"

[webhook.ci]
verify = "bearer"
env_file = "~/.config/harness/triggers.env"   # CI_HOOK_TOKEN=...
secret = "${CI_HOOK_TOKEN}"

[harness.on-ci]
harness = "claude-code"
prompt_file = "~/.config/harness/prompts/on-ci.md"
triggers = ["webhook.ci"]
```

```sh
curl -X POST -H "Authorization: Bearer $CI_HOOK_TOKEN" \
     -H 'Content-Type: application/json' -d '{"status":"failed"}' \
     http://127.0.0.1:9080/hooks/ci
# 202 {"webhook":"ci","event_id":"wh-…","decision":"fired",
#      "firings":[{"harness":"on-ci","decision":"started","run_id":4}]}
```

The listener serves exactly `POST /hooks/<name>` and `GET /healthz` (`ok`).
It never redirects and never serves files.

| Status | When |
|---|---|
| `202` | Verified. `decision` is `fired` with one entry per bound harness (`started`/`skipped` carry `run_id`; `queued` does not), `ignored` when `events` filtered it out, or `duplicate` when its delivery ID already fired (with the first firing's `event_id`). `ignored` and `duplicate` fire nothing and make no run record. The response never waits for a run. |
| `401` | Missing, malformed or wrong credential. Every cause gets the same body; the log names the route and peer, never the value presented. |
| `404` | The name is unknown, disabled, or bound by no harness. All three are byte-identical, so routes cannot be enumerated. |
| `405` | Any method other than `POST` on `/hooks/<name>` (or other than `GET` on `/healthz`). |
| `413` | The body is over `max_body`. Checked before the credential is. |
| `429` | The route is over its `rate_limit`. `Retry-After` says how many seconds until the next token. |
| `503` | 64 deliveries are already in flight. |

Every response carries `Content-Security-Policy`, `X-Frame-Options`,
`X-Content-Type-Options`, `Referrer-Policy` and `Cache-Control: no-store`,
plus `Strict-Transport-Security` when the listener terminates TLS itself.
Slow clients are cut off: 10 s to send headers, 30 s to read the request,
30 s to write the response, 60 s idle, 64 KiB of headers.

After verification, each delivery goes through three filters, in order:

1. **`events`**: an event name not in the list (or no event name at all) is
   `202 ignored`.
2. **De-duplication**: when the scheme has a delivery header (`delivery_header`
   for `bearer`/`hmac-sha256`, the preset's otherwise) and the delivery
   carries one, an ID that already fired on this route in the last 24 hours —
   among the last 1024 kept — is `202 duplicate`. The set is in memory and
   starts empty when the daemon does.
3. **`rate_limit`**: a token bucket of `n` refilling `n` per unit, so `"2/m"`
   allows a burst of two, then one every 30 s. Only a delivery that verified,
   passed `events` and is not a duplicate spends a token, so forged traffic
   cannot use up the real sender's budget. A `429` is not remembered as seen,
   so the sender's retry fires. `"0"` turns the limit off.

A reload keeps a route's bucket and de-duplication set as long as its name and
`rate_limit` are unchanged; changing `rate_limit` starts both afresh.

#### Verification schemes

`verify` picks how a delivery proves it came from whoever holds the secret.
The check runs over the body bytes exactly as they arrived, before anything
parses them. The four presets fix their header names, so a GitHub, Gitea or
GitLab route is three lines:

```toml
[webhook.gh]
verify = "github"
env_file = "~/.config/harness/triggers.env"   # GH_HOOK_SECRET=...
secret = "${GH_HOOK_SECRET}"
```

| `verify` | Credential the sender presents | Event header | Delivery header |
|---|---|---|---|
| `bearer` | `Authorization: Bearer <secret>` | `event_header` | `delivery_header` |
| `hmac-sha256` | hex HMAC-SHA256 of the body, keyed by the secret, in `signature_header` after `signature_prefix` | `event_header` | `delivery_header` |
| `github` | `X-Hub-Signature-256: sha256=<hex HMAC-SHA256>` | `X-GitHub-Event` | `X-GitHub-Delivery` |
| `gitea` | `X-Gitea-Signature: <hex HMAC-SHA256>` | `X-Gitea-Event` | `X-Gitea-Delivery` |
| `gitlab` | `X-Gitlab-Token: <secret>` | `X-Gitlab-Event` | `X-Gitlab-Event-UUID` |
| `standard-webhooks` | not implemented yet; see below | — | — |

For `github` and `gitea`, set the same value as the forge hook's **Secret**;
for `gitlab`, as its **Secret token**. A missing signature header, two of
them, a wrong prefix, anything but 64 hex digits, or a MAC that does not
match is a `401`. The event header becomes the event name `events` matches,
and the delivery header becomes the run's `event_id`. The event file carries
only `Content-Type`, `User-Agent` and those two headers, never the signature
or token.

A `gitlab` token is a shared password, not a signature: it proves the sender
knows the secret, but does not cover the body. Prefer `gitea`- or
`github`-style signing where the sender offers it, and TLS either way.

:::warning standard-webhooks is not verified yet
A route using `verify = "standard-webhooks"` loads, logs a warning, and
answers **every** delivery `401` until its verifier lands. It never accepts
an unverified delivery.

A non-loopback `webhook_listen` without TLS starts with a warning: bearer and
GitLab tokens then cross the network in cleartext. Bind loopback behind a
TLS-terminating proxy, or set both TLS files.
:::

Changing `webhook_listen` or the TLS files needs a daemon restart; a reload
logs that a restart is required and keeps serving on the old settings. Route
changes (a source added, disabled, rebound) apply on reload. On shutdown the
listener stops accepting and gives in-flight deliveries 5 seconds.

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
| `HARNESS_WEBHOOK_LISTEN` | `--webhook-listen` | `host:port` | unset (no webhook listener) |
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

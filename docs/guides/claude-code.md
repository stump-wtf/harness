---
title: "Claude Code under Harness"
sidebar_label: "Claude Code"
sidebar_position: 5.5
---

# Claude Code under Harness

Harness runs [Claude Code](https://claude.com/claude-code) two ways, and they
behave differently enough that most surprises come from mixing them up:

| | Resident harness | One-shot (prompt harness) |
|---|---|---|
| Config | `harness = "claude-code"` plus `args` | `harness = "claude-code"` plus `prompt` or `prompt_file` |
| What runs | interactive `claude`, in a terminal you can attach to | `claude -p`: one turn, then it exits |
| Fired by | the daemon keeping it up (`enabled`, `restart`) | a `schedule`, a trigger, or `harness trigger NAME` |
| Wakes on a Switchboard doorbell | yes, with the channels flag (below) | no, it has exited. A `[channel.*]` trigger starts a new run instead. |

## What Harness actually runs

### A resident harness: `claude` plus your `args`, verbatim

The adapter supplies the executable and appends your `args` unchanged. Harness
adds no flags of its own:

```toml
[harness.claude-main]
harness = "claude-code"
args = ["--continue"]
workdir = "~/src/my-project"
restart = "on-failure"
restart_delay = 30
enabled = true
```

runs `claude --continue` in `~/src/my-project`. The keys that shape a one-shot's
command (`model`, `auto_accept`, `max_turns`) are rejected on a resident harness;
pass the flags through `args` yourself, such as `args = ["--model", "claude-opus-5"]`.

### A one-shot: `claude -p`, with flags from your config

Give the harness a `prompt` (or `prompt_file`) instead of `args`, and Harness
builds the command:

```
claude -p [--dangerously-skip-permissions] [--model M] [--max-turns N] --verbose --output-format stream-json <prompt>
```

| Key | Adds |
|-----|------|
| `auto_accept = true` | `--dangerously-skip-permissions` |
| `model = "M"` | `--model M` |
| `max_turns = N` | `--max-turns N` (omitted when unset or `0`) |
| always | `--verbose --output-format stream-json`, so `harness logs` can read the run |

`--dangerously-skip-permissions` comes **only** from `auto_accept`, and it means
what it says: **the agent may use any tool, with no prompt**, including shell
commands and file writes. Nobody is attached to approve a tool call during a
scheduled run, so a one-shot that uses tools usually needs it. Scope what the
run can reach instead: its `workdir`, the credentials in its `env_file`, and the
MCP servers its workdir configures.

```toml
[harness.nightly-digest]
harness = "claude-code"
prompt = "Summarize yesterday's merged pull requests in DIGEST.md"
model = "claude-sonnet-5"
auto_accept = true
max_turns = 30
workdir = "~/agents/nightly-digest"
env_file = "~/.config/harness/env/nightly-digest.env"
schedule = "CRON_TZ=UTC 0 6 * * *"
timeout = "20m"
```

runs `claude -p --dangerously-skip-permissions --model claude-sonnet-5
--max-turns 30 --verbose --output-format stream-json "Summarize yesterday's…"`
at 06:00 UTC. [Scheduled sweeps](./scheduled-sweeps) covers the schedule, run
history and how to tell a good run from a bad one.

## A one-shot cannot take extra flags

`prompt` and `args` are mutually exclusive, and so are `prompt_file` and `args`.
A config that sets both fails to load:

```
harness "reviewer": "prompt" and "args" are mutually exclusive (args configure a long-running harness; the agent argv is synthesized at spawn)
```

So today a one-shot **cannot** carry the channels flag, `--allowedTools`, an
`--mcp-config`, or `--append-system-prompt`. Three one-shot keys for exactly
this are **coming**: `system_prompt_file`, `mcp_config` and `allowed_tools`
([SPEC-0018](/specs/stack-installer/spec), REQ-11).

### Where a one-shot's persona lives

Until then, put everything the run needs in its `workdir`. Claude Code reads it
there on every start, `-p` included:

```
~/agents/reviewer/
├── CLAUDE.md                 # the persona: who it is, what to do, what never to do
├── .mcp.json                 # MCP servers for this directory
└── .claude/
    └── settings.json         # tool permissions and MCP server approval
```

`.mcp.json`, where Claude Code expands `${SWITCHBOARD_TOKEN}` from the
environment the harness's `env_file` supplies:

```json
{
  "mcpServers": {
    "switchboard": {
      "type": "http",
      "url": "https://switchboard.example.com/mcp/your-endpoint-slug",
      "headers": { "Authorization": "Bearer ${SWITCHBOARD_TOKEN}" }
    }
  }
}
```

`.claude/settings.json`:

```json
{
  "enabledMcpjsonServers": ["switchboard"],
  "permissions": { "allow": ["mcp__switchboard"] }
}
```

Claude Code ignores a project's own MCP approvals until the folder is trusted.
Run `claude` in the workdir once, by hand and as the daemon's user, and accept
the trust prompt ([before the first start](./first-agent#before-the-first-start)).
The `permissions.allow` list pre-approves tools without `auto_accept`'s
anything-goes; use it when a one-shot needs only a few.

## Authentication

A supervised Claude Code uses your Keychain login on macOS, and a
`claude setup-token` subscription token (`CLAUDE_CODE_OAUTH_TOKEN` in the
`env_file`) on Linux and headless boxes. `ANTHROPIC_API_KEY` bills the API, not
your subscription, and a one-shot uses it without asking whenever it is set. See
[Claude Code authentication](./first-agent#claude-code-authentication).

## Waking on Switchboard doorbells

A [doorbell](https://switchboard.stump.wtf/docs/getting-started/concepts#the-queue-is-the-record-the-doorbell-is-only-a-hint)
arrives over Claude Code's Channels extension. There are two ways to act on it.

**A resident session with the channel loaded.** Register the endpoint with
`claude mcp add`, then load it as a channel and pre-allow its tools:

```toml
[harness.claude-sb]
harness = "claude-code"
args = ["--dangerously-load-development-channels", "server:switchboard", "--allowedTools", "mcp__switchboard"]
workdir = "~/agents/claude-sb"
restart = "no"
enabled = false
```

- Without the channels flag, Claude Code **drops every doorbell silently**, even
  though Switchboard records it as delivered.
- Without `--allowedTools mcp__switchboard`, the woken session stops at a
  permission prompt.
- **The flag asks for confirmation at every start.** A restarted session waits
  at that prompt until someone runs `harness attach claude-sb` and confirms, so a
  restart is not hands-off. An opt-in auto-confirm, which logs a warning every
  time it fires, is **coming**
  ([ADR-0029](/decisions/adr-0029-auto-confirm-dev-channels)).

[Push events: Claude Code](./push-events#claude-code-verified-with-one-obstacle)
has the measurements behind these rules and the `claude mcp add` command.

**A one-shot fired by a trigger.** `claude -p` cannot be woken: it exits when its
turn ends. Let the Harness daemon hold the channel instead, and start a one-shot
that drains the queue with `claim_next` each time the doorbell rings. This needs
no channels flag and no confirmation, so it survives restarts unattended:

```toml
[channel.switchboard]
url = "https://switchboard.example.com/mcp/my-agent-k3x9"
env_file = "~/.config/harness/triggers.env"
headers = { Authorization = "Bearer ${SB_TOKEN}" }

[harness.sb-drain]
harness = "claude-code"
prompt = "Call claim_next until it answers empty; work each todo, then complete or fail it."
auto_accept = true
workdir = "~/agents/sb-drain"
env_file = "~/.config/harness/env/sb-drain.env"
triggers = ["channel.switchboard"]
schedule = "@every 1h"                  # safety net: a doorbell can be missed
timeout = "30m"
```

The one-shot still needs the Switchboard MCP server in its workdir (the
`.mcp.json` above) to call `claim_next`. The `schedule` stays because push is
lossy: [ADR-0021](/decisions/adr-0021-on-demand-one-shots) recommends a drainer
that fires on every doorbell *and* on a clock.

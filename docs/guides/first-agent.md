---
title: "Your first supervised agent"
sidebar_position: 3
---

# Your first supervised agent

A **harness** is one supervised process. The daemon starts it in its own
pseudo-terminal, keeps its scrollback, restarts it according to a policy you
choose, and lets any client attach to it as if you had launched it in that
terminal yourself.

This guide puts an interactive agent CLI under supervision and walks through the
verbs you will use every day.

## A minimal `harness.toml`

Create `~/.config/harness/harness.toml`:

```toml
[harness.crush-main]
harness = "crush"
workdir = "~/src/my-project"
env_file = "~/.config/harness/env/crush-main.env"
description = "Crush in my-project"
restart = "on-failure"
restart_delay = 30
enabled = true

[harness.claude-main]
harness = "claude-code"
workdir = "~/src/my-project"
env_file = "~/.config/harness/env/claude-main.env"
description = "Claude Code in my-project"
restart = "on-failure"
restart_delay = 30
enabled = false
```

The daemon picks the file up on its own: it watches `harness.toml` and reloads
when you save. Run `harness reload` if you'd rather be explicit.

### The keys

| Key | What it does |
|-----|--------------|
| `harness` | **Required.** Which adapter runs this harness: `crush`, `claude-code`, `codex`, or `generic`. The adapter supplies the executable (`crush`, `claude`, `codex`, or `sh` for `generic`). |
| `args` | Arguments appended after the executable, such as `["--yolo"]` for Crush. For `generic` these are `sh`'s arguments, so an arbitrary command is `args = ["-c", "my-command --flag"]`. |
| `workdir` | The directory the agent starts in. Set it: agents are project-scoped, and `harness logs` uses it to find the agent's sessions. `~` expands. |
| `env_file` | A `KEY=VALUE` file layered onto this harness's environment at start. Put credentials here, not in `harness.toml`. A missing file is silently skipped. |
| `description` | Free text shown in `harness list` and the dashboard. |
| `enabled` | `true` means "keep this running": it starts now, and again every time the daemon starts. `false` means defined but idle until you `harness start` it. |
| `restart` | What to do when the process exits. See [Restart policy](#restart-policy-and-what-it-costs). |
| `restart_delay` | Seconds to wait before a restart. Default `0`. |

There is no `cmd` key. The `harness` kind decides what runs, and a config using
the old `cmd` key is rejected with a pointer to the new schema. Unknown keys are
a load error too, so a typo like `workir` fails loudly instead of being
ignored.

### Before the first start

Run each agent once by hand, in the same `workdir` and as the same user, and
get it fully logged in:

- **Claude Code:** `claude` opens a login flow and a "trust this folder"
  prompt. Harness can't click through either for you. Alternatively, put
  `ANTHROPIC_API_KEY` in the harness's `env_file`.
- **Crush:** configure a provider in `crush.json` or its environment, and put
  the API key in the `env_file`.

If you skip this, the harness starts green and sits at a login prompt nobody
sees. You can always `harness attach` and finish the prompt there.

## The verbs

```sh
harness list                    # every harness: state, schedule, next run, restarts, description
harness describe crush-main     # one harness in detail
harness attach crush-main       # drive it as a live terminal
harness logs crush-main         # what it did (see Observability)
harness stop crush-main         # stop it and mark it disabled
harness start claude-main       # start it and mark it enabled
harness restart crush-main      # stop + start; also clears a failed state
harness                         # the dashboard: all of the above, one keystroke each
```

`harness list` looks like this:

```
NAME         STATE         SCHEDULE            NEXT        RESTARTS   DESCRIPTION
crush-main   ● running     —                   —           0          Crush in my-project
claude-main  ○ stopped     —                   —           0          Claude Code in my-project
```

The `SCHEDULE` and `NEXT` columns are em dashes here because neither harness is
scheduled; [scheduled sweeps](./scheduled-sweeps) fill them in.

`start` and `stop` change **intent**, not just the process. A harness you
`stop` stays stopped across daemon restarts and reboots until you `start` it
again. The intent lives in the daemon's `state.json`, and `harness describe`
shows it as the `enabled` row.

### Attaching and detaching

`harness attach NAME` hands you the agent's terminal. Every key goes to the
agent except the `Ctrl-b` prefix:

| Keys | Action |
|------|--------|
| `Ctrl-b` then `d` | detach; the agent keeps running |
| `Ctrl-b` then `[`, or `PgUp` | scroll back through history |
| `Ctrl-b` then `h` / `l` | hop to the previous / next harness without detaching |
| `Ctrl-b` then `?` | help |

`harness attach NAME --ro` watches without sending keystrokes. Use it when you
want to look over an agent's shoulder without risking a stray key.

## Profiles

Profiles switch whole sets of harnesses together, such as "just the worker on
the laptop" versus "everything on the server":

```toml
[harness.reviewer]
harness = "crush"
workdir = "~/agents/reviewer"
enabled = false

[harness.docs-writer]
harness = "claude-code"
workdir = "~/agents/docs-writer"
enabled = false

[profile.light]
description = "just the reviewer"
harnesses = ["reviewer"]
autostart = true

[profile.full]
description = "every agent"
harnesses = ["reviewer", "docs-writer"]
autostart = true
```

```sh
harness profiles              # list them; the active one is marked *
harness use-profile full      # switch
```

Only one profile is active at a time. With `autostart = true`, its harnesses
come up whenever the daemon starts.

## Restart policy, and what it costs

`restart` mirrors Docker Compose:

| Value | Restarts when |
|-------|---------------|
| `"always"` | the process exits for any reason. **This is the default** for a harness with `args`. |
| `"unless-stopped"` | same as `always` here; a manual `stop` already persists |
| `"on-failure"` | the process exits non-zero |
| `"no"` | never |

The daemon also watches for **crash loops**. Three exits within ten seconds
mark a harness as `flapping` (shown as `◐ degraded`). From then on, restarts
back off from 1s, doubling toward a 30s cap. `harness describe` shows the
`flapping` marker, and `harness doctor` warns about any degraded harness.

For a metered agent — anything that bills per token or draws on a plan's quota
— **prefer `on-failure` with a real `restart_delay`**:

- **Every restart pays the agent's startup cost again.** That means its system
  prompt, tool definitions, MCP server handshakes, and whatever it reads to
  orient itself. That is thousands of tokens before it does anything useful.
- **`always` restarts clean exits too.** An agent that finishes its task, or
  that you `/quit`, is immediately relaunched, forever. Under `on-failure`, a
  clean exit stays down.
- **A short `restart_delay` turns a persistent failure into a token burner.**
  Expired credentials, a provider outage, a broken config: the agent starts,
  fails, and starts again. `restart_delay = 30` or more caps that at about two
  attempts a minute even before crash-loop backoff kicks in.

:::caution Watch for harnesses that never settle

The supervisor is designed to give up and park a harness in `failed` after
repeated consecutive failures. Current builds do not apply that limit, so a
harness that fails every time keeps retrying at its backoff interval
indefinitely. Check `harness doctor` for `degraded` harnesses, and `harness stop`
anything that is looping.

:::

## Changing a running harness

Edit `harness.toml` and save. The daemon reloads, but a **running** harness
keeps its old settings until it restarts. That way an edit never kills an agent
mid-task. `harness describe` shows `config — changed — restart to apply` until
you do:

```sh
harness restart crush-main
```

A harness you **add** with `enabled = true` starts on reload. A harness you
**remove** is stopped.

Next: [scheduled sweeps](./scheduled-sweeps).

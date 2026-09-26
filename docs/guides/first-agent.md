---
title: "Your first supervised agent"
sidebar_position: 3
---

# Your first supervised agent

:::tip Paste this to your agent

```text
Read https://stump-wtf.github.io/harness/llms.txt and
https://stump-wtf.github.io/harness/guides/first-agent. Put my agent CLI under
Harness supervision as that page describes, using the Claude Code
authentication mode that fits this machine. Then prove it: `harness list`
shows it running, and `harness attach NAME --ro` shows the agent itself, not a
login or trust prompt.
```

:::

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
| `harness` | **Required.** Which adapter runs this harness: `crush`, `claude-code`, `codex`, `generic`, or `command`. The adapter supplies the executable (`crush`, `claude`, `codex`, or `sh` for `generic`); a `command` harness names its own in `argv` instead (see [The command kind](/usage/configuration#the-command-kind)). |
| `args` | Arguments appended after the executable, such as `["--yolo"]` for Crush. For `generic` these are `sh`'s arguments, so an arbitrary command is `args = ["-c", "my-command --flag"]`. |
| `workdir` | The directory the agent starts in. Set it: agents are project-scoped, and `harness logs` uses it to find the agent's sessions. `~` expands. |
| `env_file` | A `KEY=VALUE` file layered onto this harness's environment at start. Credentials go here rather than in `harness.toml` — though on macOS a Keychain-backed agent login needs no `env_file` at all (see below). A missing file is silently skipped. |
| `description` | Free text shown in `harness list` and the dashboard. |
| `enabled` | `true` means "keep this running": it starts now, and again every time the daemon starts. `false` means defined but idle until you `harness start` it. Note the profile interaction: a harness the daemon does not yet know (new at boot, or new via `harness reload`) takes an autostart-profile membership as `enabled = true`; a harness the daemon already tracks keeps its persisted intent, and a stop is never undone by a reload (see Profiles). |
| `restart` | What to do when the process exits. See [Restart policy](#restart-policy-and-what-it-costs). |
| `restart_delay` | Seconds to wait before a restart. Default `0`. |

There is no `cmd` key. The `harness` kind decides what runs, and a config using
the old `cmd` key is rejected with a pointer to the new schema. Unknown keys are
a load error too, so a typo like `workir` fails loudly instead of being
ignored.

### Before the first start

Run each agent once by hand, in the same `workdir` and as the same user, and
get it fully logged in:

- **Claude Code:** `claude` opens a "trust this folder" prompt, and a login
  flow unless it already has a credential. Harness can't click through either
  for you. Which credential a supervised Claude Code should use depends on
  where the daemon runs; see
  [Claude Code authentication](#claude-code-authentication) below. For exactly
  what Harness runs for Claude Code, see [Claude Code under Harness](./claude-code).
- **Crush:** configure a provider in `crush.json` or its environment, and put
  the API key in the `env_file`.

If you skip this, the harness starts green and sits at a login prompt nobody
sees. You can always `harness attach` and finish the prompt there.

### Claude Code authentication

Claude Code can authenticate a supervised session three ways. Pick by where the
daemon runs:

| Mode | Use it when | What goes in the `env_file` | Bills |
|------|-------------|-----------------------------|-------|
| **Keychain** | macOS, with the daemon in your GUI login session | nothing | your Claude subscription |
| **Subscription token** | Linux, headless boxes, containers, SSH-only hosts, a system LaunchDaemon | `CLAUDE_CODE_OAUTH_TOKEN` from `claude setup-token` | your Claude subscription |
| **API key** | you have only a Claude Console key, or want per-token API billing | `ANTHROPIC_API_KEY` | **the API**, not your subscription |

**Keychain (the macOS default).** Log in once with `claude`, as the same user
the daemon runs as, and you are done: no `env_file`, no key. Claude Code keeps
the login in your login Keychain (the generic-password item
`Claude Code-credentials`), not in a file, and a harness inherits it because the
daemon spawns the agent as the same user. Copying a key into an `env_file` here
would move a credential *out* of the Keychain and onto disk, which is strictly
worse.

The one condition is that the daemon runs in your GUI login session. The
LaunchAgent in [Run the daemon as a service](/guides/run-as-a-service) does
(`launchctl bootstrap gui/$(id -u)`), so the normal setup is fine. A *system*
LaunchDaemon, a different user, or an SSH session with no unlocked login
keychain cannot reach it: use a subscription token there.

**Subscription token (the headless and Linux default).** On any machine with a
browser, run once:

```sh
claude setup-token
```

It opens the same browser sign-in as `/login` and prints a one-year OAuth token.
It needs a Pro, Max, Team or Enterprise plan, and it saves the token nowhere, so
copy it straight into the harness's `env_file` and lock the file down:

```sh
# ~/.config/harness/env/claude-main.env   (chmod 600)
CLAUDE_CODE_OAUTH_TOKEN=sk-ant-oat01-...
```

```sh
chmod 600 ~/.config/harness/env/claude-main.env
harness restart claude-main      # env files are read at start
```

The token can only make model requests, so a session using it cannot open
Claude Code's Remote Control or fetch claude.ai connectors; MCP servers you
configure locally still work. Renew it before the year is out.

**API key.** `ANTHROPIC_API_KEY=sk-ant-api...` in the `env_file` works anywhere,
but it **bills your API account per token, not your subscription**. Claude Code
ranks it *above* `CLAUDE_CODE_OAUTH_TOKEN` and your login, and a one-shot
(`claude -p`) uses it without asking whenever it is set. So an
`ANTHROPIC_API_KEY` left in the daemon's environment moves every scheduled run
onto API billing. See
[the harness bills my API account](./troubleshooting#the-harness-bills-my-api-account-or-starts-at-a-login-prompt).

Never put any of these in `harness.toml`, which you may keep in version control:
credentials belong in the `env_file`, mode `0600`
([ADR-0008](/decisions/adr-0008-security-and-secrets)). Check which credential a
session is using with `claude auth status`, run as the daemon's user, or
`/status` inside an attached session.

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

:::caution Profile membership only autostarts harnesses the daemon does not know yet
A member that is new at boot, or new to the daemon via `harness reload`, starts
even when `enabled = false` (ADR-0014): for it, the profile is its autostart
intent. A harness the daemon already tracks keeps its persisted intent instead
— a member left down with `enabled = false`, or one you `harness stop`ped, stays
down across reloads and restarts. The daemon lists such dormant members in
`harness list` and `doctor` so they are never silently lost.
:::

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

:::note A harness that never settles ends up `failed`

A failing harness does not retry forever. Every non-zero exit from a run that
lasted less than **5 minutes** counts as a consecutive failure; a clean exit, or
a run that lasts 5 minutes, resets the count. After **more than 5** consecutive
failures the daemon gives up and parks the harness in **`failed`**
(`✖ failed` in `harness list`), however long its `restart_delay` is, so a slow
failure loop stops too, not just a fast one.

A `failed` harness stays down until you fix the cause and run:

```sh
harness restart NAME
```

`harness doctor` reports it as an error with that same hint, and
`harness describe NAME` shows its last exit code and whether it was flapping.
The latch lives in the running daemon: a daemon restart starts an `enabled`
harness again, and a harness that is still broken works its way back to
`failed`. To keep it down across restarts, `harness stop NAME`.

:::

## Run it only during working hours

An always-on agent draws on your plan's usage around the clock, including the
hours nobody is looking at what it does. `operating_hours` gives a resident
harness weekly windows it may run in. Outside them the daemon holds it down, and
it starts again when the next window opens:

```toml
[harness.claude-main]
harness = "claude-code"
workdir = "~/src/my-project"
restart = "on-failure"
restart_delay = 30
enabled = true
operating_hours = "TZ=America/New_York Mon-Fri 09:00-18:00"
```

- Closing is **graceful** by default: the agent finishes its current turn first,
  for up to 15 minutes (`hours_shutdown_timeout`). Set
  `hours_shutdown = "immediate"` to stop it at the close instead.
- Outside hours, `harness list` shows it as `off-hours`, not `stopped`, and its
  NEXT column says when it opens.
- Hours never touch `enabled`. `harness stop` still stops it for good, and the
  next window does not undo that.
- Working late? `harness start claude-main` out of hours runs it under a
  one-hour lease; `harness start claude-main --for 3h` asks for longer.

Hours go in the global `harness.toml` only, and cannot be combined with
`schedule`: a scheduled sweep is already on a clock. The grammar, every rule and
the error messages are in
[Configuration → Operating hours](/usage/configuration#operating-hours).

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

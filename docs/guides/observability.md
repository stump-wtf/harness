---
title: "Observability"
sidebar_position: 6
---

# Observability

Unattended agents fail quietly. This page covers the four ways Harness lets you
see what happened:

- `harness logs` for a post-mortem of one run;
- the **chatroom** for every agent's activity in one live stream;
- `harness doctor` for the health of the whole setup;
- the **SSH cockpit** for all of it from another machine.

## `harness logs`: what did that run actually do?

```sh
harness logs NAME                     # the latest run, as agent activity
harness logs NAME --lines 50          # only the last 50 entries
harness logs NAME --follow            # keep printing as the agent works
harness logs NAME --raw               # the durable log: raw output + lifecycle lines
harness logs NAME --json              # structured, for scripts
harness logs NAME --include-ambiguous # also sessions another harness could have written
harness logs NAME --run 12            # one run of a scheduled harness
```

For scheduled harnesses, two more verbs find the run worth reading:

```sh
harness jobs                          # every scheduled harness: next run, last run, failure streak
harness runs NAME                     # its run history: trigger, outcome, duration, exit code
```

For an agent harness, `harness logs` reads the agent's **own session
transcript**, the one Claude Code, Crush or Codex writes for itself. It matches
the transcript to the harness's latest run and prints a timeline instead of a
screenful of terminal repaint.

Here is a sweep that failed:

```
run 2026-09-11 07:40:00 → 07:56:21 · exit 1 · crush
07:40:00  state     stopped → starting
07:40:00  state     starting → running
07:40:02  session   e088ec4e · crush · your-model
07:40:05  read      /home/you/sweeps/infra-check/prompt.md
07:41:10  exec      ssh web01 docker ps  (failed)
07:56:20  ERROR     Bad Request: context window exceeded
07:56:21  exited    code=1
07:56:21  state     running → stopped
```

Read it top to bottom:

1. **The header** gives the run window, the exit code and the adapter.
2. **`state` lines** are Harness's lifecycle, and **`exited`** is the process
   exit.
3. **`session`** is the agent session that ran: its id, the tool and the model.
4. **Tool entries** are what the agent did. `read` and `edit` show a file,
   `exec` a command, and `(failed)` marks a tool call that errored.
5. **`ERROR`** is the error the agent reported. Here it is the reason for exit
   code 1.

This run failed on its first real command, and then spent fifteen minutes
retrying and pulling output into its context until the model refused. The fix
is in the sweep's design, not its schedule: a narrower scope, plus a completion
contract that says "stop and report blocked" when a tool fails. See
[Designing a sweep you can trust](./scheduled-sweeps#designing-a-sweep-you-can-trust).

Lines prefixed with `?` are sessions that could also belong to another harness.
That happens, for example, when two harnesses share a `workdir`. They are only
shown with `--include-ambiguous`. Give every harness its own `workdir` and this
goes away.

When there is no transcript to match, `harness logs` explains why in `note`
lines and prints the durable log instead. That covers `generic` harnesses, a
harness with no `workdir`, and an agent that died before it opened a session.

### The durable log

`--raw` shows what the process printed, interleaved with lifecycle lines:

```
2026/09/11 20:46:00 INFO run started run_id=2 trigger=schedule
2026/09/11 20:46:00 INFO state changed from=stopped to=starting
2026/09/11 20:46:00 INFO state changed from=starting to=running
…agent output…
2026/09/11 20:46:03 INFO exited code=0
2026/09/11 20:46:03 INFO state changed from=running to=stopped
2026/09/11 20:46:03 INFO run finished run_id=2 outcome=success exit_code=0
```

It lives on disk too, so you can read it with ordinary tools:

| File | Holds |
|------|-------|
| `~/.local/state/harness/logs/NAME.log` | everything a harness printed, across runs (rotated) |
| `~/.local/state/harness/jobs/NAME/RUN_ID.log` | one scheduled run's output and lifecycle |
| `~/.local/state/harness/state.json` | intent, and the run history for scheduled harnesses |

:::caution Check raw output before you share it

The activity view masks credential-shaped strings in the commands it shows,
such as a token in a URL or an `Authorization:` header. **The durable log and
the chatroom are not redacted yet.** Anything an agent printed, including a
secret, is there verbatim. Read `--raw` output before pasting it into an issue or
a chat.

:::

## The chatroom: every agent at once

Open the dashboard with `harness` and press **`C`**. The chatroom shows every
agent harness's activity as one chronological stream, like a group chat. Each
harness speaks as its own user, and tool calls, results and prompts read as
messages. It is the fastest way to see what a pool of workers is doing right
now, or to notice the one that has gone quiet.

| Key | Action |
|-----|--------|
| `j` / `k`, `↓` / `↑` | scroll |
| `space` or `p` | pause or resume following new messages |
| `1` to `5` | show or hide a harness |
| `0` or `a` | show every harness |
| `esc` or `q` | back to the dashboard |
| `Ctrl-C` | quit `harness` |

Messages are attributed to the harness whose run produced them. A session that
Harness cannot tie to one specific harness is labelled by its tool instead, for
example `@crush`. Separate `workdir`s keep attribution precise here, just as
they do for `harness logs`.

## `harness doctor`: is everything healthy?

```sh
harness doctor
```

```
CHECK         STATUS      DETAIL
config        ok          /home/you/.config/harness/harness.toml — 4 harnesses
daemon        ok          listening at /run/user/1000/harness.sock
version       ok          client v0.4.0 · daemon v0.4.0
ssh           ok          off (not enabled in config)
harnesses     warn        1/4 degraded: sb-worker
                          → check `harness logs <name>` for the failure
summary       4 passed · 1 warning(s) · 0 failed
```

`doctor` checks, in order:

- **`config`:** the file parses, or it shows the exact file and line that
  doesn't.
- **`daemon`:** the socket is reachable.
- **`version`:** client and daemon are the same build. After an upgrade,
  restart the daemon so they match.
- **`ssh`:** the remote cockpit's state.
- **`harnesses`:** anything `degraded` or `failed`.

It also prints the active profile and autostart state when you use profiles,
and a `SETTING` table showing where each process setting came from: a flag, an
environment variable, the file, or a default.

Run it after every config change, and first whenever something seems off.
`harness doctor --json` gives the same data to scripts, which makes a cheap
health check for your own monitoring.

Two more views help:

```sh
harness describe NAME     # one harness: state, restarts, last exit, flapping, schedule, attached sessions
harness daemon status     # version, pid, uptime, socket, harness count
```

The daemon's own log goes wherever your service sends it:
`journalctl --user -u harness` under systemd, or the `StandardErrorPath` file
under launchd. For more detail, start the daemon with `--log-level debug` or
`HARNESS_LOG_LEVEL=debug`.

## The SSH cockpit

The same dashboard, chatroom and attach are available over SSH. That lets you
check on your agents from a laptop, a tablet or a phone. Enable it in
`harness.toml`:

```toml
[server]
enabled = true
listen = "127.0.0.1:23234"
authorized_keys_file = "~/.ssh/harness_authorized_keys"

# A key that may watch but never type
[[server.key]]
key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleKeyForAPhoneReadOnly phone"
read_only = true
```

Restart the daemon, then connect from a machine whose key is listed:

```sh
ssh -p 23234 your-host
```

- **Keys only.** There is no password authentication at all, and the SSH
  username is ignored. The key is the identity. Enabling `[server]` without any
  key source is a config error.
- **`read_only = true`** gives that key the dashboard, the chatroom and
  `attach --ro` behavior: live output and scrollback, with keystrokes ignored.
  Use it for any device you'd rather not type into an agent from.
- **A read-write key can type into every agent**, and agents often run with
  `--yolo`. Treat such a key like shell access to the box.
- **Choose `listen` deliberately.** `127.0.0.1` plus an SSH tunnel, or the
  address of a private network interface such as a VPN or tailnet, is safer
  than `0.0.0.0`.
- The server's host key is generated once and kept under
  `~/.local/state/harness/`, so clients see a stable fingerprint.

Next: [how Harness, Switchboard and Cairn fit together](./harness-switchboard-cairn).

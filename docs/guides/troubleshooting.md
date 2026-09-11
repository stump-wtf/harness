---
title: "Troubleshooting"
sidebar_position: 8
---

# Troubleshooting

Start every investigation with the same two commands:

```sh
harness doctor              # is the config loaded, the daemon reachable, anything degraded?
harness describe NAME       # state, restarts, last exit, flapping, schedule
```

Then find the symptom below.

## The agent is running but does nothing

`harness list` shows `● running`, and nothing happens. This is the most common
failure, because it looks healthy. Attach and look before changing anything:

```sh
harness attach NAME --ro
```

Usual causes, most likely first:

- **It is waiting at a prompt nobody can see.** First runs of Claude Code ask
  you to log in and to trust the folder, and an agent can stop to ask for tool
  approval. Attach read-write (`harness attach NAME`), answer the prompt, and
  detach with `Ctrl-b d`. For unattended work, run the agent once by hand in the
  same `workdir` first ([first agent](./first-agent#before-the-first-start)).
- **Its credentials expired or never arrived.** A logged-out agent keeps running
  and fails every request. Check as the same user the daemon runs as. For
  Claude Code:

  ```sh
  claude auth status
  ```

  If you pass keys through an `env_file`, check the path. **A missing `env_file`
  is silently skipped**, so a typo means the agent starts with no key at all.
  After fixing either, `harness restart NAME`, because env files are read at
  start.

- **It never received its events.** An always-on Switchboard worker idles until
  a doorbell arrives. If none arrive:
  - **Is the channel enabled?** Crush needs `--channels server:NAME` or
    `channel_enabled: true`, from a build that has the Streamable HTTP channel
    fix ([details](./push-events#crush-the-verified-path)). Listing the server
    under `mcp` alone does not push.
  - **Did the MCP server connect?** An empty `$SWITCHBOARD_TOKEN` drops the
    `Authorization` header, and the server rejects the connection. Attach and
    check the agent's MCP status.
  - **Is another session swallowing the doorbell?** Switchboard rings one
    session per todo. An interactive session, or a second copy of the worker on
    another machine, using the same endpoint can take the ring and do nothing.
    See [one channel consumer per server](./push-events#exactly-one-channel-consumer-per-server).
  - **Does it drain on startup?** Doorbells sent while the worker was down were
    dropped; their todos are still pending. Switchboard re-rings them, but only
    after about 5 minutes, then 20 minutes, 1 hour and 6 hours, and it stops
    after a fixed number of attempts. The worker's instructions should call
    `claim_next` until empty when it starts.
- **It's a headless Claude Code channel consumer.** That setup isn't verified
  to act on doorbells, and the development-channels flag waits for an
  interactive confirmation. See
  [the Claude Code status](./push-events#claude-code-what-is-and-isnt-verified).

## It keeps restarting

`harness list` shows `◐ degraded` or `◌ restarting`, and the restart count
keeps climbing.

```sh
harness logs NAME --raw --lines 40
harness describe NAME          # last_exit, flapping
```

- **`last_exit -1` with no output at all.** The process never started, and
  almost always the executable is not on the **daemon's** `PATH`. A service gets
  a minimal `PATH`, and a harness's `env_file` can't fix the executable lookup.
  Current builds don't log this spawn error, so check the path yourself:

  ```sh
  command -v claude crush codex          # where they live in your shell
  systemctl --user show harness -p Environment   # the PATH the daemon has (Linux)
  ```

  Add the missing directory to `Environment=PATH=…` in the unit, or to
  `EnvironmentVariables` in the plist, then restart the service. See
  [the service guide](./run-as-a-service).

- **A `generic` harness exits immediately.** `args` go to `sh`, so a bare
  `args = ["/usr/local/bin/thing"]` asks `sh` to read that binary as a script.
  Use `args = ["-c", "/usr/local/bin/thing --flag"]`.
- **A flag the CLI rejects.** The agent prints its usage and exits 1. The
  durable log shows the message.
- **A real crash loop.** After three exits within ten seconds the harness is
  `flapping`, and restarts back off.

What to do right away:

- **Stop anything looping**, especially a metered agent:

  ```sh
  harness stop NAME
  ```

  The supervisor is designed to give up and park a harness in `failed` after
  repeated failures, but current builds don't apply that limit. A broken harness
  keeps retrying until you stop it.

- **Once it's fixed, clear the state:** `harness restart NAME`. That also clears
  a `failed` state.
- **Prevent the next one:** use `restart = "on-failure"` with a
  `restart_delay` of 30 seconds or more for agents
  ([why](./first-agent#restart-policy-and-what-it-costs)).

## A schedule didn't fire

Check whether the daemon knows about the schedule at all:

```sh
harness jobs                   # every scheduled harness: schedule, next run, last run, failure streak
harness runs NAME              # what each window did: success, failed, skipped, missed…
harness describe NAME          # the cron spec and the absolute next run
```

- **It isn't in `harness jobs`.** The config didn't load, or the
  harness is in a drop-in the daemon hasn't read. `harness doctor` shows a parse
  error with its file and line. For drop-ins, run `harness reload` (see
  [the config didn't reload](#the-config-didnt-reload)).
- **The machine was asleep or the daemon was down at that time.** The window
  was **missed**. `harness runs NAME` shows a `missed` record, the daemon log has
  a `scheduled run MISSED` warning, and nothing ran because `catch_up` defaults
  to `false`. Set `catch_up = true` for work that should run once on wake.
- **It fired at the "wrong" time.** Without a prefix, a schedule uses the
  daemon's local time zone. `harness describe` shows the next run; compare it to
  what you expected, and pin the zone with `CRON_TZ=UTC`
  ([why](./scheduled-sweeps#schedule-cron-pinned-to-a-zone)).
- **The previous run was still going.** With `on_overlap = "skip"`, the
  default, a window that fires mid-run records `skipped` and does nothing. A run
  that never ends skips every later window. `harness runs NAME` shows the
  `skipped` records, and `harness jobs` shows the stuck run as `running #N`. Set
  a `timeout`.
- **It fired, and the run failed fast.** `harness runs NAME` shows `failed`
  with its exit code, and `harness logs NAME --run N` shows what it did. Work
  through [it keeps restarting](#it-keeps-restarting) for the cause.
- **It ran on a different machine.** A synced config fires on every host
  running a daemon. Keep scheduled harnesses in a per-host drop-in directory
  ([details](./scheduled-sweeps#a-schedule-runs-on-exactly-one-machine)).

To rule out the schedule entirely, run it now and watch:

```sh
harness trigger NAME --wait
echo $?        # the run's exit code; 124 = timed out, 75 = skipped (a run was in flight)
```

## The config didn't reload

- **The file has an error.** A config that fails to parse is not applied. The
  daemon keeps running the last good config. `harness doctor` shows the error
  with its file and line; fix it and save again.
- **You changed a drop-in.** Only `harness.toml` itself is watched. Files in
  `harness_d` need `harness reload`.
- **You changed a running harness.** Reload applies to what the daemon *will*
  run. A running harness keeps its old settings until it restarts, so an edit
  never kills an agent mid-task. `harness describe NAME` shows `config — changed
  — restart to apply`. Run `harness restart NAME`.
- **You changed an `env_file` or a `prompt_file`.** Env files are read when a
  harness starts, so run `harness restart NAME`. Prompt files are read at every
  run, so the next run picks the change up with no action.
- **Watching is off.** `[daemon] watch_config = false`, or
  `HARNESS_WATCH_CONFIG=false` in the service environment, disables
  auto-reload. Use `harness reload`, or `kill -HUP` the daemon.
- **The daemon reads a different file than the one you edited.** The `config`
  row of `harness doctor` shows the path the daemon loaded. A `HARNESS_CONFIG`
  or `--config` in the service definition overrides the default.

## `harness` says the daemon isn't running, but it is

The client and the daemon disagree about the socket path. That usually means
your shell and the service see different `XDG_*` variables, most often on
macOS. Pin `HARNESS_SOCKET` to the same short path in both
([details](./run-as-a-service#the-client-has-to-find-the-socket)).

## The daemon exits at startup with `bind: invalid argument`

The socket path is too long: about 104 bytes on macOS, 108 on Linux. It happens
when `XDG_RUNTIME_DIR` or `XDG_STATE_HOME` points somewhere deeply nested. Set
`HARNESS_SOCKET` (or `--socket`) to a short path, such as
`~/.local/state/harness/harness.sock`.

## Still stuck?

Collect `harness doctor`, `harness describe NAME`, `harness logs NAME`, and the
daemon's own log from around the time of the problem. Read `--raw` logs for
secrets before sharing them; see
[the durable log](./observability#the-durable-log).

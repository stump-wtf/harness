---
title: "Notifications"
sidebar_position: 4.5
---

# Notifications

A harness that gives up, gets stopped by the loop guard, or has its session
rotated needs a person — and until you configure a notify hook, the only record
of it is a log line. On one box a remote-control harness failed with `Error: You
must be logged in to use Remote Control.` six times, gave up into `failed`, and
sat dead for thirteen hours before anyone looked.

The `[notify]` table names one program the daemon runs when that happens. It is
yours: it can send a Signal message, post to ntfy, page, mail — Harness does not
care, it only hands the program the event.

```toml
[notify]
command  = ["/home/me/.config/harness/notify.sh"]  # argv; exec'd without a shell
events   = ["failed", "flapping", "loop_stopped", "session_rotated", "recovered"]  # the default
timeout  = "15s"   # the hook's whole process group is killed after this
cooldown = "15m"   # one alert per harness per event per window; "0s" turns it off
```

Only `command` is required. `command[0]` must be an absolute path: the daemon
runs under init with whatever `PATH` that gives it, and there is no shell to
look anything up. The table is global — a project file or a `harness_d`
drop-in that declares it is refused — and it follows `harness reload`, so
adding or changing it needs no daemon restart.

The hook is a daemon-side program, not a harness: it gets no PTY, no restart
policy, no run record and no trace, and it runs only when the daemon has
something to say.

## Events

| Event | When |
|---|---|
| `failed` | A harness gave up into `failed` after exhausting its restart budget. It stays down until `harness restart`. |
| `flapping` | Crash-loop backoff escalated: the harness is exiting quickly over and over. Usually arrives before `failed`. |
| `loop_stopped` | The [runaway tool-loop guard](./supervision#what-the-daemon-guarantees) stopped the harness. Its enabled intent is cleared, so it stays down until `harness start`. |
| `session_rotated` | The session guard found a `crush` session wedged on context-limit errors, archived its store and restarted it on a fresh session — or could not finish, and left it stopped. |
| `recovered` | A harness you were told was `failed` or `loop_stopped` is running again, so the alert thread can close. |
| `run_failed` | A scheduled or triggered run ended `failed` or `timed_out`. **Not in the default set** — some jobs fail as their normal "nothing to do" answer. Add it to `events` to opt in. |

`test` is what `harness doctor --notify-test` sends; it is never listed in
`events` and always runs.

The cooldown is per harness and per event, so a harness that flaps all night
alerts once per window rather than once per restart. A `recovered` clears that
harness's `failed` and `loop_stopped` cooldowns, so a harness that fails again
straight after you fix it alerts again.

## What the hook receives

Each delivery sets these environment variables (on top of the daemon's own
environment, which the hook inherits):

| Variable | Example |
|---|---|
| `HARNESS_NOTIFY_EVENT` | `failed` |
| `HARNESS_NOTIFY_HARNESS` | `claude-rc` |
| `HARNESS_NOTIFY_HOST` | `kitt` |
| `HARNESS_NOTIFY_STATE` | `failed` |
| `HARNESS_NOTIFY_MESSAGE` | one line, ready to send as-is (below) |

and writes the same facts as JSON on stdin:

```json
{
  "version": 1,
  "event": "failed",
  "harness": "claude-rc",
  "host": "kitt",
  "state": "failed",
  "message": "claude-rc failed: gave up after 6 consecutive failures (last exit 1): \"Error: You must be logged in to use Remote Control.\" — restart with `harness restart claude-rc`; see `harness logs claude-rc`",
  "cause": "Error: You must be logged in to use Remote Control.",
  "hint": "harness logs claude-rc",
  "time": "2026-09-25T20:15:16Z",
  "exit_code": 1,
  "restarts": 6
}
```

`cause` is the most useful single fact the daemon has: for `failed`,
`flapping` and `run_failed`, the last line the agent printed before it exited
(the daemon's own log lines are skipped); for `loop_stopped`, the tool and the
count; for `session_rotated`, how many recent turns failed. `tool` and `count`
(loop stops) and `run_id` (failed runs) appear when they apply. New fields may
be added under the same `version`; a rename or removal bumps it.

Every string passes through the same credential redaction `harness logs` uses
before it leaves the daemon, because `cause` quotes agent output and agent
output is where tokens turn up. Only `argv[0]` of the hook is ever reported
back (by `harness doctor`); the rest of your argv is yours.

### A minimal hook

```sh
#!/bin/sh
# ~/.config/harness/notify.sh — post each event to an ntfy topic.
exec curl -fsS --max-time 10 \
  -H "Title: harness on $HARNESS_NOTIFY_HOST: $HARNESS_NOTIFY_EVENT" \
  -d "$HARNESS_NOTIFY_MESSAGE" \
  https://ntfy.sh/my-private-topic
```

Anything the hook prints goes to the daemon log only if it fails. A non-zero
exit, a missing file, or a timeout is logged as `notify: hook failed` with the
tail of the hook's output and counted in the metric below; the daemon never
retries.

## Checking it works

```sh
harness doctor                 # notify row: configured? executable? last delivery?
harness doctor --notify-test   # the daemon runs the hook once with a `test` event
```

`--notify-test` runs the hook from the daemon, with the daemon's environment,
so it proves the whole path rather than just the script. It exits non-zero if
the hook did.

## Delivery

Delivery is asynchronous and never holds up supervision: notifications queue
for a small pool of workers, and if the queue is full the notification is
dropped and counted rather than waited for. With the metrics listener on,
outcomes are exported as:

| Series | Type | Notes |
|---|---|---|
| `harness_notify_deliveries_total{event,result}` | counter | `ok`, `error` (could not start, or exited non-zero), `timeout`, `dropped` (queue full), `suppressed` (inside the cooldown). |

An alert on a hook that has stopped working:

```yaml
- alert: HarnessNotifyHookFailing
  expr: increase(harness_notify_deliveries_total{result=~"error|timeout|dropped"}[1h]) > 0
```

<!-- canary 2026-09-26: docs-only PR to exercise the forward review loop -->

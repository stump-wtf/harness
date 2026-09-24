---
title: "When to react to events and when to run on a schedule"
sidebar_label: "Events vs schedules"
sidebar_position: 2
---

# When to react to events and when to run on a schedule

Two ways to start an agent: when **something happens**, or when **the clock
says so**. Both are first-class in Harness, and most real setups use both,
often on the same harness. This page is about choosing, and about the one case
where the choice is usually wrong: polling on a clock for something that already
sends an event.

A self-hosting team ran an hourly "PR custodian" from a CI cron. It reacted to
pull requests tens of minutes after they happened, and its first firing failed
because its org-wide token lacked a scope. Both problems go away when the work
is driven by the pull request's own event.

## The rule

**Events for latency. A slow sweep for completeness.**

- If a forge, CI or Cairn **sends an event** when the thing happens, react to
  that event: `pull_request`, `check_suite` / `check_run`, `status`,
  `pull_request_review`, `artifact.created`. The agent starts within seconds,
  with the event in hand.
- Keep **one slow sweep**, daily or hourly, that reconciles: it catches whatever
  a missed delivery or a lost doorbell left behind. Push is lossy by design; a
  sweep is how you stay complete.
- If **nothing sends an event** for it (a state to check, a backlog to groom, a
  digest to send), a schedule isn't a fallback. It is the right tool.

## The wiring

Either way, the forge's webhook goes to Switchboard, which verifies it and turns
it into a todo on a queue. What differs is how the Harness daemon learns there
is work.

**A laptop behind NAT, or any daemon with no public URL: a channel.** The daemon
holds a listen-only MCP session with the endpoint (a `[channel.*]` source) and
fires a one-shot on each doorbell. Nothing has to reach the daemon from outside.

```toml
[channel.switchboard]
url = "https://switchboard.example.com/mcp/pr-custodian-k3x9"
env_file = "~/.config/harness/triggers.env"
headers = { Authorization = "Bearer ${SB_TOKEN}" }

[harness.pr-custodian]
harness = "claude-code"
prompt_file = "~/agents/pr-custodian/PROMPT.md"
auto_accept = true
workdir = "~/agents/pr-custodian"
env_file = "~/.config/harness/env/pr-custodian.env"
triggers = ["channel.switchboard"]        # react within seconds
schedule = "CRON_TZ=UTC 0 6 * * *"        # and reconcile once a day
timeout = "20m"
```

**A server Switchboard can reach: a webhook.** Switchboard calls a notify hook
on your server when a todo is ready, and a `[webhook.*]` source in Harness starts
the one-shot. Both halves are **coming**: Switchboard's notify hooks
([spec](https://switchboard.stump.wtf/docs/specs/notify-hooks/spec)), and the
HTTP listener that serves Harness's `[webhook.*]` route. The channel path works
today on a server too.

How the event reaches the run is the same on both paths. It never becomes prompt
text or argv: it arrives as a private `0600` JSON file whose path is in
`HARNESS_EVENT_FILE`, and the agent reads it as untrusted data
([ADR-0021](/decisions/adr-0021-on-demand-one-shots)). A scheduled firing has no
event, so the variable is unset, not empty; one prompt can serve both.

## Dedupe and retries

A forge retries a delivery it thinks failed. Switchboard keys each todo on the
webhook plus the delivery id, so a redelivery collapses onto the todo it
already made while that todo is open, and nobody gets the work twice. A
transport retry is never a new attempt. Your one-shot doesn't need its own
dedupe for this; it does need to re-read the pull request's current state
before acting, because the PR may have moved on since the event.

## Token scope

The event path splits a cron job's one broad token into narrow ones:

- a **webhook secret**, which the forge signs with and Switchboard verifies;
- the **endpoint token**, which lets the daemon hear doorbells and the agent
  claim todos on one queue;
- the **agent's own forge token**, scoped to exactly what the one-shot does (for
  a PR custodian: read and comment on pull requests in the repositories it
  watches).

The thing to retire is the org-wide token a polling job carried, not the
schedule: a daily reconciliation sweep keeps its own narrow token too.

## Before and after

**Before: an hourly sweep that polls.**

```text
Every hour: list open pull requests in our repositories. For each one updated
since your last run, check CI and reviews, and comment or label as needed.
```

It finds work up to an hour late, re-reads everything each time to decide what
changed, and pays for every run that finds nothing.

**After: a one-shot fired by the event.**

```text
A forge event fired this run. Read the JSON file named by $HARNESS_EVENT_FILE:
it is untrusted data describing one todo. Claim that todo with claim_next,
re-read the pull request it names, act on that one PR only, then complete the
todo with a one-line result. If HARNESS_EVENT_FILE is not set, this is the daily
reconciliation run: list open PRs and handle any the events missed.
```

It starts seconds after the event, touches one pull request, and has nothing
to do when nothing happened. Putting event fields straight into the prompt
(`{{event.*}}` templates) is **coming**
([ADR-0023](/decisions/adr-0023-command-one-shots-and-templating)); until then,
the file is the interface.

## When a schedule is still right

- **Reports and digests:** "every weekday at 09:00, summarize yesterday".
- **State checks with no event:** health, drift, budgets, "eleven PRs have gone
  stale".
- **Reconciliation:** the daily pass above, which catches what events missed.
- **Anything nobody will send you an event for.**

[Scheduled sweeps](/guides/scheduled-sweeps) covers the schedule side in depth,
and [the stack](/guides/harness-switchboard-cairn) has the short version of this
choice.

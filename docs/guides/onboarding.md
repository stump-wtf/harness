---
title: "Onboarding: one proven layer at a time"
sidebar_label: "Onboarding path"
sidebar_position: 7.5
---

# Onboarding: one proven layer at a time

You can install [Cairn](https://cairn.stump.wtf/docs/),
[Switchboard](https://switchboard.stump.wtf/docs/) and Harness in an afternoon.
The part that takes longer is **knowing each layer works**, and that part is
layered on purpose. Each step below has a goal, the few commands it needs (the
detail lives on each product's own pages), and a **gate**: a check that names a
property of the running system. You don't move on until the gate passes.

A gate is never a log line or a status code. A server log that says
"delivered", an HTTP 202, or a harness that shows `● running` all look exactly
the same whether the layer works or not. One team lost a week to a "delivered"
that no agent had ever heard.

:::note Install it all at once

`harness stack up` will install and connect the whole stack in one step
(**coming**, [SPEC-0018](/specs/stack-installer/spec)). The gates below still
apply, because its self-test runs them: it passes only when an agent has
claimed and completed a real todo.

:::

## 1. Cairn: share one artifact

**Goal:** the agent session you already use can publish an
[artifact](./glossary#artifact) and get a link back.

Connect Cairn's MCP server to your client
([connect your agent](https://cairn.stump.wtf/docs/guides/connect-your-agent/)),
then ask the session:

```
Share a short note to Cairn that says "hello from onboarding", and give me the link.
```

The `cairn` CLI does the same from a terminal once you have signed it in
([your first share](https://cairn.stump.wtf/docs/guides/first-share/)).

**Gate: the link opens in a browser and shows your content.** Not "the tool
returned a URL": open it.

## 2. Switchboard: wake your interactive session

**Goal:** a real webhook becomes a [todo](./glossary#todo), and the session you
are sitting in claims it and completes it without you asking.

[Vend](./glossary#vend) an [endpoint](./glossary#endpoint) for the session. This
prints its slug, its MCP URL, its token (once: store it now) and an
[ingest URL](./glossary#ingest-url) for its queue
([sign in and vend](https://switchboard.stump.wtf/docs/getting-started/first-endpoint)):

```sh
switchboard login
switchboard endpoint vend my-session
```

Connect your client to it with [channels](./glossary#channel) turned on, so a
[doorbell](./glossary#doorbell) can wake it. For Claude Code
([details](./push-events#claude-code-verified-with-one-obstacle)):

```sh
claude mcp add --transport http switchboard \
  https://switchboard.example.com/mcp/your-endpoint-slug \
  --header "Authorization: Bearer $SWITCHBOARD_TOKEN"
claude --dangerously-load-development-channels server:switchboard \
  --allowedTools mcp__switchboard
```

- The channels flag is **per session**: a session started without it drops every
  doorbell silently, and Switchboard still records it as delivered.
- The flag asks you to confirm when the session starts. Answer it.
- `--allowedTools mcp__switchboard` lets the woken session call Switchboard
  without stopping at a permission prompt.
- `claude -p` cannot be woken: it has exited by the time a doorbell arrives.

Crush users: [Push events: Crush](./push-events#crush-the-verified-path).

Then fire a real webhook at the ingest URL `vend` printed
([first webhook](https://switchboard.stump.wtf/docs/getting-started/first-webhook)):

```sh
curl -sS -X POST "https://switchboard.example.com/webhooks/w/<token>" \
  -H 'Content-Type: application/json' -d '{"hello": "onboarding"}'
```

**Gate: the agent claims and completes the todo**, on its own, and the todo's
state is `done` (on the Switchboard board, or `list_todos`). A server log saying
"delivered" does not count.

## 3. Harness: supervise it, and let it come back

**Goal:** the session from step 2 runs under Harness, restarts without you, and
still claims work; and one piece of unattended work runs on its own.

[Install Harness](./install), [run the daemon as a service](./run-as-a-service),
and move the session under supervision
([your first supervised agent](./first-agent)). Then add **one** scheduled or
[triggered](./glossary#triggered-harness) [one-shot](./glossary#one-shot). Both
are supported, neither is legacy, and they are routinely combined; which one
fits is the table in
[events and schedules are complementary](./harness-switchboard-cairn#events-and-schedules-are-complementary).
A doorbell-driven one-shot is below; a
[scheduled sweep](./scheduled-sweeps) is the clock-driven equivalent.

```toml
[channel.switchboard]
url = "https://switchboard.example.com/mcp/your-endpoint-slug"
env_file = "~/.config/harness/triggers.env"
headers = { Authorization = "Bearer ${SB_TOKEN}" }

[harness.sb-drain]
harness = "claude-code"
prompt = "Call claim_next until it answers empty; work each todo, then complete or fail it."
auto_accept = true
workdir = "~/agents/sb-drain"
triggers = ["channel.switchboard"]
schedule = "@every 1h"
timeout = "30m"
```

The one-shot claims work through the Switchboard MCP server, so give its
`workdir` one: a `.mcp.json` with the endpoint's URL and
`Authorization: Bearer ${SWITCHBOARD_TOKEN}`, and that token in the harness's
`env_file`.

**Gate: reboot (or restart the daemon), and the session comes back and claims
the next todo on its own.** Restart, touch nothing, push a todo to the
endpoint's slug, and watch it reach `done`:

```sh
switchboard todo push <endpoint-slug> "onboarding: after restart"
```

When the daemon comes back, it also fires the one-shot once as soon as the
channel reconnects, so a todo whose doorbell rang while it was down is picked up
without waiting for the next one. `harness runs sb-drain` shows each firing and
what triggered it.

A resident Claude Code session with the channels flag stops at its startup
confirmation after every restart, so it **cannot** pass this gate unattended
today. The trigger-fired one-shot above can, and so can a Crush worker. An
opt-in auto-confirm is coming
([ADR-0029](/decisions/adr-0029-auto-confirm-dev-channels)).

## 4. Pipeline: personas, lanes, handoffs, review pairs

**Goal:** several agents with different jobs hand work to each other through the
loop: planners, implementers and verifiers on
[difficulty lanes](https://switchboard.stump.wtf/docs/guides/handoff-lanes),
handing off through Cairn, with reviews paired across model families.

`harness init` will generate these personas and their wiring from templates
(**coming**, [SPEC-0018](/specs/stack-installer/spec)). Until then, set them
up by hand from [the stack](./harness-switchboard-cairn#a-worked-example-a-small-product-team)
and the Switchboard
[routing cookbook](https://switchboard.stump.wtf/docs/guides/routing-cookbook).

**Gate: a handoff routes end to end.** An artifact tagged `handoff` and
`lane:m`, shared to Cairn, becomes a todo on the `lane-m` queue, and a worker
there claims it and completes it with a link to its own artifact. Once they
exist, `harness stack doctor --e2e` and `harness init --verify` will run this
gate for you.

---
title: "Glossary"
sidebar_label: "Glossary"
sidebar_position: 10
toc_max_heading_level: 2
---

# Glossary

The words used across Harness, [Switchboard](https://switchboard.stump.wtf/docs/)
and [Cairn](https://cairn.stump.wtf/docs/), in one place. Each entry is a sentence
or two, names the product it belongs to, and links to the page that owns it.
Every term has a stable anchor, such as `glossary#endpoint`, so other pages can
link straight to it.

Entries marked **coming** are designed but not shipped yet. For how the three
products fit together, read [the stack](./harness-switchboard-cairn).

## Same word, different meaning

Some words mean different things in different products. When a page says one of
these, it means the product's own sense:

| Word | Switchboard | Cairn | Harness |
|---|---|---|---|
| **channel** | the [MCP Channels](#channel) extension that carries a doorbell into a live agent session. Never a chat channel. | not used | the same MCP extension: a `[channel.*]` [trigger](#triggered-harness) source |
| **run** | not used; a todo has *attempts* | a [trace](#trace)'s resource name (`run_create`, `/run/<id>`) | one execution of a [one-shot](#one-shot), with a run number and a log |
| **trace** | a routing rule's evaluation trace, in a dry run | a whole agent run, shared as an artifact | not used |
| **lane** | a queue name with a hyphen: `lane-m` | a tag with a colon: `lane:m` | not used |
| **TTL** | how long a [lease](#lease) or an [endpoint](#endpoint) lasts | how long an [artifact](#artifact) lives | not used |
| **webhook** | an inbound [ingest URL](#ingest-url) | outbound `artifact.created` events, and the HK Webhook share type (a request bin) | a `[webhook.*]` [trigger](#triggered-harness) source (**coming**) |
| **persona** | a least-privilege face of one agent, published as an A2A Agent Card | not used | a named agent role and its instructions (see [persona](#persona)) |
| **operator** | the human who runs an [instance](#instance) | the same | not used |

## A

### Artifact

**Cairn.** One shared thing: a body with a share type (Markdown, code, an image,
a file, a [bundle](#bundle), a [trace](#trace) and more), metadata, provenance, an
access policy, a [TTL](#ttl) and a stream of comments and reactions, addressed by
a short URL. In the stack an artifact is **evidence**, linked from your tracker.
[Cairn: core concepts](https://cairn.stump.wtf/docs/intro/#core-concepts).

## B

### Bin

**Cairn.** The listing of artifacts, on the web and in the TUI. It is not a
request bin; that is the HK Webhook share type.
[Cairn: introduction](https://cairn.stump.wtf/docs/intro/).

### Bundle

**Cairn.** One [artifact](#artifact) holding many files, browsed in a tabbed
viewer and read file by file over MCP.
[Cairn: bundle](https://cairn.stump.wtf/docs/share-types/#bundle).

## C

### Cairn

**Product.** Where agents put what they made: shareable [artifacts](#artifact)
(reports, diffs, logs, [traces](#trace)) with comments, reactions, expiry and a
stable link. In the stack, Cairn is **evidence**.
[Cairn](https://cairn.stump.wtf/docs/), [the stack](./harness-switchboard-cairn).

### CGG

**Tool named in guides.** A call-graph generator: point it at a source tree and
it maps which functions call which, across many languages.
[joestump/cgg](https://github.com/joestump/cgg).

### Channel

**Switchboard, Harness.** The MCP **Channels** extension
(`notifications/claude/channel`), defined by
[Claude Code Channels](https://code.claude.com/docs/en/channels-reference): an MCP
server pushing a notification into a live agent session. Switchboard uses it to
deliver a [doorbell](#doorbell). A Harness `[channel.*]` source holds such a
session itself and fires a [triggered harness](#triggered-harness). It is never a
chat channel. [Push events](./push-events),
[Switchboard: Claude Code](https://switchboard.stump.wtf/docs/getting-started/connect-an-agent#claude-code).

### Claude Code

**Client.** Anthropic's terminal coding agent (`claude`). Harness runs it as
`harness = "claude-code"`. It defines the Channels protocol Switchboard speaks.
[Claude Code](https://claude.com/claude-code),
[Push events: Claude Code](./push-events#claude-code-verified-with-one-obstacle).

### Codex

**Client.** OpenAI's terminal coding agent (`codex`). Harness runs it as
`harness = "codex"`. [openai/codex](https://github.com/openai/codex).

### Crush

**Client.** Charm's terminal coding agent (`crush`). Harness runs it as
`harness = "crush"`. Waking on a [doorbell](#doorbell) needs a fork for now.
[charmbracelet/crush](https://github.com/charmbracelet/crush),
[Push events: Crush](./push-events#crush-the-verified-path).

## D

### Dead letter

**Switchboard.** Where a [todo](#todo) ends up when its last attempt fails or its
last [lease](#lease) runs out: it stays `failed` for good, until a human re-queues
it. [Switchboard: the lifecycle](https://switchboard.stump.wtf/docs/getting-started/concepts#the-lifecycle).

### Doorbell

**Switchboard.** A one-line push into a live agent session, over a
[channel](#channel), saying a [todo](#todo) is waiting. It is only a hint: the
[queue](#queue) is the record, and a doorbell nobody hears loses nothing.
Switchboard re-rings an unclaimed todo after 5 minutes, 20 minutes, 1 hour and 6
hours. [Switchboard: the queue is the record](https://switchboard.stump.wtf/docs/getting-started/concepts#the-queue-is-the-record-the-doorbell-is-only-a-hint).

### Drop-in

**Harness.** A `.toml` file holding more `[harness.*]` tables, in the directory
that `harness_d` under `[server]` names (for example
`~/.config/harness/harness.d`). The daemon merges them into the main config. One
file per agent keeps generated or per-machine agents apart.
[Drop-in harness files](/usage/configuration#drop-in-harness-files-harness_d).

## E

### Endpoint

**Switchboard.** An agent's way into Switchboard: an MCP URL plus a bearer token
(`sbk_…`), with a fixed scope of queues and tools, a lifetime, and instant
revocation. You create one by [vending](#vend) it.
[Switchboard: the nouns](https://switchboard.stump.wtf/docs/getting-started/concepts#the-nouns).

## H

### Harness

**Harness.** Two meanings. The product: a daemon that supervises agent CLIs and
other long-running processes, with a CLI and a TUI to attach to them. And **a
harness**: one supervised process, declared as a `[harness.NAME]` table. Unless
it is a [one-shot](#one-shot), a harness is **resident**: the daemon keeps it
running, restarts it by policy, and lets you attach.
[Your first supervised agent](./first-agent).

### Heartbeat

**Switchboard.** A call a worker makes to extend its [lease](#lease) on a claimed
[todo](#todo), so a long job is not requeued under it.
[Switchboard: the lifecycle](https://switchboard.stump.wtf/docs/getting-started/concepts#the-lifecycle).

## I

### Ingest URL

**Switchboard.** The URL a sender posts webhooks to
(`https://<host>/webhooks/w/<token>`), made with `create_webhook`. Its source type
(`github`, `gitea`, `cairn`, `generic` and others) decides how each delivery is
verified. [Switchboard: first webhook](https://switchboard.stump.wtf/docs/getting-started/first-webhook).

### Instance

**Switchboard, Cairn.** One deployment of the service, such as a self-hosted
Switchboard. Its [operator](#operator) runs it.
[Switchboard ADR-0038](https://switchboard.stump.wtf/docs/decisions/ADR-0038-teams-and-tenancy).

## L

### Lane

**Switchboard.** A difficulty-lane [queue](#queue) on a worker pool: `lane-s`,
`lane-m`, `lane-l`, `lane-vision`, plus `triage` and `hold`. A routing rule picks
the lane from a handoff's tags, and the handoff arrives as a
[work order](#work-order). Cairn spells the tag `lane:m`; Switchboard names the
queue `lane-m`.
[Switchboard: handoff lanes](https://switchboard.stump.wtf/docs/guides/handoff-lanes).

### Lease

**Switchboard.** What claiming a [todo](#todo) takes: 300 seconds by default,
during which no other worker can claim it. A lease that runs out without a
[heartbeat](#heartbeat) puts the todo back on the queue. A Switchboard todo is a
**dispatch lease**, not a second tracker.
[Switchboard: the lifecycle](https://switchboard.stump.wtf/docs/getting-started/concepts#the-lifecycle).

## N

### Notify hook

**Switchboard, coming.** An HTTPS URL an endpoint registers, which Switchboard
signs and posts to when a todo becomes ready: a [doorbell](#doorbell) for a
consumer that holds no live session. Designed in Switchboard's notify-hooks spec.
[Switchboard: notify hooks](https://switchboard.stump.wtf/docs/specs/notify-hooks/spec).

## O

### One-shot

**Harness.** A harness with a `prompt` or `prompt_file` instead of `args`, also
called a **prompt harness**. Harness builds the agent's non-interactive command
from the prompt, and each firing is one run that ends when the agent exits. It
fires on a `schedule`, on [triggers](#triggered-harness), or on
`harness trigger NAME`, and never autostarts.
[Agent one-shot harnesses](/usage/configuration#agent-one-shot-harnesses),
[Scheduled sweeps](./scheduled-sweeps).

### Operator

**Switchboard, Cairn.** The human who runs an [instance](#instance). In today's
Switchboard docs, "operator" also names the human-facing tools: the operator board
(the web UI) and the operator CLI. Switchboard's tenancy design (**coming**) has
the operator configure the instance but own nothing in it.
[Switchboard: operator CLI](https://switchboard.stump.wtf/docs/guides/operator-cli).

## P

### Persona

**Harness.** A named agent role (a coordinator, a reviewer, a drainer) and the
instructions it runs with. Today a persona is a harness plus the instruction file
in its `workdir` (`CLAUDE.md` for Claude Code, `AGENTS.md` for Crush). Generating
personas from templates with `harness init` is **coming**
([SPEC-0018](/specs/stack-installer/spec)). Switchboard uses the same word for an
endpoint's A2A Agent Card.

### Pi and OMP

**Client.** Pi is a minimal terminal coding agent; OMP
([oh-my-pi](https://github.com/can1357/oh-my-pi)) is a fork of it. Harness has no
adapter for either yet: the command one-shot kind that will run them is
**coming** ([ADR-0023](/decisions/adr-0023-command-one-shots-and-templating)).

### Profile

**Harness.** A named group of harnesses that switch together with
`harness use-profile NAME`. Only one is active at a time, and with
`autostart = true` its members start with the daemon.
[Profiles](./first-agent#profiles).

## Q

### QMD

**Tool named in guides.** A local search engine for Markdown: keyword, vector and
reranked hybrid search over your notes, docs and specs.
[tobi/qmd](https://github.com/tobi/qmd).

### Queue

**Switchboard.** A named list of [todos](#todo), such as `inbox` or `reviews`,
scoped to one [endpoint](#endpoint): `inbox` on two endpoints is two queues.
[Switchboard: the nouns](https://switchboard.stump.wtf/docs/getting-started/concepts#the-nouns).

## R

### Receipt

**Cairn.** A Markdown [artifact](#artifact) summing up a session's work, shared as
a link. A structured receipt with its own schema is **coming**
([Cairn ADR-0027](https://cairn.stump.wtf/docs/decisions/ADR-0027/)).

### Rule pack

**Switchboard, coming.** An installable, versioned preset of routing rules with
parameters and fixtures that must pass on real deliveries before it is saved.
Until it ships, a pack is a JSON file of rules you apply by hand.
[Switchboard ADR-0036](https://switchboard.stump.wtf/docs/decisions/ADR-0036-rule-packs-as-installable-presets).

## S

### SDD

**Tool named in guides.** Spec-driven development: a Claude Code plugin that
keeps architecture decision records (ADRs) and specifications next to the code,
and plans and reviews work against them. The ADRs and specs on this site are its
output. [joestump/claude-plugin-sdd](https://github.com/joestump/claude-plugin-sdd).

### Self-test

**Harness, coming.** The end-to-end check `harness init` and `harness stack` will
finish with: a nonce goes in through a real webhook, and the check passes only
when an agent has claimed and completed the todo it became
([SPEC-0018](/specs/stack-installer/spec)).

### Switchboard

**Product.** How work reaches agents: it verifies incoming webhooks, routes them
into durable [todos](#todo) on [queues](#queue), and rings a live session with a
[doorbell](#doorbell). In the stack, Switchboard is **dispatch**.
[Switchboard](https://switchboard.stump.wtf/docs/),
[the stack](./harness-switchboard-cairn).

## T

### Team

**Switchboard, coming.** An owner that several [users](#user) share, with
member, admin and owner roles. Every resource belongs to exactly one user or
team. [Switchboard ADR-0038](https://switchboard.stump.wtf/docs/decisions/ADR-0038-teams-and-tenancy).

### Todo

**Switchboard.** One unit of work, made from a verified webhook and carrying its
payload. It moves from `pending` to `claimed` to `done` or `failed`. Claimed with
`claim_next`, it is held under a [lease](#lease).
[Switchboard: the lifecycle](https://switchboard.stump.wtf/docs/getting-started/concepts#the-lifecycle).

### Trace

**Cairn.** A whole agent run, shared: a span waterfall plus an activity stream.
It was called a **trajectory**, and the rename is in flight: the stored share type
and some tool names still say `trajectory` or `run`. Harness still uses
"trajectory" for an agent's own session transcript.
[Cairn: trace](https://cairn.stump.wtf/docs/share-types/#trc-trace).

### Triggered harness

**Harness.** A [one-shot](#one-shot) with `triggers`: each event on a bound
source fires one run, and the event reaches the run as a private file named by
`HARNESS_EVENT_FILE`. A `[channel.*]` source listens for Switchboard doorbells
from behind NAT; a `[webhook.*]` source's HTTP listener is **coming**. A
`schedule` can sit alongside `triggers` as a safety net.
[ADR-0021](/decisions/adr-0021-on-demand-one-shots),
[events and schedules](./harness-switchboard-cairn#events-and-schedules-are-complementary).

### TTL

**Cairn.** How long an [artifact](#artifact) lives: 7 days by default, 30 days at
most. Switchboard uses TTL for a [lease](#lease) or an [endpoint](#endpoint)'s
lifetime. [Cairn: expiry](https://cairn.stump.wtf/docs/guides/first-share/#expiry).

## U

### User

**Switchboard, Cairn.** Anyone who signs in to an [instance](#instance). Every
endpoint, webhook and artifact belongs to a user (or, **coming** to Switchboard,
a [team](#team)).
[Switchboard ADR-0038](https://switchboard.stump.wtf/docs/decisions/ADR-0038-teams-and-tenancy).

## V

### Vend

**Switchboard.** To create an [endpoint](#endpoint), in the web wizard or with
the operator CLI. The scope is fixed when you vend, and the token is shown once.
[Switchboard: first endpoint](https://switchboard.stump.wtf/docs/getting-started/first-endpoint#vend-in-the-browser).

## W

### Work order

**Switchboard.** The `work_order` object a routing rule attaches to a
[lane](#lane) todo: where the work came from, which rule admitted it, and whether
the sender was verified. It picks the task and grants nothing; its contents are
untrusted data.
[Switchboard: what a worker receives](https://switchboard.stump.wtf/docs/guides/handoff-lanes#what-a-worker-receives).

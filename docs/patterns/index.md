---
title: "Patterns"
sidebar_label: "Overview"
sidebar_position: 0
---

# Patterns

Each pattern here is one recurring job, done with Harness, Switchboard and Cairn
together: what each tool carries, the config that wires them, and the trap that
catches people. Teams building on the stack kept rebuilding these inside their
own coordinators, so they are written down once.

Patterns live with the product that carries most of the weight. This index lists
all of them:

| Pattern | What it solves | Lives in |
|---|---|---|
| [Author/reviewer pairs on different model families](./author-reviewer-pairs) | a reviewer that doesn't share its author's blind spots, and never approves its own work | Harness |
| When to react to events and when to run on a schedule (**coming**) | doorbells for work that arrives, schedules for work that sweeps | Harness |
| Human in the loop through notifications, not a ticket queue (**coming**) | asking a person to sign off without making them drain a queue | [Switchboard](https://switchboard.stump.wtf/docs/) |
| Environment binding (**coming**) | one queue and endpoint per environment, refusing work that arrives on the wrong one | [Switchboard](https://switchboard.stump.wtf/docs/) |
| Receipts and evidence (**coming**) | what goes in Cairn, what goes in the tracker, and what goes in the repo | [Cairn](https://cairn.stump.wtf/docs/) |

Every pattern assumes the roles on [the stack](/guides/harness-switchboard-cairn)
page: the tracker and the front door stay yours, Switchboard dispatches, Harness
supervises, and Cairn keeps the evidence.

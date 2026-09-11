---
title: "Getting started"
sidebar_label: "Overview"
sidebar_position: 0
---

# Getting started

These guides take you from nothing installed to a small, always-on agent
setup running on your own Mac or Linux box. By the end you will have:

- **Harness**, supervising agent CLIs (Claude Code, Crush, Codex) as a service.
  It restarts them when they fall over, fires scheduled sweeps on a cron, and
  lets you attach to any of them from a terminal or over SSH.
- **Switchboard**, turning webhooks into durable todos. It pushes those todos
  to your always-on agents the moment they arrive, so the agents don't poll.
- **Cairn**, giving your agents a place to publish reports and handoffs as
  shareable links.

You do not need all three. Harness is useful on its own from the first page;
Switchboard and Cairn slot in when you want real-time work and shareable output.

## The path

Read these in order the first time; each one assumes the previous.

| # | Guide | You finish with |
|---|-------|-----------------|
| 1 | [Install](./install) | `harness` on your `PATH`, verified with `harness doctor` |
| 2 | [Run the daemon as a service](./run-as-a-service) | the supervisor surviving logout and reboot (systemd or launchd) |
| 3 | [Your first supervised agent](./first-agent) | a Crush or Claude Code session you can attach to, restart, and read logs from |
| 4 | [Scheduled sweeps](./scheduled-sweeps) | an unattended agent run on a cron, with outcomes you can trust |
| 5 | [Push events with MCP channels](./push-events) | an always-on agent that wakes on a Switchboard todo |
| 6 | [Observability](./observability) | post-mortems with `harness logs`, the chatroom, `doctor`, and the SSH cockpit |
| 7 | [How Harness, Switchboard and Cairn fit together](./harness-switchboard-cairn) | the whole loop, end to end |
| — | [Troubleshooting](./troubleshooting) | answers for the usual "it's green but nothing happens" moments |

The [Usage](/usage) section is the reference behind these guides: every verb,
every config key, every flag.

## Before you start

- **A Mac or a Linux box** you can leave running. A laptop works; scheduled
  sweeps notice when it wakes from sleep.
- **At least one agent CLI**, installed and logged in as the same user that will
  run Harness: [Claude Code](https://claude.com/claude-code),
  [Crush](https://github.com/charmbracelet/crush), or Codex.
- **Credentials for that agent**, such as `$ANTHROPIC_API_KEY` or a prior
  interactive login. Harness never asks for them. It just starts the CLI with
  the environment you give it.

:::note Version

These guides track `main`. Several features they use are newer than the latest
tagged release: `CRON_TZ`, `catch_up`, `timeout`, `on_overlap`, `keep_runs`,
`prompt_file`, run history, the agent-activity view of `harness logs`, and the
chatroom. [Install](./install) shows how to build from `main` until a release
catches up.

:::

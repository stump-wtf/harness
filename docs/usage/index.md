---
title: "Usage"
sidebar_label: "Overview"
sidebar_position: 0
---

# Usage

This guide covers every supported Harness feature — what each one does, how to
configure it, and the exact commands to drive it from the keyboard or a script.

All commands below assume the `harness` binary is on your `$PATH` and the
`harness daemon` is running (see [Quickstart](./quickstart) if you're new).

## The shape of Harness

Harness is a single Go binary with two roles, modeled after `systemctl`:

- **`harness daemon`** — long-lived supervisor. It owns every harness: the
  processes, their PTYs, daemon-side scrollback, restart policy, and state. Run
  it once, supervised by init.
- **`harness`** — thin client. Open the **TUI dashboard** with no arguments, or
  run one-shot **verbs** (`list`, `start`, `logs`, ...) for scripting. A client
  can die at any moment without affecting a single harness.

**A harness** is any long-running command you want kept alive and
re-attachable — an agent CLI, a REPL, a watcher, a background service.

## What you can do

| Area | Docs |
|------|------|
| Install & quickstart | [Quickstart](./quickstart) |
| CLI verbs (list, start, stop, logs...) | [CLI reference](./cli) |
| Configuration (`harness.toml`) | [Configuration](./configuration) |
| Supervision & restart policy | [Supervision](./supervision) |
| The TUI dashboard | [Cockpit TUI](./tui) |
| Project-scoped compose (`up` / `down` / `ps`) | [Projects](./projects) |
| Remote access over SSH | [Remote access](./remote) |

## Where the architecture comes from

Harness's engineering decisions are recorded as Architecture Decision Records
and specifications. If you want the *why* behind any behavior here, see
[Architecture Decisions](/decisions) and [Specifications](/specs).

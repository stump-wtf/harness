---
title: "Quickstart"
sidebar_position: 1
---

# Quickstart

Get a harness up and attached in a couple of minutes.

## 1. Install

The preferred method is Homebrew (builds from source, so no macOS Gatekeeper
prompt):

```sh
brew tap stump-wtf/tap
brew install harness
```

Or build from source (Go 1.22+):

```sh
git clone https://gitea.stump.rocks/stump.wtf/harness.git
cd harness
go install ./cmd/harness
```

## 2. Configure a harness

Create `~/.config/harness/harness.toml` and define what to run. A minimal
always-on harness:

```toml
[harness.heartbeat]
cmd = "sh"
args = ["-c", "while true; do echo $(date); sleep 60; done"]
description = "prints the time once a minute"
enabled = true
```

The full field reference is in [Configuration](./configuration) and
`harness.toml.example` in the repo.

## 3. Run the daemon

Start the supervisor. Easiest in a terminal:

```sh
harness daemon
```

Or detach it into the background (dev convenience — prefer an init service for
production, see [Supervision](./supervision)):

```sh
harness daemon --detach
```

## 4. Drive it

From the same or another terminal:

```sh
harness list          # see every harness and its state
harness attach heartbeat   # attach to it as a live terminal (Ctrl-C to detach)
harness describe heartbeat # show its details
harness logs heartbeat      # read its tail log
```

`harness` with no arguments opens the full TUI dashboard.

## 5. Check the plumbing

```sh
harness doctor
```

prints a health report: config parses, the daemon is reachable, harnesses are
in their expected states, and the client/daemon protocol versions agree.

That's the whole loop. Next: [the CLI reference](./cli).

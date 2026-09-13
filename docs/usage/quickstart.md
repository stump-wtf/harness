---
title: "Quickstart"
sidebar_position: 1
---

# Quickstart

Get a harness up and attached in a couple of minutes. For the longer
walkthrough — running the daemon as a service, supervising real agents,
scheduled sweeps and push events — start with [Getting started](/guides).

## 1. Install

Homebrew builds from source, so there is no macOS Gatekeeper prompt. `--HEAD`
builds `main`, which these docs track:

```sh
brew tap stump-wtf/tap
brew install --HEAD harness
```

Or build from source (Go 1.26+):

```sh
git clone https://github.com/stump-wtf/harness.git
cd harness
go install ./cmd/harness
```

## 2. Configure a harness

Create `~/.config/harness/harness.toml` and define what to run. A minimal
always-on harness:

```toml
[harness.heartbeat]
harness = "generic"
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

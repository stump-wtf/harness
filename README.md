# Harness

> `systemctl` for your agents.

**Harness** is a client-server TUI for supervising, attaching to, and *hopping
between* long-running terminal processes — agent CLIs (Claude Code, Crush),
REPLs, watchers — built in Go on the
[Charmbracelet](https://github.com/charmbracelet) ecosystem. The successor to
[`zsh-harnessd`](https://github.com/stump-wtf/zsh-harnessd).

A single `harness` binary has two faces:

- **`harness daemon`** — long-lived supervisor. Owns every harness: the process,
  its PTY, daemon-side scrollback, restart policy, and state.
- **`harness`** — thin client. Open the keyboard-driven **dashboard** with no
  arguments, or run one-shot **verbs** (`list`, `start`, `logs`, ...) to script
  it. Locally over a Unix socket, or remotely over SSH
  ([Wish](https://github.com/charmbracelet/wish)).

Think `tmux` + `systemctl` + an agent-ops dashboard in one Go binary.

## Docs

**Full usage documentation — install, quickstart, CLI reference, config,
supervision, TUI, projects, remote access — lives on the docs site:**

- **https://stump-wtf.pages.stump.rocks/harness/** ← the current build
- **https://stump-wtf.github.io/harness/** ← public mirror

The site also publishes this project's architecture decisions and
specifications. Source of truth for these docs lives in `docs/` and `docs-site/`
in this repo.

## Install

### Homebrew (preferred)

```sh
brew tap stump-wtf/tap
brew install harness
```

### From source

Requires Go 1.22+.

```sh
git clone https://gitea.stump.rocks/stump.wtf/harness.git
cd harness
go install ./cmd/harness
```

## Quickstart

```sh
harness daemon              # run the supervisor (or run it as a service)
harness                     # open the TUI dashboard
harness list                # see every harness and its state
harness attach foo          # attach to harness "foo" as a live terminal
```

Define what to supervise in `~/.config/harness/harness.toml`:

```toml
[harness.heartbeat]
cmd = "sh"
args = ["-c", "while true; do echo $(date); sleep 60; done"]
enabled = true
```

Then `harness doctor` verifies config, daemon, and state. The full config
reference and every verb are in the docs above.

## Status

**Alpha — and self-hosting.** `v0.1.0` is tagged, and Harness supervises real
work daily. Everything in the docs is implemented and exercised, but the TOML
schema and daemon protocol can still change before v1.

## Development

Origin of truth is [gitea.stump.rocks/stump.wtf/harness](https://gitea.stump.rocks/stump.wtf/harness),
mirrored read-only to [github.com/stump-wtf/harness](https://github.com/stump-wtf/harness).
Do work against Gitea.

```sh
make check       # fmt + vet + test + race (the CI gate)
make test        # go test ./...
make lint        # fmt + vet
```

## License

[MIT](LICENSE)

# Harness

> `systemctl` for your agents.

**Harness** is a client-server TUI for supervising, attaching to, and *hopping
between* long-running coding agents — Claude Code, Crush, Codex, and the other
agents [agent-trace](https://github.com/stump-wtf/agent-trace) can read — built
in Go on the [Charmbracelet](https://github.com/charmbracelet) ecosystem. The
successor to [`zsh-harnessd`](https://github.com/stump-wtf/zsh-harnessd).

Harness is bound to agent-trace: every run is recorded as the agent's
normalized trace, so it supervises only agents agent-trace supports. It is not
a general process manager. Run arbitrary processes under your init system
(systemd, launchd).

A single `harness` binary has two faces:

- **`harness daemon`** — long-lived supervisor. Owns every harness: the process,
  its PTY, daemon-side scrollback, restart policy, and state.
- **`harness`** — thin client. Open the keyboard-driven **dashboard** with no
  arguments, or run one-shot **verbs** (`list`, `start`, `logs`, ...) to script
  it. Locally over a Unix socket, or remotely over SSH
  ([Wish](https://github.com/charmbracelet/wish)).

Think `tmux` + `systemctl` + an agent-ops dashboard in one Go binary.

## Docs

**Documentation:** https://stump-wtf.github.io/harness/

- **[Getting started](https://stump-wtf.github.io/harness/guides)** — the 0-to-1
  path: install, run the daemon as a service, supervise your first agent,
  scheduled sweeps, push events with MCP channels, observability, and how
  Harness fits with [Switchboard](https://switchboard.stump.wtf/docs/) and
  [Cairn](https://cairn.stump.wtf/docs/intro/).
- **[Usage](https://stump-wtf.github.io/harness/usage)** — the reference: every
  verb, config key, and flag.

The site also publishes this project's architecture decisions and
specifications. Source for the docs lives in `docs/` and `docs-site/` in this
repo.

## Install

### Homebrew

```sh
brew install stump-wtf/tap/harness
```

Installing by the fully qualified name is a one-liner and the only form that
works out of the box: since [Homebrew 6.0.0](https://brew.sh/2026/06/11/homebrew-6.0.0/)
non-official taps require explicit trust, and a fully qualified name trusts just
that one formula rather than the tap and everything it may ever contain. The old
`brew tap stump-wtf/tap` + `brew install harness` form now fails because the
short name needs the tap loaded and the tap is untrusted.

That installs the latest tagged release. To build unreleased work from `main`
instead, add `--HEAD`:

```sh
brew install --HEAD stump-wtf/tap/harness
```

### From source

Requires Go 1.26+ (older Go toolchains download it automatically).

```sh
git clone https://github.com/stump-wtf/harness.git
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
[harness.crush]
harness = "crush"
workdir = "~/src/my-project"
enabled = true
```

`harness` is required and names the kind. `crush`, `claude-code` and `codex`
are the agents agent-trace can read. `command` runs any other program, with no
shell: its `argv` array is the whole process, so each element reaches the
program as one byte-identical argument.

The `generic` kind, which ran an arbitrary `sh` command, is deprecated and is
being removed (ADR-0033); a harness must be an agent agent-trace can read.
`command` is what replaces it for a program that is not an agent —
`harness.toml.example` has the details, including why a `command` harness has
no `args`, and how it runs on a `schedule` or `triggers` with its argv
templated over the run.

Then `harness doctor` verifies config, daemon, and state. The full config
reference and every verb are in the docs above.

## Status

**Alpha — and self-hosting.** `v0.6.0` is the latest tag, and Harness supervises
real work daily. Everything in the docs is implemented and exercised on `main`,
but the TOML schema and daemon protocol can still change before v1.

## Development

Development happens on a private Gitea instance, which is the origin of truth;
[github.com/stump-wtf/harness](https://github.com/stump-wtf/harness) is its
public copy, updated on every push.

- **Found a bug or want something?**
  [Open a GitHub issue](https://github.com/stump-wtf/harness/issues/new/choose).
  Issues there are read and triaged onto the canonical tracker. The bug form
  asks for what we need to reproduce it: `harness --version`, how you
  installed it, your OS and agent CLI, the relevant `harness.toml` table, and
  `harness doctor` output.
- **Have a fix?** Open an issue describing it and link your branch or fork.
  Pull requests can't be merged on GitHub, because each sync from the canonical
  repository overwrites it, so a maintainer carries the change across and
  credits you.
- **Security issue?** Don't open a public issue; see [SECURITY.md](SECURITY.md).

```sh
make check       # fmt + vet + test + race + fuzz (the CI gate)
make test        # go test ./...
make lint        # fmt + vet
make fuzz        # fuzz targets for FUZZTIME each (default 30s)
```

## License

[MIT](LICENSE)

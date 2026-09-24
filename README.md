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
[harness.heartbeat]
harness = "generic"
args = ["-c", "while true; do echo $(date); sleep 60; done"]
enabled = true
```

`harness` is required and names the kind: `crush`, `claude-code`, or `codex`
for an agent CLI, `generic` for anything else (it runs `sh`, so an arbitrary
command goes in `args` as `["-c", "…"]`).

Then `harness doctor` verifies config, daemon, and state. The full config
reference and every verb are in the docs above.

## Status

**Alpha — and self-hosting.** `v0.5.0` is the latest tag, and Harness supervises
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
make check       # fmt + vet + test + race (the CI gate)
make test        # go test ./...
make lint        # fmt + vet
```

## License

[MIT](LICENSE)

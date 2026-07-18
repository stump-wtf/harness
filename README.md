# Harness

> `systemctl` for your agents.

**Harness** is a client-server TUI for supervising, attaching to, and *hopping
between* long-running terminal processes — agent CLIs (Claude Code, Crush),
REPLs, watchers — built in Go on the
[Charmbracelet](https://github.com/charmbracelet) ecosystem. It is the
successor to [`zsh-harnessd`](https://github.com/stump-wtf/zsh-harnessd), which
proved the idea as a zsh + tmux + systemd plugin.

A **harness** is any long-running command you want kept alive and
re-attachable. `harnessd` (the daemon) supervises a set of harnesses, each in
its own PTY with daemon-owned scrollback. `harness` (the client) is a
keyboard-driven dashboard: see every harness and its state, hop into any one as
a live terminal, switch between **profiles** (named configurations of
harnesses), start/stop/edit — locally over a Unix socket, or remotely over SSH
via [Wish](https://github.com/charmbracelet/wish).

Think `tmux` + `systemctl` + a purpose-built agent-ops dashboard, as a single
Go binary — with tmux demoted from foundation to optional escape hatch.

## Status

**Design phase.** No code yet — the architecture and UX are specified and the
backlog is being planned from the specs. The daemon knows nothing about what
runs inside a harness; that stays a feature.

## Design artifacts

- **ADRs** — [`docs/adrs/`](docs/adrs/): eight accepted-direction decisions
  (Go + Charm, daemon/thin-client split, native multiplexer with tmux backend,
  Unix socket + Wish transport, supervision layers, TOML config + profiles,
  scrollback/state persistence, security model).
- **Specs** — [`docs/openspec/specs/`](docs/openspec/specs/):
  [`tui`](docs/openspec/specs/tui/spec.md) (screens, keybindings, states),
  [`daemon-protocol`](docs/openspec/specs/daemon-protocol/spec.md) (framing,
  control RPCs, attach stream, backpressure),
  [`harness-lifecycle`](docs/openspec/specs/harness-lifecycle/spec.md) (the
  state machine).
- **Charm ecosystem map** —
  [`docs/charm-ecosystem-map.md`](docs/charm-ecosystem-map.md): every layer of
  the architecture mapped to a maintained Charm package.
- **Visual design exploration** — [`docs/design/`](docs/design/): the Claude
  Design exploration (screenshots + a terminal-native design system) that sets
  the visual direction — calm ops cockpit, ANSI neon on blue-black, the "hop"
  as the signature moment. Open `docs/design/Harness.dc.html` in a browser for
  the full exploration.

## Naming

- **`harnessd`** — the daemon
- **`harness`** — the CLI/TUI client (`harness` with no args opens the TUI;
  `harness list`, `harness attach foo`, etc. mirror the daemon RPC for scripts)

## Development

Origin of truth is
[gitea.stump.rocks/stump.wtf/harness](https://gitea.stump.rocks/stump.wtf/harness),
mirrored to [github.com/stump-wtf/harness](https://github.com/stump-wtf/harness).
Do work against Gitea.

## License

[MIT](LICENSE)

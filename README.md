# Harness

> `systemctl` for long-running terminal processes.

**Harness** is a client-server TUI for supervising, attaching to, and *hopping
between* long-running terminal processes, built in Go on the
[Charmbracelet](https://github.com/charmbracelet) ecosystem.

A **harness** is any long-running command you want kept alive and
re-attachable. The `harness` binary is a single Go binary with two faces:
**`harness daemon`** (the supervisor, run as a user service) owns all harness
state — each in its own PTY with daemon-owned scrollback — while **`harness`**
(the client, run interactively) is a keyboard-driven dashboard: see every
harness and its state, hop into any one as a live terminal, switch between
**profiles** (named configurations of harnesses), start/stop/edit — locally
over a Unix socket, or remotely over SSH via
[Wish](https://github.com/charmbracelet/wish).

Think `tmux` + `systemctl` + a purpose-built ops dashboard, as a single
Go binary — with tmux demoted from foundation to optional escape hatch.

## Status

**Alpha — and self-hosting.**
[`v0.1.0`](https://gitea.stump.rocks/stump.wtf/harness/releases/tag/v0.1.0) is
tagged, and Harness now supervises real work daily.

What works today: `harness daemon` supervises each harness in its own PTY with
daemon-owned scrollback; the TUI dashboard lists, starts, stops, edits and hops
between them; `attach` gives you the live terminal; profiles switch whole sets
of harnesses at once; and the one-shot CLI (`list`, `describe`, `logs`,
`doctor`, …) mirrors the daemon RPC for scripting. The optional SSH front door
(`[server]`, public-key only) is implemented but off by default.

Expect alpha edges — the TOML schema and the daemon protocol can still change
before v1. The daemon knows nothing about what runs inside a harness; that
stays a feature.

## Install

### Homebrew (preferred)

```sh
brew tap stump-wtf/tap
brew install harness
```

The formula builds from source, so the binary is compiled locally and never
picks up macOS's `com.apple.quarantine` attribute — no Gatekeeper prompt.

### From source

Requires Go 1.22+.

```sh
git clone https://gitea.stump.rocks/stump.wtf/harness.git
cd harness
go install ./cmd/harness
```

The `harness` binary is installed to your `$(go env GOPATH)/bin` (usually
`~/go/bin` — make sure it's on your `$PATH`).

### Build without installing

```sh
go build -o harness ./cmd/harness
./harness --version
```

### Run it

The single binary serves both roles:

```sh
harness daemon              # start the supervisor (long-lived)
harness                     # open the TUI dashboard against a running daemon
harness list                # one-shot: list harnesses and states
harness attach foo          # one-shot: attach to harness "foo"
harness --help              # full command reference
```

### Running the daemon as a user service

The daemon is meant to be kept alive by your init system (ADR-0005). A
**systemd** `--user` unit:

```ini
# ~/.config/systemd/user/harness.service
[Unit]
Description=Harness supervisor

[Service]
ExecStart=%h/go/bin/harness daemon
Restart=on-failure

[Install]
WantedBy=default.target
```

Then:

```sh
systemctl --user daemon-reload
systemctl --user enable --now harness.service
```

On **macOS**, use the equivalent launchd LaunchAgent (`dev.harness.daemon.plist`)
with `ProgramArguments` set to `<path>/harness daemon`.

## Design artifacts

The ADRs and specs below are published as a browsable site at
**[stump-wtf.github.io/harness](https://stump-wtf.github.io/harness/)**, rebuilt
on every push to `main`.

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

## Development

Origin of truth is
[gitea.stump.rocks/stump.wtf/harness](https://gitea.stump.rocks/stump.wtf/harness),
mirrored to [github.com/stump-wtf/harness](https://github.com/stump-wtf/harness).
Do work against Gitea.

## License

[MIT](LICENSE)

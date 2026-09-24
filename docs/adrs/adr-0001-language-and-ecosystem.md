---
status: accepted
date: 2026-07-18
decision-makers: [joestump]
enables: [ADR-0003, ADR-0004]
related: [ADR-0002]
---

# ADR-0001: Language and ecosystem — Go + Charmbracelet

## Context and Problem Statement

The predecessor implementation is a zsh plugin (`harnessd()`), a bash launcher
(`harness-run`), a systemd template, and a launchd plist template, glued around
tmux, with `python3`'s `tomllib` for config parsing. It works, but:

* The UX ceiling is low: `harnessd list` prints colored dots; everything
  interactive is "shell out to tmux."
* It spans four languages (zsh, bash, python, unit-file DSL) for one small tool.
* A real client-server TUI needs a PTY layer, a terminal emulator, an RPC layer,
  and a component-based UI — none of which zsh/bash want to host.

We want a single implementation language and a UI toolkit that makes a
keyboard-driven, attach-a-live-terminal dashboard *and* a network daemon
tractable.

## Decision Drivers

* First-class TUI framework with components (list, viewport, table, forms).
* A **terminal emulator** we can embed to render harness PTYs inside our own UI.
* A **PTY** abstraction that's cross-platform (Linux + macOS at minimum).
* A path to **serve the UI over SSH** (remote attach) without a second stack.
* Single static binary; trivial `go install` / Homebrew distribution.
* A toolkit the broader community already reaches for in this exact niche
  (agent CLIs, dev tooling).

## Considered Options

* Option 1 — Go + Charmbracelet (Bubble Tea et al.)
* Option 2 — Rust (ratatui + a PTY crate like `portable-pty` + `russh` for SSH)
* Option 3 — Stay in shell, thicken the tmux veneer (status bar, popups via tmux)
* Option 4 — TypeScript/Node (Ink) for the TUI

## Decision Outcome

Chosen option: **Option 1 — Go + Charmbracelet**, because it is the only option
where *every* moving part of what we want has a maintained, idiomatic library
from one coherent ecosystem:

* [**Bubble Tea**](https://github.com/charmbracelet/bubbletea) — the Elm-style
  TUI runtime (Model / Update / View).
* [**Bubbles**](https://github.com/charmbracelet/bubbles) — list, viewport,
  table, textinput, spinner, help, key-binding registry.
* [**Lip Gloss**](https://github.com/charmbracelet/lipgloss) — styling/layout
  (borders, panes, adaptive light/dark color).
* [**Huh**](https://github.com/charmbracelet/huh) — forms, for `create`/`edit` of
  a harness without dropping to `$EDITOR`.
* [**Wish**](https://github.com/charmbracelet/wish) — an SSH server that serves a
  Bubble Tea program per session, with the PTY and resize wired up. This is the
  remote-attach story for free (ADR-0004).
* [**`x/vt`**](https://pkg.go.dev/github.com/charmbracelet/x/vt) — a virtual
  terminal emulator (parses the harness's output, maintains a screen + scrollback,
  exposes an `InputPipe` and a `Draw`able) — this is what lets us **embed a live
  terminal pane inside the TUI** rather than shelling out (ADR-0003).
* [**`x/xpty`**](https://pkg.go.dev/github.com/charmbracelet/x) /
  `creack/pty` — cross-platform PTY allocation for the supervised process.
* [**`x/ansi`**](https://pkg.go.dev/github.com/charmbracelet/x/ansi),
  [**`x/term`**](https://pkg.go.dev/github.com/charmbracelet/x/term) — ANSI
  parsing and raw-mode/termios helpers.
* [**Glamour**](https://github.com/charmbracelet/glamour) — if we render help /
  READMEs / agent markdown output inside the TUI.
* [**Charm `log`**](https://github.com/charmbracelet/log) — structured logging in
  the daemon.
* [**Fang**](https://github.com/charmbracelet/fang) — the Cobra "starter kit":
  wraps our root command to give styled help/errors, an automatic `--version`,
  `man` generation, and shell completions. The `harness` CLI surface
  (ADR-0002) gets a polished, consistent feel for free.
* [**colorprofile**](https://github.com/charmbracelet/colorprofile) — detects the
  terminal's color depth and degrades our palette gracefully (TrueColor → 256 →
  16 → mono), which matters especially for remote/SSH sessions.
* [**ultraviolet**](https://github.com/charmbracelet/ultraviolet) — Charm's newer
  cell/"drawable" rendering primitives that `x/vt` targets; the substrate for
  compositing the embedded terminal pane into our UI (ADR-0003).

A fuller inventory of the whole ecosystem — every library we'd use now, defer, or
just reference — lives in the [Charm ecosystem map](../usage/charm-ecosystem-map.md).
The short version: **every box in the ADR-0002 architecture maps to a maintained
Charm package**, from PTY up to SSH directory. That coherence *is* the argument
for this ADR.

Go also gives us: one static binary (daemon *and* client in the same executable,
selected by subcommand), effortless concurrency for the supervisor loops and the
socket server, and a `tomllib`-free config story (`BurntSushi/toml`) so we drop
the `python3` runtime dependency.

### The binary shape

One binary, `harness`:

* `harness daemon` — run the daemon (what systemd/launchd starts; ADR-0005).
* `harness` (no args) — connect to the daemon and open the TUI.
* `harness {list,start,stop,restart,status,attach,logs,edit}` — scriptable RPC
  clients that mirror the predecessor's verbs (backward-compatible muscle memory).

### Consequences

* Good, because every capability we need is a first-party Charm package → low
  integration risk, consistent idioms, one dependency graph.
* Good, because remote-over-SSH becomes an adapter, not a rewrite (Wish).
* Good, because it drops the zsh/bash/python/unit-DSL sprawl to essentially one
  language.
* Good, because distribution is a single binary; `go install` and a Homebrew tap
  are trivial.
* Bad, because we leave the "it's just a zsh plugin you source" install
  simplicity behind. The new install is "grab a binary + a service unit."
  (ADR-0005 keeps the unit.)
* Bad, because some `x/*` packages are explicitly *experimental* — APIs can move
  under us. We accept that in exchange for the embedded-terminal capability; pin
  versions and isolate `vt`/`xpty` usage behind our own interface so churn is
  contained.
* Bad, because Go's TUI ↔ terminal-emulator integration (rendering a `vt` screen
  inside Bubble Tea every frame, at speed) is the one genuinely novel piece of
  engineering here — de-risked by ADR-0003 but not free.

### Confirmation

* `go.mod` names the Charm packages this ADR relies on (Bubble Tea, Bubbles,
  Lip Gloss, Huh, Wish, `x/vt`, `x/xpty`, `x/ansi`, `x/term`, `log`,
  `colorprofile`) and `BurntSushi/toml`; no Python runtime is required.
* `cmd/harness` builds the single `harness` binary; `harness daemon` and the
  client verbs are subcommands of it.

## Pros and Cons of the Options

### Option 1 — Go + Charmbracelet

* Good, because every layer (PTY, emulator, TUI runtime, forms, SSH server,
  logging, CLI chrome) has a maintained package from one ecosystem.
* Good, because Wish gives "serve this TUI over SSH" as a batteries-included layer.
* Good, because Go produces one static binary with easy concurrency.
* Bad, because several `x/*` packages are experimental and their APIs move.

### Option 2 — Rust (ratatui)

* Good, because it is entirely capable (`portable-pty`, `russh`, `vt100`) and a
  fine choice on the merits.
* Bad, because the pieces come from *different* crates with less cohesion than
  Charm.
* Bad, because there's no Wish-equivalent "serve this TUI over SSH"
  batteries-included layer. More assembly, weaker ecosystem gravity for this
  specific niche.

### Option 3 — Stay in shell + thicken tmux

* Good, because it needs the least new code.
* Bad, because it can't give us an embedded, styleable UI or a real network
  protocol; we'd be forever constrained to what tmux's status line and popups
  allow. This is the thing we're trying to grow out of.

### Option 4 — TypeScript/Node (Ink)

* Good, because it offers great DX for the UI.
* Bad, because it adds a runtime dependency, has a weak PTY/SSH server story, and
  has GC/startup characteristics we don't want in a resident daemon.

## More Information

* **Related ADR-0002** — the daemon/client split this ecosystem has to serve.
* **Enables ADR-0003** — multiplexing via `x/vt` and `x/xpty`.
* **Enables ADR-0004** — Wish for remote access.
* The [Charm ecosystem map](../usage/charm-ecosystem-map.md) inventories every
  Charm library and its role (adopted, deferred, or reference only).

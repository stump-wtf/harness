# Charm ecosystem map — what we use, defer, or reference

A full pass over [charm.land](https://charm.land) and the
[charmbracelet org](https://github.com/orgs/charmbracelet/repositories) (55+
repos) + the [`x/*`](https://github.com/charmbracelet/x) experimental packages,
mapped onto this project. The point: Charm isn't just "Bubble Tea" — it's a whole
stack that happens to cover *every* layer Harness needs, from PTY up to SSH
directory. Legend: **★ core (v1)** · **◑ likely (v1/v1.x)** · **○ later / optional**
· **▷ reference / inspiration** · **✕ not relevant**.

## Layer 1 — TUI runtime & rendering (the client)

| Lib | Role here | Tier |
|-----|-----------|------|
| [**bubbletea**](https://github.com/charmbracelet/bubbletea) | The TUI runtime (Model/Update/View) for the whole client. | ★ |
| [**bubbles**](https://github.com/charmbracelet/bubbles) | list, viewport, table, textinput, spinner, help, key registry — the dashboard's parts. | ★ |
| [**lipgloss**](https://github.com/charmbracelet/lipgloss) | All styling/layout: panes, borders, the status ribbon, adaptive color. | ★ |
| [**colorprofile**](https://github.com/charmbracelet/colorprofile) | Detect terminal color depth & degrade the palette gracefully (TrueColor→256→16→no-color). Makes the ADR-0008/spec-tui themes robust across terminals **and over SSH**. | ★ |
| [**ultraviolet**](https://github.com/charmbracelet/ultraviolet) | "TUI primitives" — the newer cell/drawable rendering layer that `x/vt` targets (`uv.Drawable`). This is *how* the embedded terminal pane composites into our UI (ADR-0003). | ★ |
| [**harmonica**](https://github.com/charmbracelet/harmonica) | Physics-based spring animation → the "hop" signature moment (spec-tui): smooth slide/flash when switching harnesses. | ◑ |
| [**glamour**](https://github.com/charmbracelet/glamour) | Render markdown inside the TUI (help, a harness's README, agent markdown output). | ◑ |
| [bubblezone](https://github.com/lrstanley/bubblezone) *(community, not Charm)* | Mouse hit-zones if we add mouse support. Not first-party; optional. | ○ |

## Layer 2 — Terminal emulation & PTY (the daemon core — ADR-0003)

This is the layer that makes "bake in the multiplexer" real. It's almost entirely
in [`x/*`](https://github.com/charmbracelet/x).

| Pkg | Role here | Tier |
|-----|-----------|------|
| [**x/vt**](https://pkg.go.dev/github.com/charmbracelet/x/vt) | The virtual terminal emulator: parse harness output → screen + scrollback, `InputPipe`, `Draw`. **The heart of ADR-0003.** | ★ |
| [**x/xpty**](https://github.com/charmbracelet/x/tree/main/xpty) | Cross-platform PTY allocation for the supervised process. | ★ |
| **x/conpty** | Windows ConPTY backing for `xpty` — the path to eventual Windows support. | ○ |
| **x/ansi** | ANSI/escape-sequence encode & parse — under `vt` and for anything we emit. | ★ |
| **x/cellbuf** | Cell-based screen buffer — backs diff-based repaints of the terminal pane. | ★ |
| **x/input** | Keyboard/mouse event parsing — decoding client input on the attach path. | ★ |
| **x/term** / **x/termios** | Raw mode, termios, size — low-level terminal control. | ★ |
| **x/wcwidth** | East-Asian/wide & combining char widths — **exactly the correctness risk ADR-0003 flagged**; use it so agent output with emoji/CJK renders right. | ★ |
| **x/vttest** / **x/vt/vttest** | VT conformance test helpers → the emulator golden-test harness (de-risks ADR-0003). | ◑ |
| **x/vcr** | Record/replay of terminal interactions (recorder/matcher/marshaler/hooks) → capture real Claude Code / Crush sessions as fixtures for emulator tests, and a seed for future session recording (ADR-0007). | ◑ |
| [**sequin**](https://github.com/charmbracelet/sequin) | Human-readable ANSI decoder — a debugging tool while building/hardening the emulator. | ▷ |
| **x/editor** | Launch `$EDITOR` cleanly — backs the "edit raw TOML" escape hatch (spec-tui). | ◑ |
| **x/mosaic** | Render images in the terminal — niche; only if we ever show images. | ○ |

## Layer 3 — CLI surface (the `harness` command — ADR-0001)

| Lib | Role here | Tier |
|-----|-----------|------|
| [**fang**](https://github.com/charmbracelet/fang) | Cobra starter kit: styled help/usage, styled errors, auto `--version`, `man` generation, shell completions. Wrap the root command → a polished `harness` CLI for free. | ★ |
| [**log**](https://github.com/charmbracelet/log) | Structured, colorful daemon logging. | ★ |
| [**huh**](https://github.com/charmbracelet/huh) | Forms for create/edit-harness (writes back to TOML, ADR-0006); can also drive an interactive `harness init`. | ★ |
| [**gum**](https://github.com/charmbracelet/gum) | Not linked in, but ▷ inspiration for our palette/confirm affordances; users could script around `harness` with it. | ▷ |

## Layer 4 — Server, SSH & remote (ADR-0004 / ADR-0008)

| Lib | Role here | Tier |
|-----|-----------|------|
| [**wish**](https://github.com/charmbracelet/wish) | SSH server that serves our Bubble Tea TUI per session (PTY+resize wired). The entire remote-attach story. | ★ (remote) |
| [**wishlist**](https://github.com/charmbracelet/wishlist) | "The SSH directory" — a menu of SSH endpoints. **Directly enables the multi-host story:** one launcher listing every machine's `harness daemon`, hop across boxes. Answers "hop into harnesses *anywhere*." | ◑ |
| [**keygen**](https://github.com/charmbracelet/keygen) | Generate the daemon's Ed25519 **host key** on first run (ADR-0008 host-key verification). | ◑ |
| [**melt**](https://github.com/charmbracelet/melt) | Back up/restore that host key as seed words — so a re-provisioned host keeps its identity and clients don't trip host-key-changed warnings. | ○ |
| [**promwish**](https://github.com/charmbracelet/promwish) | Prometheus middleware for Wish → daemon metrics (attach counts, sessions) for Joe's monitoring. | ○ |
| [**soft-serve**](https://github.com/charmbracelet/soft-serve) | Not a dependency — ▷ the reference implementation of a real Wish-based multi-user SSH TUI daemon. Read it for how they do auth, access levels, and TUI-over-SSH at scale. | ▷ |
| [**confettysh**](https://github.com/charmbracelet/confettysh) | ▷ tiny Wish example; nice for a first spike. | ▷ |

## Layer 5 — AI-harness awareness (the open question — README Q2)

Harness is agent-agnostic today. If we lean into agent-*aware* affordances, Charm
already ships the Go AI plumbing (it's what Crush is built on):

| Lib | Role here | Tier |
|-----|-----------|------|
| [**fantasy**](https://github.com/charmbracelet/fantasy) | "Build AI agents with Go, multiple providers, one API." If Harness ever grows *its own* agentic features (e.g. a supervisor that summarizes what an idle agent is waiting on), this is the substrate. | ○ (depends on Q2) |
| [**catwalk**](https://github.com/charmbracelet/catwalk) | Catalog of LLM providers/models (powers Crush). Useful if we detect *which* model/provider a harness is running to badge it in the UI. | ○ (depends on Q2) |
| [**crush**](https://github.com/charmbracelet/crush) | ▷ Not a dep — but it's a flagship harness we run, and a north-star for TUI-agent UX. Our emulator golden tests (x/vcr) should include real Crush sessions. | ▷ |

## Layer 6 — Docs, demos, distribution

| Tool | Role here | Tier |
|------|-----------|------|
| [**vhs**](https://github.com/charmbracelet/vhs) | Scripted terminal recordings → the README demo tape (the "hop" in motion) and regression GIFs. spec-tui asks for one. | ◑ |
| [**freeze**](https://github.com/charmbracelet/freeze) | Static images of terminal output/code → polished screenshots for docs/marketing. | ○ |
| [**vhs-action**](https://github.com/charmbracelet/vhs-action) | Keep demo GIFs fresh in CI. | ○ |
| homebrew-tap / scoop-bucket / winget / nur | ▷ Distribution patterns to mirror for shipping the `harness` binary. | ▷ |

## Not relevant

`pop` (email), `glow` (standalone md reader — we embed glamour instead),
`skate` (KV store — our state is a small file, ADR-0007), `git-lfs-transfer`,
`hotdiva2000`, `runway` (3D models), `tree-sitter-vhs`, `openai-go` (fantasy/
catwalk cover our needs), `mods` (standalone LLM CLI).

## The takeaway for the architecture

The ecosystem *confirms* the ADR-0003 bet: Charm has a complete, coherent stack
from **PTY (`x/xpty`/`conpty`) → emulation (`x/vt`, `x/ansi`, `x/cellbuf`,
`x/wcwidth`) → rendering (`ultraviolet`, `lipgloss`) → TUI (`bubbletea`,
`bubbles`) → CLI (`fang`) → SSH server (`wish`) → SSH directory (`wishlist`) →
key lifecycle (`keygen`, `melt`) → testing (`x/vttest`, `x/vcr`, `exp/teatest`) →
docs (`vhs`, `freeze`)**. Every box in the ADR-0002 architecture diagram maps to a
maintained Charm package. That's the strongest possible argument for ADR-0001, and
it means "bake it in" is assembling first-party parts, not inventing a terminal
stack from scratch.

---
status: draft
date: 2026-07-18
implements: [ADR-0001, ADR-0002, ADR-0003, ADR-0006, ADR-0007]
requires: [SPEC-0002]
---

# SPEC-0001: The TUI

## Overview

`harness` (no args) opens a keyboard-driven dashboard onto the daemon: see
every harness and its state, **hop** into any one as a live terminal, switch
between **profiles** ("configurations"), and start/stop/edit — without ever
touching `systemctl`, `tmux`, or `$EDITOR` unless you want to. Built with
Bubble Tea / Bubbles / Lip Gloss / Huh (ADR-0001) as a thin client of the
daemon protocol (SPEC-0002). Layouts, visual direction, and the design
exploration live in `design.md` and `docs/design/`.

## Requirements

### Requirement: Mode Machine

The app SHALL be a small mode machine with two primary modes — **Dashboard**
(the list/overview, default landing screen) and **Attached** (full-attention
live terminal for one harness) — plus overlays that can appear over either
mode: command palette, profile switcher, harness form, confirm dialogs, and
help.

#### Scenario: Detach returns home

- **WHEN** the user detaches from an attached harness (`Esc` or detach chord)
- **THEN** the TUI returns to the Dashboard and the harness keeps running

### Requirement: Dashboard

The Dashboard SHALL show a list of harnesses filtered to the active profile
(with a toggle to show all), each row carrying status glyph, name, state,
restart count (`↻`), and uptime/next-action. It SHALL show a detail/peek pane
for the selected harness — a live read-only tail of its output (streamed via
SPEC-0002 snapshot+tail) plus its config summary (`cmd`/`cwd`/`env`/exit/
started/backend). The header SHALL show app name, active profile, and daemon
identity (`local` or `user@host`); the footer SHALL be a key bar (`?` expands
to full help).

#### Scenario: Glance before you hop

- **WHEN** the user moves the selection to a different harness
- **THEN** the peek pane switches to a live read-only tail of that harness
  without attaching

### Requirement: Attached Mode

Attached mode SHALL render the harness's actual terminal full-width/height
from the daemon's `x/vt` screen — colors, cursor, and TUI apps inside it all
work; keystrokes forward straight to the PTY. A thin status ribbon SHALL
always show which harness is being driven, its state, and the detach hint. A
rebindable detach chord (default `Ctrl-b d`, tmux-muscle-memory-friendly) or
`Esc`-`Esc` SHALL return to the Dashboard. Read-only attaches (ADR-0008) SHALL
show a visible read-only badge and ignore input.

#### Scenario: Driving a live agent

- **WHEN** the user types while attached in interactive substate
- **THEN** the keystrokes go straight to the harness PTY

#### Scenario: Read-only badge

- **WHEN** a harness is attached in read-only mode
- **THEN** a `👁 read-only` badge is visible and input is ignored

### Requirement: Scrollback Substate

From attached mode, `Ctrl-b [` or `PgUp` SHALL enter a scrollback substate
that freezes the view and enables `↑/↓/PgUp/PgDn/g/G` navigation and `/`
search over the daemon-owned scrollback (ADR-0007). `q`/`Esc` SHALL exit
scrollback back to live.

#### Scenario: Searching history

- **WHEN** the user presses `/` in scrollback and enters a term
- **THEN** matches in the scrollback ring are navigable without disturbing
  the live harness

### Requirement: Harness Hop

`[` / `]` SHALL hop to the previous/next harness directly from attached mode —
one keystroke, without returning to the Dashboard. The hop is the product's
signature interaction and SHOULD feel instant and physical (subtle
slide/status-ribbon flash; springs via `harmonica` rather than linear easing).

#### Scenario: One-keystroke hop

- **WHEN** the user presses `]` while attached to harness A
- **THEN** the view switches to the next harness in the list, attached, with
  the ribbon reflecting the new harness

### Requirement: Command Palette

`Ctrl-k` / `:` SHALL open a command palette that fuzzy-matches over verbs
*and* harness names (`attach crush-signal`, `restart reduit-agent`,
`profile signal-ops`, `new`). The palette SHALL mirror the scriptable CLI
verbs 1:1 so the palette and CLI never drift.

#### Scenario: Verb plus target

- **WHEN** the user types "rest redu" in the palette
- **THEN** `restart reduit-agent` is offered and executes on Enter

### Requirement: Profile Switcher

`p` SHALL open a profile switcher listing `[profile.*]` entries with
description and member count. Selecting a profile SHALL filter the dashboard
and offer to start that profile's stopped harnesses — non-destructively:
harnesses outside the profile keep running (ADR-0006).

#### Scenario: Non-destructive switch

- **WHEN** the user switches from profile A to profile B and accepts "start
  stopped"
- **THEN** B's stopped members start, and A's running members keep running

### Requirement: Harness Form

`n` (new) and `e` (edit) SHALL open a Huh form over the harness schema
(`cmd`/`args`/`workdir`/`env_file`/`restart_delay`/`backend`/`description`/
profile membership) that writes back to `harness.toml` (ADR-0006 — file is
truth). `e` SHALL pre-fill from the existing harness. An "edit raw TOML"
escape hatch SHALL open `$EDITOR`.

#### Scenario: Create without leaving the TUI

- **WHEN** the user completes the `n` form
- **THEN** the new harness lands in `harness.toml`, the daemon reloads, and
  the harness appears on the dashboard

### Requirement: Confirmation Guards

Stop, restart, and delete SHALL present a small confirm dialog
(destructive-action guard), skippable via a `--yes`-style setting.

#### Scenario: Accidental stop

- **WHEN** the user presses `x` on a running harness
- **THEN** a confirm dialog intercepts before anything is signaled

### Requirement: State Presentation

The TUI SHALL render lifecycle states (SPEC-0003) with paired glyph + color:
`●` running (green), `◐` degraded/flapping (amber), `◌` starting/restarting
(cyan), `○` stopped (dim), `✖` failed (red), `👁` read-only (blue accent).
Colors SHALL use Lip Gloss adaptive palettes for light/dark terminals, degrade
via `colorprofile` on 256/16-color or mono terminals, and never carry meaning
alone — the glyph always accompanies the color (colorblind-safe).

#### Scenario: Mono terminal

- **WHEN** the TUI runs in a monochrome terminal or degraded SSH client
- **THEN** state remains fully legible from glyphs and text

### Requirement: Keybinding Registry

All bindings SHALL be declared through the Bubbles `key.Binding` registry so
help renders them and a future config can remap them. Defaults: single-key
verbs on the dashboard (`s/x/r/e/n/p`), vim-ish scroll (`j/k/g/G`), `/`
search everywhere, `?` help everywhere, `Ctrl-k` palette everywhere.
tmux-adjacent chords (`Ctrl-b d`, `Ctrl-b [`) are provided but never
*required* — `Esc` always works.

#### Scenario: Discoverable keymap

- **WHEN** the user presses `?` in any mode
- **THEN** the full current keymap renders from the binding registry

### Requirement: Zero And Error States

The TUI SHALL design, not skip, its edge states: **no daemon** (offer to start
it inline, don't just error); **no harnesses / empty profile** (friendly
zero-state with "press `n` to create your first harness"); **config parse
error on reload** (non-fatal banner "using last-good config; line 12: …" —
never a crash, ADR-0006); **daemon disconnect** (reconnecting overlay —
harnesses are fine, only the view dropped, ADR-0002); **flapping harness**
(the `◐` row expands to show last exit code + backoff countdown, one keystroke
to logs).

#### Scenario: Daemon not running

- **WHEN** `harness` starts and no daemon socket is found
- **THEN** the TUI offers to start the daemon inline instead of printing an
  error and exiting

#### Scenario: Bad config reload

- **WHEN** a TOML reload fails to parse
- **THEN** the daemon keeps the last-good config and the TUI shows a
  non-fatal banner with the parse location

## Out of scope for v1

- Tiled multi-pane (several harnesses on screen at once) — ADR-0003 keeps it
  possible; v1 is single-attach + fast hop.
- Full tmux copy-mode selection semantics — v1 is scrollback + search.
- Mouse-first interaction — keyboard-first; mouse is a bonus.

---
id: SPEC-0001
title: The TUI
status: draft
implements: [ADR-0001, ADR-0002, ADR-0003, ADR-0006, ADR-0007]
requires: [SPEC-0002]
---

# Spec — The TUI

- **Status:** Draft (the surface Claude Design should push on hardest)
- **Relates to:** ADR-0001 (Bubble Tea/Bubbles/Lip Gloss/Huh), ADR-0002 (thin
  client), ADR-0003 (embedded `vt` terminal pane), ADR-0006 (profiles),
  ADR-0007 (scrollback)

## Purpose

`harness` (no args) opens a keyboard-driven dashboard onto the daemon: see every
harness and its state, **hop** into any one as a live terminal, switch between
**profiles** ("configurations"), and start/stop/edit — without ever touching
`systemctl`, `tmux`, or `$EDITOR` unless you want to.

## Two primary modes

The whole app is a small mode machine:

1. **Dashboard** — the list/overview. Default landing screen.
2. **Attached** — full-attention live terminal for one harness (the embedded `vt`
   pane, ADR-0003). `Esc`/detach hotkey returns to Dashboard; the harness keeps
   running.

Overlays that can appear over either mode: **Command palette**, **Profile
switcher**, **Harness form** (create/edit, Huh), **Confirm** dialogs, **Help**.

## Dashboard layout

```
┌ harness ───────────────────────────── profile: signal-ops ▾ ──── ⬢ daemon: local ─┐
│                                                                                     │
│  HARNESSES (signal-ops)                    │  crush-signal                          │
│  ● crush-signal      running    ↻0  2h14m  │  ┌───────────────────────────────────┐ │
│  ● claude-src        running    ↻0  2h14m  │  │ …last ~12 lines of live output as  │ │
│  ◐ reduit-agent      degraded   ↻3  restart│  │  a preview of the selected harness │ │
│  ○ backup-watch      stopped              │  │  (read-only peek; Enter to attach) │ │
│                                            │  │                                    │ │
│  + all harnesses (4 hidden by profile)     │  └───────────────────────────────────┘ │
│                                            │  cmd  crush --yolo --data-dir … --cha… │
│                                            │  cwd  ~/.local/share/crush-signal-chan │
│                                            │  env  ~/.config/vault/secrets-static.… │
│                                            │  exit 0    started 14:02    backend na │
│                                                                                     │
├─────────────────────────────────────────────────────────────────────────────────────┤
│ ↵ attach   s start   x stop   r restart   e edit   n new   p profile   / search   ? │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

- **Left:** a [Bubbles `list`](https://github.com/charmbracelet/bubbles) of
  harnesses, filtered to the active profile (toggle to show all). Each row: status
  glyph + name + state + restart count (`↻`) + uptime/next-action.
- **Right:** a **detail/peek pane** for the selected harness — a live read-only
  tail of its output (streamed from the daemon; ADR-0007 snapshot+tail) plus its
  config summary (`cmd`/`cwd`/`env`/exit/started/backend). This is the "glance
  before you hop" affordance.
- **Header:** app name · active **profile** (dropdown → profile switcher) · daemon
  identity (`local` or `user@host` when connected over SSH — ADR-0004).
- **Footer:** a Bubbles `help` key bar (short list; `?` expands to full help).

## Attached mode

```
┌ crush-signal ● running ─────────────────────── ↻0 · 2h14m · native ── ^b d detach ─┐
│                                                                                     │
│   (the harness's actual terminal, full width/height, rendered from the daemon's     │
│    x/vt screen — colors, cursor, TUI apps inside it all work; you type, it goes      │
│    straight to the PTY)                                                              │
│                                                                                     │
│                                                                                     │
├─────────────────────────────────────────────────────────────────────────────────────┤
│ SCROLL ↑↓ · / search · g/G top/bottom · [ ] prev/next harness · Esc dashboard        │
└─────────────────────────────────────────────────────────────────────────────────────┘
```

- The body is the embedded terminal pane (ADR-0003). In **interactive** substate,
  keystrokes forward to the PTY; a **detach chord** (default `Ctrl-b d`, tmux-muscle-
  memory-friendly, rebindable) or `Esc`-`Esc` returns to Dashboard.
- **Scrollback substate** (`Ctrl-b [` or `PgUp`): freezes the view, enables
  `↑/↓/PgUp/PgDn/g/G`, and `/` search over scrollback (ADR-0007). `q`/`Esc` exits
  scrollback back to live.
- `[` / `]` **hop to prev/next harness** without returning to the dashboard — the
  core "hop between harnesses" verb, one keystroke.
- A thin **status ribbon** (top) always shows which harness you're driving, its
  state, and the detach hint — so you never forget you're inside a live agent.
- **Read-only attach** (ADR-0008) shows a `👁 read-only` badge and ignores input.

## Overlays

- **Command palette** (`Ctrl-k` / `:`) — fuzzy over verbs *and* harness names:
  `attach crush-signal`, `restart reduit-agent`, `profile signal-ops`, `new`.
  Mirrors the CLI verbs so the palette and the scriptable CLI stay 1:1.
- **Profile switcher** (`p`) — a list of `[profile.*]` with description + member
  count; selecting one filters the dashboard and offers "start this profile's
  stopped harnesses?" (non-destructive per ADR-0006).
- **Harness form** (`n` new / `e` edit) — a [Huh](https://github.com/charmbracelet/huh)
  form over the harness schema (`cmd`/`args`/`workdir`/`env_file`/`restart_delay`/
  `backend`/`description`/profile membership). Writes back to `harnessd.toml`
  (ADR-0006). `e` on an existing harness pre-fills it. An "edit raw TOML" escape
  hatch opens `$EDITOR` for power users.
- **Confirm** — stop/restart/delete get a small confirm (destructive-action guard),
  skippable with a `--yes`-style setting.
- **Help** (`?`) — full keymap via Bubbles `help`, and a "connected to: …" / daemon
  version line.

## State glyphs & color (maps to spec-harness-lifecycle)

| glyph | state | color (adaptive) | meaning |
|-------|-------|------------------|---------|
| `●` | running | green | supervisor up + process alive |
| `◐` | degraded / flapping | yellow/amber | crash-looping or restart-backoff (ADR-0005) |
| `◌` | starting / restarting | cyan | transient |
| `○` | stopped | gray/dim | intentionally down |
| `✖` | failed | red | exited non-zero, not restarting |
| `👁` | read-only attach | blue accent | you can watch but not type |

Colors via Lip Gloss adaptive palettes so light/dark terminals both look right,
with [`colorprofile`](https://github.com/charmbracelet/colorprofile) degrading the
palette when the terminal (or a remote SSH client) is 256/16-color or mono —
**an explicit ask for Claude Design: define the palette + the two themes, and how
they degrade.** Keep the green/amber/red semantics load-bearing and colorblind-safe
(pair color with glyph, never color alone — the table already does).

## Keybinding philosophy

- **tmux-adjacent where it helps muscle memory** (`Ctrl-b d` detach, `Ctrl-b [`
  scrollback) but rebindable, and never *required* (Esc always works).
- Single-key verbs on the dashboard (`s/x/r/e/n/p`), vim-ish scroll (`j/k/g/G`),
  `/` search everywhere, `?` help everywhere, `Ctrl-k` palette everywhere.
- All bindings declared through the Bubbles `key.Binding` registry so `help`
  renders them and a future config can remap them.

## Empty / error / connection states (design these, don't skip them)

- **No daemon running:** offer to start it (`harness daemon` / the service) inline,
  don't just error.
- **No harnesses / empty profile:** a friendly zero-state with a "press `n` to
  create your first harness" call to action (mirrors today's "no harnesses" hint).
- **Config parse error on reload:** a non-fatal banner ("using last-good config;
  line 12: …") — never a crash (ADR-0006).
- **Daemon disconnect (esp. remote):** a reconnecting overlay; harnesses are fine,
  it's just the view that dropped (ADR-0002).
- **Harness flapping:** the `◐` row expands to show last exit code + backoff
  countdown; one keystroke to open its logs.

## Visual direction (for Claude Design — the brief)

- **Personality:** a calm ops cockpit, not a toy. Dense but legible; think
  `k9s`/`lazygit` restraint, with Charm's warmth. It's watching autonomous agents
  with scary permissions — it should feel *trustworthy and clear about state*.
- **Signature moment:** the **hop** — switching harnesses (`[`/`]` or palette)
  should feel instant and physical (a subtle slide/status-ribbon flash), because
  that's the product's whole promise. [`harmonica`](https://github.com/charmbracelet/harmonica)
  (spring physics) is the tool for making that motion feel right rather than linear.
- **State legibility over decoration:** the single most important thing a glance
  must answer is "which of my agents are healthy, which need me." Status column,
  glyphs, and the degraded/flapping treatment carry that.
- **Deliverables we'd love:** the color system + light/dark themes (+ how they
  degrade via `colorprofile`); the header and status-ribbon treatment; the
  degraded/flapping row design; the attach-mode chrome (how much frame vs.
  bleed-to-edge for the terminal); the empty/error states; a
  [`vhs`](https://github.com/charmbracelet/vhs)-recorded demo tape of the hop, and
  [`freeze`](https://github.com/charmbracelet/freeze) stills for docs.

## Explicitly out of scope for v1 (see ADRs)

- Tiled multi-pane (several harnesses on screen at once) — ADR-0003 keeps it
  possible; v1 is single-attach + fast hop.
- Full tmux copy-mode selection semantics — v1 is scrollback + search.
- Mouse-first interaction — keyboard-first; mouse is a bonus, not the design center.

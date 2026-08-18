---
title: "Cockpit TUI"
sidebar_position: 6
---

# Cockpit TUI

`harness` with no arguments opens the keyboard-driven dashboard onto a running
daemon: every harness and its state at a glance, with a one-keystroke "hop" into
any one as a live terminal. It's a calm ops cockpit — state legibility over
decoration (see `docs/design/` in the repo for the design exploration).

## The dashboard

The dashboard lists every harness with its **state glyph**, name, state, and
restart count, over a faint sub-line carrying that harness's `cmd` (or
`prompt`), backend, last exit code, and PID. Vim-style navigation works
everywhere (`j`/`k`, `g`/`G`).

Beside the list, the **preview pane** is a live read-only view of the selected
harness's screen and nothing else — move the selection and it follows, without
attaching. The list takes only the width its rows need, so the preview gets
every column that's left.

The preview is a real read-only attach under the hood, so the harness inside it
is sized to the pane and follows it: a full-screen agent lays out at the size
you're actually looking at, not at the 80×24 it was spawned with. Hop in with
`↵` and it resizes again to your whole window; detach and the preview takes it
back. If two clients are watching the same harness at different sizes, the
smaller one wins — `harness describe <name>` lists every attached session and
flags the one setting the minimum.

| Key | Action |
|-----|--------|
| `↵` | attach to the selected harness |
| `s` / `x` / `r` | start / stop / restart |
| `e` | edit the harness definition |
| `n` | new harness |
| `d` | delete a harness |
| `p` | profile switcher |
| `a` | show all harnesses |
| `l` | logs |
| `y` | copy the selected harness name (OSC52) |
| `/` | search |
| `^k` or `:` | command palette |
| `?` | full help |
| `q` / `^c` | quit |
| `esc` | back (always unwinds an overlay or attach) |
| `↵`/`y` | confirm a guarded action (e.g. delete) |

## Attached mode

`↵` (or `harness attach <name>`) opens the embedded terminal — a full-window
`x/vt` emulator fed by the daemon's attach stream, with a 1-line status bar at
the bottom: **logo chip · harness identity + state · read-only badge**, and on
the right the compact attached keymap (hop / scrollback / detach / help).

In attached mode, bare keys reach the embedded process (an agent CLI, a REPL)
unchanged. Harness reserves only the `Ctrl-b` prefix and a few intercept keys:

| Key | Action |
|-----|--------|
| `^b d` | **detach** — return to the dashboard (the harness keeps running) |
| `^b [` or `PgUp` | enter scrollback |
| `^b h` / `^b l` | **hop** to the previous / next harness |
| `^b s` / `^b r` | start / restart the attached harness |
| `^b ?` | open the keymap help from attached mode |

`esc` always works to back out of an overlay or a full-screen detach without a
chord.

## The hop

The **hop** is the signature interaction: while attached to one harness, `^b h`
or `^b l` instantly switches the attach to the previous/next harness. It stays
*attached* the whole time — you never drop to the dashboard — and the status bar
flashes a quick animation as you land. The harness never notices; you're just
resubscribing to another screen.

A hop reopens at the **current window size** (never the 80×24 box of a freshly
spawned PTY), so you land exactly where you were.

## Scrollback

Daemon-owned scrollback means history survives detach (ADR-0007). From attached
mode, enter scrollback with `^b [` or `PgUp`:

- Scroll with `j`/`k`, `PgUp`/`PgDn`, or the mouse wheel (`q`/`esc` returns to
  live).
- Scrollback renders **inert text with the harness's colors intact** — it shows
  what the process produced, not a replay of its repaint traffic.
- `harness scrollback` isn't a separate surface: scroll back in-place, then
  `q` returns you to the live stream.

## Read-only attach

`harness attach <name> --ro` opens the harness read-only: you see live output
and scrollback but keystrokes are ignored. The status bar shows a read-only
badge. This is also how a scoped SSH key attaches (see [Remote access](./remote)).

## Palette & search

- `^k` or `:` opens the command **palette** — fuzzy access to every dashboard
  verb without the keymap.
- `/` opens incremental **search** across the list; in scrollback, `/` searches
  the history.

## Zero state

With no harnesses defined, the dashboard shows a friendly zero state pointing
you at `harness new` and the config file, rather than an empty list.

## Remote

The same cockpit is reachable over SSH — a remote session is just another thin
client onto the daemon. See [Remote access](./remote).

---
status: proposed
date: 2026-10-26
decision-makers: []
extends: [ADR-0003, ADR-0007]
related: [ADR-0008, ADR-0040, ADR-0052]
---

# ADR-0055: The guest owns the mouse while it asks for it; harness scrollback is the fallback

> **Not yet accepted.** Proposed in the PR for stump.wtf/harness#954 by the
> handoff lane. Part 1 is implemented in that PR, because the reporter's
> upstream correction of their own report (stump-wtf/harness#29) settled its
> direction and validated it against a real fullscreen Claude Code; nobody on
> the project has written it down, so it is recorded here rather than assumed.
> Part 2 is the part the thread left open, and it needs a maintainer's call.

## Context and Problem Statement

Attach scrollback for a resident `claude-code` session showed the supervisor's
own log lines and shards of repaints instead of the conversation
(issue #954). Investigating it turned up two different guests wearing one
complaint:

1. **The inline renderer.** Claude Code with its default inline (Ink) renderer
   repaints in place and never scrolls the screen, so nothing ever scrolls off
   the top into an emulator scrollback. What the wheel showed was the
   daemon-owned history (`peekLines` → the sanitized durable log), interleaved
   with repaint fragments.
2. **The fullscreen renderer** (`"tui": "fullscreen"`). That guest switches to
   the alternate screen, enables DECSET 1003 and 1006, and pages its own
   transcript from the wheel and PgUp/PageUp. `harness attach` takes the wheel
   and PgUp for its own scrollback unconditionally, so a guest that had
   explicitly asked for the mouse could not be scrolled at all, and the
   scrollback that opened was repaint traffic.

Every emulator on the attach path is built with `SetScrollbackSize(1)`
(`internal/attach/mux.go`, `internal/tui/vtview.go`,
`internal/supervisor/sanitize.go`), which is deliberate: x/vt's default keeps
~85 MiB of cells per harness (SPEC-0002 REQ "Emulator Memory"). So the rows that
scroll off the daemon's screen are discarded by design, and history for a
non-mouse guest is served from the durable log instead.

## Decision

### Part 1 — mouse ownership follows the guest's modes (implemented)

While the guest has mouse reporting on (DECSET 9, 1000, 1002, 1003, encoded with
1006), the wheel, the clicks and PgUp belong to the guest, and the attach client
forwards them encoded the way that mode asks for. While it does not, harness
keeps them and its scrollback answers the wheel. This is tmux and screen
behaviour, which is what operators who also use tmux already expect.

Decided details, all of them chosen so that neither end does something the other
never asked for:

- **The daemon states the modes; the client does not guess them.** A client
  attaches to a screen snapshot, and cells, cursor and SGR survive that repaint
  while a DECSET does not. A guest that enabled the mouse once at startup would
  therefore stay invisible to anything attaching later. The mux shadows the mouse
  modes exactly as it already shadows bracketed paste, and appends them to the
  snapshot and to the coalesced repaint — every tracked mode stated
  explicitly, on or off, so a stale "on" cannot survive a snapshot
  (`internal/mousectl`, `Mux.snapshotLocked`). `Mux.AnsiScreen` stays mode-free:
  capture writes those bytes to a real terminal, where a mouse DECSET would
  change what the operator's own terminal reports.
- **Ask the host terminal only for what the guest wants.** The client declares
  all-motion to its own terminal only when the guest enabled 1003; a guest that
  enabled 1000 gets no hover bytes, a guest that enabled 9 gets presses only,
  and a guest below 1002 gets no drags.
- **Shift still releases the grab (#49).** Shift means "the operator is
  selecting text", and that reading wins over the guest's ownership.
- **A read-only attach keeps the mouse**, because its input is dropped by the
  daemon anyway (ADR-0008) and the wheel is that viewer's only way to scroll.
- **The scrollback substate keeps the mouse**, and `^b [` still opens harness
  scrollback while the guest holds it. PgUp only stops being harness's key when
  the guest owns the mouse.
- **An event outside the guest's screen is nobody's.** With the guest holding the
  mouse, a wheel over harness's own status bar is dropped rather than opening
  scrollback: scrolling the viewer away from a guest that is waiting for the
  event is the failure this issue is about.

### Part 2 — history for a guest that never asked for the mouse (open, not implemented)

Symptom 1 is not fixed by Part 1, and the honest options are:

- **A. Raise the client emulator's scrollback and serve its rows.** Cheap, and
  wrong for the reported guest: an inline Ink renderer repaints the same lines
  by moving the cursor up, so almost nothing scrolls off the top and the
  scrollback fills with the same repaint's ghosts. It also re-opens the memory
  question SPEC-0002 closed.
- **B. Have the mux keep real committed history**: rows the emulator actually
  scrolled (margin-scroll, `IU`, `D` at the last line) are appended to a
  per-harness line history with a byte budget, and attach scrollback reads that
  instead of `peekLines`. This is the shape the original report implies, and it
  is the only option that can be right for every guest, but it needs three
  decisions this lane should not make alone: the budget (the ring is already
  byte-bounded at `DefaultRingLines`; is history a second budget or the same
  one?), how an in-place repaint counts as history at all — for a guest that
  never scrolls there *is* no scrolled-off row, which points at a formatter that
  commits a line when the guest's own protocol says a turn ended (the ADR-0052
  seam) rather than at the emulator — and how it relates to the sanitized
  durable log (ADR-0007) and the screen-as-observability record (ADR-0040)
  instead of becoming a third copy of the same output.
- **C. Leave history alone and fix the reader**, accepting that what the wheel
  shows for an inline agent is the log, and make the log readable for that
  adapter (ADR-0052's territory).

Proposal: take **B** as the direction and route the inline-repaint case through
**B**'s formatter seam, so a conversation's committed turns land in history
without making the emulator remember every repaint. That is a slice of its own:
it changes the memory model, the ring, and at least one adapter, and this PR
deliberately does not half-do it.

## Consequences

- Mouse ownership is decided in one place (`internal/mousectl`) that both ends
  read, so the daemon and the client cannot disagree about who owns the wheel.
- Attach snapshots grow by ~46 bytes of mode state; nothing on the wire changes
  format, so a new client against an old daemon still works for guests whose
  mode change it witnesses itself, and an old client against a new daemon simply
  ignores the extra DECSETs.
- A daemon that is already running must be restarted for the snapshot half to
  take effect; the client half works against an unpatched daemon for guests
  whose mode change the client sees live.
- The shadow is per harness, not per run, exactly as the existing bracketed-paste
  shadow is. A guest killed without restoring its modes (no `?1000l` on exit)
  leaves that harness reading as mouse-owning until its next guest sets a mouse
  mode, and a plain `command` guest started in its place would then receive
  mouse bytes it never asked for. Clearing it needs a start-of-run hook the
  attach path does not have, because history deliberately survives a restart
  (ADR-0007), so this is recorded as a gap rather than solved by inventing one
  here.
- Scrollback for a guest that never asks for the mouse is unchanged here and
  stays a known gap, tracked by this ADR and issue #954.

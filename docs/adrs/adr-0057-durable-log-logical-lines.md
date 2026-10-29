---
status: proposed
date: 2026-10-29
decision-makers: []
extends: [ADR-0007, ADR-0040]
related: [ADR-0033, ADR-0055]
---

# ADR-0057: The durable log records logical lines, and how a row ended is remembered, not inferred

> **Not yet accepted.** Proposed in the PR for stump.wtf/harness#279 by the
> handoff lane. ADR-0007 (as amended) says the durable log holds what the program
> wrote, one line per line; the sanitizer has been storing one line per screen
> *row*, which differs whenever the terminal wrapped a row instead of the program
> ending it. Which signal to use for that distinction is the part nobody on the
> project has written down, so it is recorded here rather than assumed.

## Context and Problem Statement

`harness logs` reads the durable per-harness log, which `ptyHistory` builds by
running the PTY stream through an emulator and writing the rows that scroll off
the screen (ADR-0007 as amended, ADR-0040). In-place repaints produce nothing,
and a printed line produces one row: readable, escape-free, line-oriented.

That holds only while one screen row is one line the program wrote. Text that
reaches the last column is wrapped by the terminal onto the next row without the
program writing a newline, so the row that scrolls off the top is half a logical
line and the row after it is the other half. A long path, a long sentence, a
diff line wider than the PTY: each lands in the log, and so in `harness logs`
and `harness logs --json`, in pieces (#279).

The tempting signal is geometry — a row that fills the terminal width is a wrap.
It is mechanically true of every wrap, and also true of every row the program
*happened* to end at exactly the width: a full-width rule line in a table, a
border a TUI paints, an 80-column log line. Gluing those to their neighbours is
worse than the split, and for a full-screen agent TUI it is a common row shape.
The decision is therefore how to tell "the program ended this row" from "the
terminal ended this row", given that the sanitizer sees only screen states.

## Considered Options

1. **Geometry only** (row width equals the terminal width). Cheap and stateless.
   Rejected: it joins full-width rows the program terminated, which is the
   common shape in exactly the logs this issue is about.
2. **Ask the emulator** for its pending-wrap (phantom) state at emit time. The
   public surface exposes a cursor position, not that flag, and by emit time the
   wrap in question happened a screenful of rows ago anyway.
3. **Attribute the scroll that ends a row, and carry the record to the row.**
   Every scroll ends exactly one row — the cursor's own — whether the cursor
   moved down onto a blank row or the screen shifted under it. Which of the two
   it was is answerable at the moment the stream is fed, from one cheap fact: did
   the piece being fed contain a newline the program wrote?

## Decision

Option 3, with the width test kept as a validity check.

1. **Cut the stream at each newline and feed the newline as its own piece.** A
   scroll during a piece holding no newline was the terminal wrapping; a scroll
   during the newline itself, or a cursor step down a row during it, was the
   program. Feeding "text\n" as one piece lets a wrap and the following newline
   cancel into one shift that says nothing about either.
2. **Keep one record per row the cursor has left but the screen has not yet
   emitted**, in output order, and consume one per emitted row (`pushEnd` /
   `popEnd`). Pushes and emissions are the same events seen from two ends, so
   they stay aligned without tracking rows individually; the queue holds about
   one screen of records.
3. **Join a row to the next only when both say so**: its record says the terminal
   ended it, and it really does fill the terminal width — the mechanical sign of
   a wrap, and what a record that lost its alignment cannot fake.
4. **Fail toward today's behaviour.** A piece that scrolls without printing was
   moved by an escape sequence, so its rows stay whole; a piece that repositions
   the cursor with an escape does not get to claim rows it jumped over; a run
   whose records exceed `historyEndsCap`, or whose emulator panicked and was
   reset, stops joining for the rest of the run. Missing information means no
   joining, never a wrong one.

## Consequences

- Joined lines make the log read like the program's own output, which is what
  ADR-0007 promised and the row-based store only approximated.
- The sanitizer keeps one bool per row on the screen plus a bounded queue, no
  per-row structures.
- Output that scrolls by escape sequences rather than by printing keeps its
  screen-row splitting. Folding those in would need per-cell damage tracking,
  which the emulator does not expose; a record cannot be invented from a cursor
  jump.
- The `harness logs` text path and its `--json` path read the same durable file,
  so both change together and the regression test at the sanitizer covers both.
- If a maintainer would rather the log stay exactly row-per-line and have joining
  opt-in per harness, the switch is one branch in `emitLocked` plus a config key;
  the records already carry everything the join needs.

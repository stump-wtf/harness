---
status: proposed
date: 2026-10-29
decision-makers: []
extends: [ADR-0007]
related: [ADR-0040]
---

# ADR-0058: Scroll detection believes only the smallest shift its rows can support — the durable log never re-prints a row that is still on screen

> **Not yet accepted.** Proposed in the PR for stump.wtf/harness#1021 by the
> m lane. The bug and its root cause are agreed in the issue; the issue's
> "possible directions" are unexplored options, and nobody has chosen between
> them. The rule below is that choice, for a maintainer to confirm or replace.

## Context and Problem Statement

ADR-0007 makes the durable per-harness log the rows that *scroll off* the
emulator's screen, not the raw PTY bytes: the sanitizer diffs the screen before
and after each chunk and re-emits `prev[:k]`, where `k` is the shift that turns
`prev` into `cur`. A row is therefore logged exactly once — when it leaves the
screen — and a TUI that repaints in place adds nothing.

`scrollCount` measured that shift as **the largest `k` whose compared rows all
match** (#279). A shift of `k` leaves `len(prev) - k` rows to compare, so a
shift near the height of the screen is proven by a single row. Periodic content
— separator rules, a repeated status line, the transcript of a program that
prints the same block over and over — matches *every* shift that keeps its
phase, so the single-row shift wins the contest, and the sanitizer re-emits a
prefix of rows that are sitting there on screen. `harness logs` then shows
segments of the same line up to 23 times, out of order (#1021).

Opening the code showed the same root cause in a second shape, which no rule
about ranking shifts can fix on its own: a screen that repeats itself also
lines up with a shifted copy of *itself*, so a frame that merely printed a new
row at the cursor — nothing moved, nothing scrolled — satisfies the shift test
for `k = 2` on period-two content. The largest-match search read that as two
rows leaving the screen; "prefer the smallest shift" alone reads it as two rows
leaving the screen too.

Both shapes are one problem: the shift test's evidence is treated as proof, and
the search optimises for the biggest claim instead of the best-supported one.

## Decision

**Rank shifts by the evidence behind them, not by their size, and never let a
frame that moved nothing claim a scroll.** `scrollCount` now, in order:

1. **Nothing moved first.** If every row of `prev` is still on screen at its own
   index — the frame appended below the cursor, or changed nothing — the answer
   is 0, before any shift is considered. This is the ordinary frame, and on
   self-similar content it is the one a bare shift test mistakes for a scroll.
2. **The smallest supported shift wins.** Among the shifts that have enough
   evidence (below), return the *smallest* match. This is what turns the
   property around: a screen that really scrolled by `K` matches at `k = K`, so
   the smallest match can never exceed `K`, and `prev[:k]` is always a subset of
   the rows that actually left the screen. Largest-match had the same argument
   pointing the wrong way — it can only promise that everything up to `k` is
   *consistent with* having scrolled.
3. **A shift needs four rows to be believed.** `scrollEvidence = 4` comparable
   rows: a clear-and-repaint whose first rows happen to equal the previous
   screen's last rows must not buy a shift of nearly the whole screen. Four
   rows of 80-column output is content no program repeats by accident. The bar
   is capped by what the screen holds, so a screen carrying only a few rows of
   content still sees its ordinary one-row scroll.
4. **Ambiguous means say nothing.** A frame whose only matching shifts are ones
   its compared rows cannot support is read as a repaint: emit 0, never guess.

## Consequences

- Where the truth is genuinely ambiguous — periodic content shifted by more
  than the rows can distinguish — the log now leaves rows on the screen instead
  of logging them twice. Rows that stay on screen are not lost: the
  end-of-stream `Flush` records whatever is still there. The failure mode
  moves from "the transcript repeats itself out of order", which makes it
  unreadable to a human and wrong for anything reading a run's history, to
  "one line is missing until the run ends".
- A normal one-row scroll, the overwhelmingly common frame, is unchanged:
  nothing-moved fails on the first row, and `k = 1` is the first shift tried.
- An escape-driven multi-row scroll that leaves four or more rows in place is
  still measured exactly, including on periodic content where the new rows are
  distinguishable.
- The evidence floor costs real rows in one band, and this file says so
  plainly: a genuine scroll that leaves fewer than `scrollEvidence` comparable
  rows — a burst of ~77-79 printed lines in one chunk on an 80-row screen —
  is read as a repaint, and the rows the scroll evicted are gone from the
  screen, so `Flush` cannot record them either: the transcript loses a
  screenful that `main` logged, where the old largest-shift match was
  "proven" by two rows. That is the trade, not an oversight: a lost
  near-full-screen burst was judged better than the re-emitted, out-of-order
  rows #1021 produced. `scrollEvidence` is a single constant, and holding the
  unverifiable rows in a pending buffer instead of dropping them (Considered
  Alternatives) is the recorded way out if real runs show the loss matters;
  choosing between them is the maintainer's call. The band is pinned by
  `TestScrollCountEvidenceBandPinsTheTrade` so the reported 0 is not read as
  a detector bug.
- The no-scroll frame now costs one scan over the screen's rows before the
  shift loop runs; it short-circuits on the first row that differs, which is
  the first row for any real scroll.

## Considered Alternatives

- **Largest match with a strict margin over smaller shifts** (the issue's
  second option). Rejected: on self-similar content every phase-preserving
  shift matches equally, so there is no margin to compare — the winner is still
  chosen by size, which is the thing that was wrong.
- **Largest match with only a coverage floor.** Rejected: it kills the
  single-row "proof" and keeps the wrong winner everywhere else, and it still
  claims a scroll for a frame that only appended a row.
- **Smallest match with no floor.** Rejected: it fixes the append case and the
  periodic case but lets a coincidental one- or two-row overlap at the top of a
  repainted screen re-emit almost the whole previous screen.
- **Hold the unconfirmed rows in a pending buffer so nothing is ever lost.**
  Rejected for now: it adds state to the hot PTY reader path to recover rows
  that `Flush` records anyway, and the ordering it must reconstruct is the
  ordering this file exists to get right. Worth revisiting if a real run shows
  a meaningful loss.

---
status: proposed
date: 2026-10-29
decision-makers: []
extends: [ADR-0040]
related: [ADR-0033, ADR-0055]
---

# ADR-0059: Scroll detection may discount the bottom row, and pays for it with the rows above it

## Context and Problem Statement

The durable per-harness log is rebuilt from screen snapshots, not from the byte
stream: `scrollCount` compares the previous snapshot with the current one and
reports how many rows moved off the top, and the differ writes exactly those
rows. The matcher demands a pure vertical shift across the whole trimmed region,
so a frame counts as a scroll only when every surviving row lands where that
shift predicts.

That demand is one row too strict. The bottom row is the row a program is still
writing to, and it can change without anything scrolling at all. A soft-wrapped
write is the clearest case: on an 80-column screen, 86 columns of output leave 80
on the bottom row and wrap the remaining 6 into the next frame, so the snapshot
records a bottom row that is only a prefix of what that row becomes. When the
tail then arrives as its own write, with its own newline, the screen scrolls one
row, the previous bottom row and its counterpart in the shifted frame disagree,
every candidate shift fails, `scrollCount` returns 0, and the row the screen
evicted is never written. Nothing reports an error: the log is simply one line
shorter than the run (#1023).

A blank previous bottom row hides this, which is why plain
one-line-per-newline traffic never trips it. #1020 makes it easier to hit,
because joining a fold leaves a non-blank bottom row whose tail legitimately
arrives later, but any program that redraws the bottom row across a write
boundary can trigger it today.

The same function carries a separate, also pre-existing defect (#1021): on
periodic content the largest matching shift can exceed the true one and re-emit
rows that are still on screen. This record does not settle that; the rule below
is written so it does not make it worse.

## Decision

**The bottom row of the previous snapshot is exempt from the shift comparison,
and the exemption is granted only when at least one row above it lined up.** A
shift of k matches when `cur[i] == prev[i+k]` holds for every `i < len(prev)-k`
other than `i = len(prev)-1-k`, and that exemption is withheld when the exempt
row would be the only comparison.

Two properties fall out of that wording:

- **Every evicted row reaches the log.** A frame that scrolled and redrew the
  bottom row is believed, so its evicted prefix is written instead of dropped.
- **A clear-and-repaint frame still reports 0.** Its rows line up nowhere, so
  waiving one of them changes nothing, and a candidate whose only support is the
  waived row is refused rather than guessed at.

## Consequences

- The log loses no line to a bottom row that changed for a reason other than the
  shift, which is the property `harness logs` depends on: what scrolled off is
  in the file exactly once.
- On a screen holding so few rows that the exempt row would be the only
  evidence, the frame is read as no scroll. Those rows stay on screen and reach
  the log through the end-of-stream flush, so the safe direction is a delay, not
  a duplicate.
- The matcher stays a pure function of two snapshots. Nothing has to observe the
  emulator's own scroll events, and the panic recovery path that resyncs the
  previous snapshot keeps working unchanged.
- The row-level exemption is a real relaxation of the shift test, so a frame that
  did not scroll can now be read as one if its content repeats at the candidate
  period. That is #1021's failure mode, not a new one, and it is bounded by the
  same evidence requirement, one row weaker than before: the waiver removes one
  corroborating row from every candidate shift shorter than the full screen, so
  the smallest shift a single matching row can prove moves from `len(prev)-1`
  to `len(prev)-2`, and on periodic content the over-shift shape #1021
  describes is one row wider than it would otherwise be. That is one more
  reason the evidence-ranked matcher proposed in #1022 should land alongside
  this rule; whatever settles #1021 should compose with this rule rather than
  replace it.

## Considered Alternatives

- **Waive the bottom row only when the current row extends it** (the previous
  bottom row is a prefix of its counterpart). Precise for folds, and rejected:
  it fixes only the one mechanism named in the report, and leaves every other
  bottom-row redraw (a status line, a progress bar) still dropping a line.
- **Waive any single mismatching row inside the region.** More loss-resistant,
  and rejected: one waived row anywhere is one more row of the shift that nobody
  checked, which is exactly the looseness #1021 complains about. The bottom row
  is special for a reason (it is the row under the cursor), and a waiver with no
  structural justification is a guess.
- **Ask the emulator how many rows it scrolled** instead of diffing. Rejected:
  the history writer deliberately diffs snapshots, because a chunk can scroll
  several rows at once and the panic-recovery path has no reliable event stream
  to consult. Wiring scroll events in would be a new coupling between the
  sanitizer and the emulator's internals for one row of accuracy.
- **Never drop: log every row that leaves the snapshot region and de-duplicate
  when reading.** Rejected: the log is append-only and tailed by `harness logs`,
  so de-duplication would move the correctness burden to every reader.

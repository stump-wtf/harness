---
status: proposed
date: 2026-10-31
decision-makers: []
extends: [ADR-0015]
related: [ADR-0008, ADR-0030, ADR-0053]
---

# ADR-0060: A live failure's reason belongs in the detail surface, not the action field

> **Not yet accepted.** Proposed in the PR for stump.wtf/harness#1036 by the
> handoff lane. The issue named two directions and said either was acceptable,
> leaving the choice to whoever picked it up; this records the one taken, and the
> column arithmetic that decided it, so the next reader does not re-litigate it
> from the diff.

## Context and Problem Statement

PR #1035 gave the post-mortem the reason a failed tool call failed:
`internal/logview` prints the masked excerpt beside `(failed)`, and
`harness logs --json` carries it as `error_excerpt`. That PR also wired the
dashboard's watcher to capture those bytes
(`tail.WatchConfig.ErrorExcerptBytes`), so the live path would hold them. It did
not give them a consumer: `chatroom.LastAction`, which feeds the dashboard's live
activity field, returns the compact `"<badge> <tool>"` one-liner by design. A
failing run therefore said only that a tool had run, and explained itself once it
was over.

The two options the issue named were to append the excerpt in `LastAction` the way
`logview.Parts` does, clipped to a width that fits the field, or to render it in
the failure detail surface and keep the field minimal.

## Decision

Keep the live action field exactly as it is. Render the excerpt in the detail
surface the dashboard already has for the selected harness: the peek head.

1. **The field has no room, by construction.** `actionFieldWidth` is 22 columns
   and the field already spends them on `"[BADGE] Tool HH:MM:SS"`, with
   `fitAction` truncating anything longer. An appended excerpt survives as an
   ellipsis standing where the timestamp was, a different ellipsis on every
   event. The column is fixed-width precisely so that the action changes and
   nothing around it moves, and widening it spends the metadata beside it, which
   is the failure ADR-0015's field budget exists to prevent.
2. **The surfaces with room already speak this dialect.** The peek head carries
   contextual notes (it names the guest viewport when the pane crops the
   screen), belongs to exactly one harness, and is what you look at when a row's
   action stops looking healthy. The chatroom stream prints the excerpt on the
   failed line itself, and a one-shot's preview *is* `harness logs`, which prints
   it too. Where those already say it, the head says nothing: the bytes appear
   once per failure, never twice.
3. **The note follows the newest action, not the alarming one.** The head states
   a reason only while the newest action observed for that harness is the failed
   call, so a harness that recovers stops being accused, and a replayed older
   failure cannot resurrect a note over a newer success.
4. **Presentation only.** Redaction stays upstream where agent-trace masks before
   cutting (ADR-0008). Nothing is captured that #1035 did not already capture,
   nothing is re-masked on the way to the screen, and `harness logs --json` is
   unchanged.

## Consequences

- A failing run explains itself while it runs. The live field and the
  post-mortem now agree about the same failure because both read the same field
  off the same event, clipped the same way (`logview.Clip`/`logview.Inert` at
  `logview.ExcerptWidth`), which is the parity #1035's wiring was staged for.
- The live action field still does not carry a reason, and at 22 columns it
  cannot. Making the row itself distinguish a failed newest action (a marker, a
  colour, a wider field) is a layout decision this ADR does not make; it needs a
  maintainer's call and a pass over the list pane's width tests.
- The reason is shown for the harness you are looking at, not for every row at
  once. That is deliberate: a dashboard of rows each trailing their own failure
  would re-elide the per-harness facts that appear nowhere else on the screen.
- The peek head's note is one more head note, so a very wide excerpt competes
  with the crop note for the pane's columns; the head is truncated from the
  right, and the failure leads.

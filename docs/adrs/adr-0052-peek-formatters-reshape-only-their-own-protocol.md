---
status: proposed
date: 2026-10-25
decision-makers: []
extends: [ADR-0011, ADR-0021]
related: [ADR-0003, ADR-0004]
---

# ADR-0052: Preview formatters reshape only their own protocol — readable output never rewrites a guest's terminal bytes

> **Not yet accepted.** Proposed in the PR for stump.wtf/harness#951 by the
> m lane. The bug and its root cause are agreed in the issue; nobody has
> decided the rule below, or the open question of whether a resident session
> should have a formatter at all.

## Context and Problem Statement

`claude -p --output-format stream-json` prints one JSON object per line, and the
dashboard preview and attach view run those bytes through the claude-code
adapter's `PeekFormatter` (#13) so a human sees tool calls instead of
bookkeeping JSON. ADR-0021 keeps that interpretation inside the adapter: no
generic layer parses a backend's payload.

The same formatter is installed for every `claude-code` attach, including a
**resident** session (`harness = "claude-code"` with a prompt-less, interactive
`claude`), whose output is not JSON at all. It is a full-screen Ink app
repainting with relative cursor moves that count rows — blank rows included —
and ending a frame with a cursor-parking tail that has no newline after it. The
formatter was line-oriented and line-buffered: it dropped blank lines and held
every byte until the next newline. The client's emulator therefore received a
frame shorter and later than the app believes it drew, `\x1b[5A` overshot the
top of the repaint, and the redraw overwrote rows above it — the attached view
of Claude Code's folder-trust dialog corrupted after the first keypress while
`harness capture`, fed the same bytes straight, stayed correct (#951).

## Decision

A peek formatter may buffer and rewrite only the bytes it can name as its own
protocol events, and it may decide a line is one only from something the format
guarantees (for stream-json: the line's first byte is `{`). Everything else is
the guest's terminal output and is forwarded unchanged, in the chunk it arrived
in. When the guess fails — an ESC appears before the newline, which raw JSON
cannot contain, or the line runs past the line cap — the bytes already buffered
**and the rest of that line** go out exactly as they came.

Put plainly: a formatter may hide noise it recognizes. It may never move,
shorten, reorder or delay output it does not recognize. Byte-faithfulness is the
default; interpretation is the exception, bounded to bytes the adapter can name.

## Consequences

- The attached view of an interactive session shows the same screen the daemon's
  emulator holds, because for non-event bytes the formatter is now a pass-through.
- Formatters stay installed for resident sessions, where they do nothing. They
  are harmless now; whether they should be there at all is the open question.
- One behavior change for one-shots: a blank line in stream-json output is
  forwarded instead of dropped. `claude -p` does not emit one, so no run is
  expected to notice.
- Any future formatter (a codex or crush readable preview, if one is added)
  inherits this rule: pass through anything it cannot name.

## Considered options

- **Don't install a formatter for resident sessions.** Cleanest, but
  `protocol.HarnessInfo` carries `Adapter`, `Args` and `Argv` and nothing that
  says "one-shot", so it needs a new wire field. Deferred to the open question.
- **Teach the formatter Ink's frame shape.** Rejected: it would make the preview
  layer an emulator of one vendor's TUI, and it would still be guessing.

## Open questions for the maintainer

- Add a "one-shot" field to `protocol.HarnessInfo` so an attach to a resident
  session opens byte-faithful without a toggle, or is the hardened formatter
  enough and the extra wire surface not worth its cost?

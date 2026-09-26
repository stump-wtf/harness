---
status: accepted
date: 2026-09-26
decision-makers: [joestump]
extends: [ADR-0003, ADR-0007]
related: [ADR-0002, ADR-0008]
---

# ADR-0040: The emulator screen is the observability source a sweep can read

## Context and Problem Statement

An unattended harness has no one watching its glass. Two gaps in that mode,
filed together as issue #735:

1. **A harness frozen at an interactive prompt reports `state: running`.** A
   permission dialog, a workspace-trust screen, an MCP consent wall — the
   process is alive, the CPU is quiet, and the lifecycle state machine (which
   describes the PROCESS) is correctly green. Nothing distinguishes "busy and
   healthy" from "frozen forever at a prompt nobody will answer".
2. **`harness logs` cannot see a full-screen prompt.** That is not a bug in
   the log; it is the design working. ADR-0007 (as amended for #279) stores
   sanitized history: the rows that SCROLL off the emulator screen, one per
   line, no raw repaint bytes. A full-screen prompt repaints in place and
   never scrolls, so it never lands in the log. A monitoring sweep that greps
   `harness logs` for prompt-shaped text confidently reports "nothing is
   stuck" while a harness plainly is — using data it had no way to know was
   incomplete. And the only way to see the current screen was `attach`,
   which needs an interactive TTY and cannot be scripted.

The tempting fix for (2) — stuff raw redraws into the durable log — would undo
the #279 amendment and bury the log under per-frame junk for every full-screen
TUI, the exact problem it was amended to stop.

## Decision

The daemon already owns, per harness, an x/vt emulator fed every byte the guest
writes (ADR-0003). That emulator's visible screen is the truthful, complete
record of what is on the glass, so observability reads **it**, not the log:

1. **`harness capture <name>`** renders the harness's visible screen without a
   TTY: plain text by default (one row per line, trailing blank rows trimmed —
   the grep-friendly view), the attach snapshot's self-contained ANSI repaint
   under `--ansi`, and the full payload under `--json`. The protocol gains the
   `capture` op (ProtoMinor 16); a harness whose daemon has never teed output
   answers `no_screen` rather than an empty screen, because "nothing drawn yet"
   and "an empty screen" are different facts a sweep must not conflate.
2. **The waiting projection.** `list` and `describe` carry `waiting` on a
   harness whose visible screen has gone unchanged for a threshold (30s
   default, daemon option for tests) WHILE a curated pattern of interactive
   prompt shapes is visible on it — y/n confirmations, permission dialogs,
   workspace-trust screens, "press enter" walls, consent prompts. The
   conjunction is the whole point: idle alone is a healthy harness between
   turns; a prompt alone is ordinary prose. Neither half is a verdict.
   The lifecycle state stays untouched — `waiting` is a projection over the
   glass, rendered alongside the state glyph, not an eighth state in the
   SPEC-0003 machine.

## Consequences

* Detection stays pattern-based on TTY CONTENT, not backend semantics: the
  daemon never learns what kind of program is on the other end, and `capture`
  never sends input (read-only attach discipline, ADR-0008). A false "waiting"
  costs an operator's glance; it can never cost a keystroke.
* The screen-age clock is PTY BYTES received, not screen text compared: a busy
  TUI repainting identical cells every second is activity, so a healthy
  harness cannot read as waiting.
* The emulator survives daemon restarts of a supervised process but not of the
  daemon itself; `capture` answers from the live daemon, and a dead daemon has
  nothing to ask — the durable log remains the historical record.
* The pattern table is curated, in code, and will trail new prompt shapes.
  That is accepted: the failure mode is a missed detection (the status quo
  today), never a false keystroke.

## Governing

* issue #735 (detection + capture), SPEC-0002 REQ "Control Operations" (the
  capture op mirrors the CLI verb), ADR-0003 (the daemon owns the emulator),
  ADR-0007 (the log stays sanitized history — unchanged by this decision),
  ADR-0008 (no input path from capture).

# ADR-0014: Reload Autostarts Newly-Introduced Harnesses

## Status

Accepted

## Context

When a harness with `enabled = true` is added to `harness.toml` while the
daemon is running and `harness reload` is issued, the harness appeared in
`harness list` with `ENABLED=no` and did not start. The config field
`enabled` (boot-time autostart) and the runtime intent field `Enabled`
(whether the user wants this harness running) are two different concepts
that shared one name and one column. For harnesses present since boot they
usually agreed; for harnesses introduced by hot reload they diverged.

This area was deliberately shaped by `265e42a` (never treat daemon shutdown
as intent change) and `2dfa8fc` (report autostart members left down by
persisted intent). Those commits established that an explicit `harness stop`
must never be overridden by a subsequent reload or restart. The question
was whether a *newly-introduced* harness — one with no persisted intent at
all — should inherit its config autostart membership as initial intent.

## Decision

**Option A: reload honours config autostart for newly-introduced harnesses.**

A harness that is new to the daemon (not present in the previous config)
and has config autostart membership (`enabled = true` or member of an
autostart profile) gets runtime intent set to `true` and is started during
reload, exactly as if the daemon had booted with it present.

Pre-existing harnesses keep their persisted intent unchanged. An explicit
`harness stop` is never undone by a subsequent reload.

This matches systemd semantics: `systemctl enable` + `daemon-reload` makes
a new unit eligible for autostart without disturbing already-stopped units.

## Consequences

- `harness reload` now starts newly-added `enabled = true` harnesses.
- The `ENABLED` column in `harness list` accurately reflects runtime intent
  for all harnesses, including newly-introduced ones.
- Removing `enabled = true` from a running harness's config and reloading
  does **not** stop it (runtime intent is independent once set).
- Project-scoped harnesses and autostart profiles behave consistently: any
  harness newly introduced by a reload that appears in the autostart set
  (via `enabled` or profile membership) is started.
- The invariant from `265e42a` / `2dfa8fc` is preserved: pre-existing
  harnesses' intent is never modified by reload.

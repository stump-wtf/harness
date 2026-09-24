---
status: accepted
date: 2026-08-12
decision-makers: [joestump]
extends: [ADR-0006]
related: [ADR-0005]
---

# ADR-0014: Reload autostarts newly-introduced harnesses

## Context and Problem Statement

When a harness with `enabled = true` is added to `harness.toml` while the daemon
is running and `harness reload` is issued, the question is whether it starts.
The config field `enabled` (boot-time autostart) and the runtime intent field
`Enabled` (whether the operator wants this harness running) are two different
concepts that share one name and one column in `harness list`. For harnesses
present since boot they usually agree; for harnesses introduced by hot reload
(ADR-0006) they can diverge: without a rule, a new `enabled = true` harness shows
`ENABLED=no` and stays down.

Two commits shape this area: `265e42a` (never treat daemon shutdown as intent
change) and `2dfa8fc` (report autostart members left down by persisted intent).
They establish that an explicit `harness stop` must never be overridden by a
subsequent reload or restart. The open question is whether a
*newly-introduced* harness — one with no persisted intent at all — should
inherit its config autostart membership as initial intent.

## Decision Drivers

* An explicit `harness stop` must never be undone by a reload or restart.
* Adding an autostart harness and reloading should behave the same as booting
  the daemon with that harness already present.
* The `ENABLED` column in `harness list` should reflect runtime intent for every
  harness.
* Match the systemd mental model operators already have (`enable` +
  `daemon-reload`).

## Considered Options

* Option 1 — Reload honours config autostart for newly-introduced harnesses
* Option 2 — Reload never sets intent; a newly-introduced harness stays stopped
  until an explicit `harness start`

## Decision Outcome

Chosen option: **Option 1 — Reload honours config autostart for
newly-introduced harnesses**, because it makes a reload equivalent to a boot for
harnesses the daemon has never seen, without touching the intent of any harness
it already knows.

A harness that is new to the daemon (not present in the previous config) and has
config autostart membership (`enabled = true` or member of an autostart profile)
gets runtime intent set to `true` and is started during reload, exactly as if the
daemon had booted with it present.

Pre-existing harnesses keep their persisted intent unchanged. An explicit
`harness stop` is never undone by a subsequent reload.

This matches systemd semantics: `systemctl enable` + `daemon-reload` makes a new
unit eligible for autostart without disturbing already-stopped units.

**Config re-introduction wins over stale persisted intent.** A harness that was
removed from the config, then re-added by a later reload, is treated as
newly-introduced — its autostart membership determines whether it starts, even
if `state.json` records a prior `Enabled=false` from an explicit stop before the
removal. Removing a harness from the config is itself a strong signal;
re-adding it with `enabled = true` is the operator saying "I want this running
again." This matches systemd: re-adding a unit file and reloading starts it per
the unit's `[Install]` section, regardless of prior state.

### Consequences

* Good, because `harness reload` starts newly-added `enabled = true` harnesses.
* Good, because the `ENABLED` column in `harness list` accurately reflects runtime
  intent for all harnesses, including newly-introduced ones.
* Good, because project-scoped harnesses and autostart profiles behave
  consistently: any harness newly introduced by a reload that appears in the
  autostart set (via `enabled` or profile membership) is started.
* Good, because the invariant from `265e42a` / `2dfa8fc` is preserved:
  pre-existing harnesses' intent is never modified by reload.
* Neutral, because removing `enabled = true` from a running harness's config and
  reloading does **not** stop it (runtime intent is independent once set).

### Confirmation

* Adding an `enabled = true` harness to `harness.toml` and running
  `harness reload` starts it, and `harness list` shows `ENABLED=yes`.
* A harness stopped with `harness stop` stays stopped across `harness reload` and
  a daemon restart.
* Removing a stopped harness from the config, reloading, re-adding it with
  `enabled = true`, and reloading again starts it.

## Pros and Cons of the Options

### Option 1 — Reload honours config autostart for new harnesses

* Good, because reload and boot agree for a harness the daemon has not seen.
* Good, because it mirrors `systemctl enable` + `daemon-reload`.
* Bad, because config autostart and runtime intent now interact at one more
  point, which the re-introduction rule has to pin down.

### Option 2 — Reload never sets intent

* Good, because reload stays a pure definition refresh.
* Bad, because a new `enabled = true` harness shows `ENABLED=no` and stays down
  until someone notices and starts it by hand.
* Bad, because the result depends on whether the harness arrived at boot or by
  reload.

## More Information

* **Extends ADR-0006** — hot reload of `harness.toml`.
* **Related ADR-0005** — autostart and enabled-state are daemon state.

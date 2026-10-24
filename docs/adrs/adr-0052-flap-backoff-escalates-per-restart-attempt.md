---
status: proposed
date: 2026-10-24
decision-makers: []
related: [ADR-0005]
---

# ADR-0052: Flap backoff escalates per restart attempt once flapping, not per crash cluster — so BackoffCap stays reachable

> **Not yet accepted.** Proposed in the PR for stump.wtf/harness#335 by the
> m lane. SPEC-0003 already requires that "subsequent restart delays escalate
> exponentially up to the cap", but the thread records no decision on what
> the escalation counter should key on once the delay outgrows the crash
> window; #335 lists the candidates and this ADR picks one.

## Context and problem statement

Crash-loop detection (SPEC-0003 REQ "Crash-Loop Detection", ADR-0005) trips
`flapping` when N exits land inside window T, and the restart delay is
`BackoffBase * 2^(attempt-1)` capped at `BackoffCap`. `flapAttempts` was
incremented only when a crash cluster tripped — N exits inside T.

The curve is then self-limiting: the delay grows 1s → 2s → 4s → 8s, and once
it passes T/2, N exits can no longer fit inside T. The cluster stops
tripping, the attempt counter freezes, and the delay plateaus — at 8s with
the production policy, 4× the retry rate the 30s cap was designed to settle
at, indefinitely. Observed in #315's repro: `flapping restarts=16
next_retry_in=8s` logged forever, for a harness that intermittently
recovers (a run past `HealthyRun` clears the give-up counter, so #315's
give-up never parks it).

## Decision drivers

- `BackoffCap` must be reachable by construction, or be documented away —
  SPEC-0003 says the delays escalate to the cap, so a permanently frozen
  counter is a bug, not a documented plateau.
- Recovery semantics stay as specced: a run surviving longer than T resets
  the crash counter and returns the harness to `running`.
- Keep the give-up wiring of #315 (`consecFailures` / `MaxRestarts`)
  untouched: it keys on a separate clock on purpose.

## Considered options

1. **Escalate per consecutive failure (`consecFailures`).** Simple and
   monotonic, but couples the backoff clock to the give-up clock the #315
   fix deliberately separated, and would keep the delay high after a run
   that survived T but not `HealthyRun` — contradicting the spec's recovery
   reset.
2. **Keep cluster semantics, widen the window as the delay grows.** Every
   window change redefines "crash loop" for detection too; two behaviors
   would share one knob.
3. **Once flapping has tripped, escalate on every restart attempt.** ✅
   Detection stays exactly as specced (a cluster trips flapping; a run
   surviving T clears everything, including this counter). Between those two
   events the counter is a per-restart attempt count, so the curve is
   monotonic and reaches the cap across a real run of crashes.

## Decision outcome

Option 3. `flapping` is still set only by an in-window crash cluster; while
it holds, each restart attempt increments `flapAttempts`, and the
`CrashWindow`-survival reset (and manual start / policy exit / hold /
parking, which reset crash state) returns escalation to the base. Give-up
remains keyed on `consecFailures` (SPEC-0003 REQ "Backoff Give-Up").

## Consequences

- A flapping harness's delay walks 1s → 2s → 4s → 8s → 16s → 30s and holds
  at the cap until it recovers, instead of freezing at 8s. Every flapping
  harness on the fleet sees the steeper tail; that is the point of the cap.
- A run surviving T now genuinely resets escalation to the base again; under
  option 1 it would have stayed escalated until `HealthyRun`.
- Verified by a supervisor-level test (`escalation_test.go`) that drives
  crash exits spaced beyond the window and asserts the observed
  `next_retry_in` reaches `BackoffCap`; it fails on the frozen-counter code
  (plateaus at 160ms on a 200ms cap) and passes with the fix.

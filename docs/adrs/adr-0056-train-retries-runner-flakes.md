---
status: proposed
date: 2026-10-29
decision-makers: []
extends: [ADR-0032]
related: [ADR-0008]
governs: [SPEC-0025]
---

# ADR-0056: A red train whose jobs never reached a runner costs one cool-down retry, not a stall

## Context and Problem Statement

ADR-0032's train treats a red attempt as a verdict: the `(pr, head, base)`
triple is remembered (SPEC-0025 REQ-10) and the train does not touch it again
until `main` or the PR head moves. That is right when the tree is genuinely
bad, and very wrong when it is not. On 2026-10-29 two train attempts for one PR
went red on two different trees, each on a different load-sensitive test that
passed on the same tree locally under `-race`, and the `build` job on both runs
reported that no runner ever started it. Separately, jobs across three runs died
with no log at all, and one `make check` died on a Go module proxy stream error
and then re-ran green, unchanged. Each of those cost the PR hours of waiting and
a human nudge, and every roll on a moved base could land on another flake.

The forge cannot distinguish these from the signal the train already reads.
`CombinedStatus` says `failure` either way. What does distinguish them is the
Actions job log: a job whose runner never attached answers `job not started`
instead of serving a log. That is a fact about the fleet, not the tree.

The other half of the report's evidence, load-sensitive tests that fail only on
a loaded host, is deliberately not solved here. Those jobs *do* have logs and
*do* contain a test failure. Retrying on a list of "known flaky tests" needs a
registry nothing maintains, and a stale list silently converts real failures
into re-runs. Those tests need budget and scaling work in their own right
(#278 and the `testwait` conventions), not a train that guesses.

## Decision

**Classify a red train attempt by the forge's per-job detail, and retry it at
most once after a cool-down when nothing in it blames the tree.**

1. **The signal is "no log at all", per failed job.** `forge.JobReporter` is an
   optional capability: `CommitJobs(ctx, repo, sha)` returns each job with its
   conclusion and a `NeverStarted` flag, which the Gitea adapter fills by
   asking that job's log endpoint. Optional, not part of `forge.Forge`, so no
   other implementation changes and a forge that cannot answer is read as
   reporting a real failure.
2. **All or nothing.** The rule fires only when *every* failed job on the train
   commit carries the signature. One job with a log and the failure is the
   tree's. A skipped job is not a failure, and a `cancelled` or timed-out job
   says nothing about runners, so neither can fire it. An unreadable log and an
   empty job list are real failures.
3. **Exactly one retry per `(pr, head, base)`.** The driver keeps the retry in
   the same map as the attempt memory, so the worst a misclassification can cost
   is one extra CI run per tree. There is no loop: the second failure with the
   same signature gets today's `red` comment, which says the tree was re-run
   first.
4. **The cool-down is a timestamp, not a sleep.** A tick never blocks on a
   flake; the next eligible PR uses the intervening ticks, and the waiting tree
   becomes attemptable when its `flake_cool_down` (default 5 minutes) has
   passed. `0` restores the pre-#1016 behaviour.
5. **No comment on the retried attempt.** REQ-9's one comment per head still
   fires, on the retry's result. An author is never told their PR failed and
   then, ten minutes later, silently merged.
6. **The driver stays out of host and forge administration.** It reads run, job
   and log state. It never re-runs a workflow, cancels a run, or touches a
   runner or an Actions setting; a fleet fault stays visible as one, and
   #1010/#1015 own fixing it.

## Consequences

- A runner flake costs one CI run and a cool-down instead of hours of a stalled
  PR and a human touching it; a genuinely broken tree costs what it costs today.
- Two new log events, `flake retry` and `flake retry exhausted`, plus the rule
  name in each, so "was this retried, and why" is answerable from the daemon log
  without rerunning anything.
- A few extra API calls, only on a red attempt: one run listing, one job listing
  per run, one log probe per failed job.
- The retry is in-memory like the rest of the attempt memory, so a daemon
  restart forgets that a tree was already retried. Accepted: the cap exists to
  stop a loop within one run, and the alternative is persistent state for a
  transient fact.
- Load-sensitive tests are *not* excused by this. A test that fails with a log
  is a failure, and #278 remains the place that gets fixed.

## Considered Alternatives

- **Retry any red once.** Rejected: it hides real regressions behind a green
  second roll, and the train's whole claim is that only tested trees land.
- **A known-flaky test registry the train consults.** Rejected for now: nothing
  maintains one, a stale entry turns a real failure into a re-run, and the
  signature it would match (a failing test in a job that ran) is indistinguishable
  from a genuine intermittent failure in the tree.
- **Re-run the workflow on the forge instead** (`actions/runs/{id}/rerun`).
  Rejected: that is forge administration from inside a driver, it leaves the
  train's memory describing a commit CI did not just test, and it can loop.
- **Wait for a runner-capacity fix and change nothing here.** Rejected: #1010
  and #1015 are host work with no ETA, and the driver's stop-and-wait costs
  hours even on a healthy pool that had one busy minute.
- **A longer cool-down, or one that grows per attempt.** Rejected: the cool-down
  exists to let a scheduling backlog drain, minutes is that scale, and a growing
  wait is a retry policy with more ways to loop.

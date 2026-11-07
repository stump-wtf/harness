---
status: proposed
date: 2026-11-06
decision-makers: []
extends: [ADR-0027]
related: [ADR-0005, ADR-0019, ADR-0027]
governs: [SPEC-0021]
---

# ADR-0062: Taking a harness out of service is a hold reason, not a new state

> **Not yet accepted.** Proposed in the PR for stump.wtf/harness#828 by the
> handoff lane. The issue decides what an operator gets (`drain`, `undrain`,
> `--hold`/`--discard`, a `--wait` that can gate a restart) and points at
> ADR-0027's hold-reason set and the admission funnel as the place it should
> land. It leaves four things unstated, and each has a consequence beyond the
> verb: whether the gate may touch a running process, whether
> `draining`/`drained` are lifecycle states, whether the gate survives a daemon
> restart, and what happens to a firing that was already queued. This records
> those four.

## Context and Problem Statement

`harness stop` does two things an upgrade does not want: it sets
`enabled = false`, which firings never check, and it SIGTERMs the run in
flight (exit 143). The reporter scripted a stop / watch / hope loop to upgrade
a daemon without killing an agent mid-run.

There are eight places a run can start from: cron, a channel event, a webhook
delivery, `harness trigger`, a `catch_up` pass, a self-triggered queue, an
after-hours lease, and a release of a hold. A gate has to cover all eight, and
it has to be visible enough that a packaging script can block on it. Sized L
because it touches the supervisor, the protocol, the CLI and `describe`.

## Decision

**1. Drain is an operator hold reason, checked first in admission.** ADR-0027
made "held" a set of reasons, and the funnel (stump.wtf/harness#470) already
sits in front of every start. Adding `drain` as a reason there gates all eight
sources with one check, and `undrain` is the ordinary release of that reason.
A per-source check would be eight checks to keep in step, and the funnel
already records the refusal as a skipped run with a reason.

**2. Drain gates starting, not being up.** Every other reason in the set keeps
a harness *down*: arming one stops the process. Drain must not, because the
issue exists precisely to stop that. So `harness drain` is its own loop command
rather than a call into `Manager.Hold`: it adds the reason, publishes, and
leaves the process alone. `describe` reads `draining` while that run finishes
and `drained` once it does not.

**3. `draining` and `drained` are surface labels, not SPEC-0003 states.** They
ride alongside `hold_reasons: ["drain"]` in one new `drain` field. A lifecycle
state would need every consumer, table and test that switches on `state` to
learn a new value, for something that is not a supervisor state at all. An
older client that ignores the new field still sees the harness held for
`"drain"`.

**4. The gate is runtime state, never persisted.** A daemon restart resumes
every harness. The chain the feature exists for is
`harness drain --all && harness drain --wait && restart`: the wait is the gate,
and the restart is the resume. A gate that survived the restart would leave the
fleet out of service until someone typed `undrain`, which is the opposite of
the request.

**5. A queued firing is held, not refused.** With `on_overlap = "queue"`, the
run in flight has already earned one more run. Running it through admission
under the gate would record a skip and throw the request away, so
`finishRunWith` keeps it queued while the reason is in the set and `undrain`
starts it like any other firing. `drain --discard` drops it once, recorded
`cancelled` with `reason = draining`, for an operator who wants the queue gone
rather than deferred.

**6. `drain --wait` answers a structured `drain_timeout`.** A deadline that
passes while a drained harness still runs is not a failure of the drain: the
gate is armed and nothing new starts. A structured code gives the CLI a
non-zero exit for the script's `&&` and a message that says what to do next.

## Consequences

The verb costs one hold reason, two control ops, three `ControlReq` fields, one
wire field, and no new state values, so nothing that reads `state` has to
change. `ProtoMinor` goes to 32; an older daemon answers the two ops with
`unknown_op`, which the CLI reports rather than pretending the harness is out of
service.

Two things stay deliberately out. `stop` honouring `enabled` is stump.wtf/harness#786,
already closed, and this does not change what `stop` does to a running process.
And the gate is per-harness, not per-daemon: `drain --all` is the CLI applying
the verb to every harness the daemon lists, so an operator who adds a harness
mid-drain is not silently drained, and `--wait` waits on what is actually
draining.

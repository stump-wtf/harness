---
status: proposed
date: 2026-10-20
decision-makers: [joestump]
extends: [ADR-0020]
related: [ADR-0040, ADR-0045, ADR-0013]
---

# ADR-0048: Put progress on the control socket — `last_successful_call_at` and supervised `children` on list/describe

> **Proposed.** This decision arrives as a design record alongside its
> implementation, because no maintainer decision on it exists yet: where the
> timestamp comes from, how children are read without putting a shell-out on a
> request path, and what the JSON shape is — those are what need reviewing.
> Nothing here is settled until this file reads `status: accepted`.

## Context and Problem Statement

Issue #958: an operator polling the control socket cannot see that an agent is
wedged. `harness_last_successful_call_timestamp` already exists on `/metrics`
(ADR-0020), and ADR-0045 built a watchdog on that same signal, but the JSON
snapshot answers with `state: running` and a screen `idle_ms`. A persistent
crush harness blocked about 23 hours on a hung child — a recursive `grep`
stalled on I/O — while its own process burned steady CPU and `list` reported it
healthy. Anyone without a Prometheus scrape had to run `ps` by hand to find the
child.

The gap is not that the daemon lacks the signal. It has three of them: the
observer's tool-call feed (what the gauge and the watchdog read), the screen's
silence (SPEC-0002's `idle_ms`, ADR-0040), and the process table. The gap is
that only one of them reaches the surfaces most operators use, and none of them
names what the process is waiting on.

## Decision Drivers

* One value, one meaning. `state` describes the process. The new field must
  describe progress, and it must be the *same* progress `/metrics` exports, or
  two surfaces disagree about one harness and both are believed sometimes.
* Nothing on the request path that did not need to be there. `list` is what the
  TUI refresh loop calls, several times a second at worst; a shell-out or a
  `/proc` walk inside a projection is a cost every client pays for a field most
  never read (the same reason ADR-0045 keeps process reads off the supervisor).
* Unknown must not look like zero. ADR-0020 omits the gauge until the first
  success precisely because `0` renders as 1970 and puts 56 years of staleness
  on every panel; an empty `children` list must not read as "this agent has no
  children" when it means "the daemon could not look".
* Nothing new leaves the machine, and nothing new is read. A child's arguments
  can carry whatever the agent passed to it, including credentials; the daemon
  must not put them on a socket everyone on the host can connect to.

## Considered Options

### 1. Where the timestamp comes from

* **Option 1 — The metrics registry.** `internal/metrics` keeps
  `series.lastSuccess`; give it an accessor and have the control plane call it.
  Rejected: the registry only exists when `metrics_listen` is on, so an operator
  who never enabled `/metrics` — exactly the audience for the control socket —
  would get a field that is silently always absent, and the control plane would
  depend on the scrape path.
* **Option 2 — The stall watchdog's progress clock.** ADR-0045's guard already
  derives, per harness, the latest `KindTool` event — the same rule, from the
  same feed, that produces the gauge — and it runs whenever the daemon does.
  Chosen: one `LastCall(name)` accessor and the field appears, with no second
  derivation of "progress" anywhere in the tree.
* **Option 3 — Derive it again in the daemon package.** Rejected: a third copy
  of the rule is how two surfaces end up disagreeing, which is driver one.

### 2. How children are read

* **Option 1 — `ps` per `list`/`describe`.** Rejected: that is a shell-out on
  the refresh path, and ADR-0045 says out loud it does not belong there.
* **Option 2 — The server samples the process table on a timer; the projection
  reads the last sample.** Chosen. One `ps` every 15 seconds, whatever the
  client count, and the projection cost is a mutex and a slice.
* **Option 3 — Walk `/proc` or use a libproc binding.** Rejected: the tree reads
  the process table through `ps(1)` already (ADR-0045), which works on Linux,
  macOS and the BSDs with no privileged access; a second mechanism would be one
  more thing to get right per platform for no additional truth.

### 3. What a child is on the wire

* **Option 1 — Executable name only.** Chosen: `pid`, `command`, `started_at`,
  `cpu_ms`. `comm` is what names a hung `grep`; argv is what carries a token.
* **Option 2 — Full argv.** Rejected on the last driver above; a wider field
  that leaks is not worth the one extra clue it adds, and the kernel truncates
  `comm` to 15 characters anyway, which is enough to spot the stuck tool.

## Decision Outcome

`HarnessInfo` gains two additive fields (ProtoMinor 28), and `describe` prints
them as rows.

### Progress is the same instant the gauge carries

`last_successful_call_at` (RFC 3339) is `stallguard.Guard.LastCall(name)` — the
latest observed tool call — formatted by the daemon's control plane. It is
reported whether or not the harness is running and whether or not it configured
a `stall_timeout`: it is the harness's history in this daemon's lifetime, not
its process's. Omitted when nothing has been observed, which is a finding in
itself for a harness `list` calls `running`, and is phrased as
`none since the daemon started` rather than a date.

### Children come from a sample, not from the request

The server owns a `procSampler` reading the whole process table on a 15-second
interval (`ps -e -o pid=,ppid=,etime=,time=,comm=`) and the projection reads
what the last pass found. Consequences accepted with the choice:

* A child that starts and exits inside one interval is never named. This answers
  "what has been sitting there for hours", which is the question #958 asks; the
  alternative is a `ps` for every `list` the TUI draws.
* The sample is up to one interval stale, so a freshly spawned child may not
  appear on the first `describe` after it starts.
* `started_at` comes from the table's elapsed column, so it is accurate to the
  second and derived as `now − elapsed` — `lstart` was not used because its
  format is the least portable thing `ps` prints.
* `cpu_ms` is cumulative user+system time. Age plus near-zero CPU is the whole
  wedged-child reading; the daemon computes no delta, because two samples in a
  read-only snapshot would be a second clock to keep.

Only a harness whose state is `running` gets children: the snapshot's pid is the
last one the harness had, and a stopped harness's pid may already belong to
another process, whose tree would then be reported as the harness's.

### Unknown is absent, never empty

`last_successful_call_at` is omitted until the first observed call, and
`children` is omitted when the harness is not running or the sampler has never
succeeded (a container with no `procps`). Neither is ever an empty list or a
zero. Where `ps` is missing the sampler logs one debug line and stops trying to
be interesting; the fields simply stay off.

### Listing children is capped at the eldest 24

`proctree.ChildLimit` is 24, ordered eldest first. A worker that fanned out a
hundred children still names the one that hung first, which is the one the
operator is asking about.

### The CLI prints rows, not new columns

`describe` gains `last call` (relative age and the absolute time) and one `child`
row per child (`pid … · command · up … · cpu …`). `list`'s table is unchanged:
the state column already carries the glyphs it carries, and a `list` is a
summary — `describe` and `--json` are where the detail lives.

## Consequences

**Positive**

* A wedge is visible from the one surface an operator already polls, with no
  Prometheus required.
* The `ps` a human would have run is now something the daemon runs once per
  interval for every client at once.
* Progress has one definition in the tree, so the gauge, the watchdog and the
  socket cannot drift into three meanings of "is it working".

**Negative**

* One more goroutine per daemon, and a `ps` every 15 seconds whether or not
  anyone reads it. Deliberate, and cheap next to the observer's poll loop.
* The child list is up to one interval stale, and short-lived children are
  invisible. An operator who needs the exact instant a child appeared is reading
  the harness's own log, not this field.
* `comm` is truncated by the kernel on Linux, so two same-named children are
  distinguished only by pid and age.

## More Information

* Issue #958 is the report; ADR-0045 is the write side of the same signal and
  explicitly left this read side to another record.
* `harness_session_active` (ADR-0020) answers a different question — whether the
  agent wrote to a session inside the idle window — and is not surfaced here,
  because the report is about a call that never returns, which is exactly when a
  session can look live.
* A follow-up worth having: expose the watchdog's stall verdict and
  consecutive-stall count on the same surface, so "the guard found this stalled
  twice and gave up" reads without the log. ADR-0045 lists it as a follow-up
  too.
* Making the interval a config key was considered and refused: nothing in the
  tree makes a sampling period operator-tunable, and a knob that can be set to
  an hour is a way to switch the field off without saying so.

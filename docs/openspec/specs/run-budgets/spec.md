---
status: draft
date: 2026-09-22
implements: [ADR-0027]
requires: [SPEC-0003, SPEC-0008, SPEC-0012, SPEC-0013, SPEC-0014]
---

# SPEC-0021: Run Budgets and Usage-Limit Backoff

## Overview

A harness may carry **budgets**: how many runs it may start per day, how many
triggered runs may be in flight across the daemon, and how many tokens and
dollars one run, one harness per day, and the whole daemon per day may spend.
Every process start passes one **admission** check first; a run in flight is
stopped when it crosses a per-run or daily cap. When a harness's provider
refuses it for an exhausted quota, the daemon **parks** the harness until the
quota resets instead of restarting it into `failed`. See ADR-0027.

Counters are read from the run ledger (SPEC-0022), so they survive a restart and
agree with `harness runs`. Token and cost figures come from the ledger's usage
accumulator (SPEC-0022 REQ "Usage Accumulation"), which reads agent-trace usage
through the daemon's observer. Error classes come from the classifier SPEC-0013
REQ-3 defines.

This spec amends, by reference and without editing them:

* **SPEC-0012 REQ "Gate Enforcement"**: `held` becomes a set of hold reasons
  (REQ-14). SPEC-0012 is not edited while #412 is open; a follow-up folds this
  in once it merges.
* **SPEC-0014 REQ "Run Record Fields"**: skip `reason` gains `quota_parked`,
  `budget` and `concurrency` (REQ-5, REQ-6, REQ-13).
* **SPEC-0008 REQ "Run Timeout"**: a run can also end in a budget stop (REQ-9).
* **SPEC-0003 REQ "Restart On Exit"**: an exit classified as quota exhaustion
  parks instead of restarting (REQ-13).

The key words MUST, MUST NOT, SHALL, SHALL NOT, SHOULD and MAY are used as in
RFC 2119.

## Requirements

### REQ-1: Per-harness budget keys

A `[harness.*]` table in the global `harness.toml` MAY carry:

| Key | Type | Applies to | Meaning |
| --- | --- | --- | --- |
| `max_runs_per_day` | integer ≥ 1 | any harness | Admissions per budget day. For a resident harness every process start counts, restarts included |
| `max_tokens` | integer ≥ 1 | one-shot (`prompt` or `prompt_file` set) | Tokens one run may spend: input + output + cache writes |
| `max_cost_usd` | decimal > 0 | one-shot | Dollars one run may spend |
| `daily_cost_usd` | decimal > 0 | any harness | Dollars this harness may spend per budget day |
| `quota_group` | string, `[a-z0-9._-]{1,64}` | any harness | Harnesses sharing one provider allowance; a park on one member parks all |
| `quota_backoff` | duration ≥ `1m` | any harness | First park length when the refusal gives no reset time. Default `15m` |
| `quota_backoff_max` | duration ≥ `quota_backoff`, ≤ `24h` | any harness | Longest backoff park. Default `6h` |

The daemon SHALL reject at config load, with an error naming the harness, the
key and the line: a value of the wrong type or out of range; `max_tokens` or
`max_cost_usd` on a harness with neither `prompt` nor `prompt_file`; and any of
these keys in a project `harness.toml` or a `harness_d` drop-in. A harness with
none of these keys SHALL behave exactly as before this spec, except for quota
parking (REQ-12), which applies to every harness.

#### Scenario: A valid budgeted one-shot

- **GIVEN** `[harness.pr-review]` with `prompt_file`, `max_runs_per_day = 40`, `max_tokens = 400000` and `max_cost_usd = 2.0`
- **WHEN** the config loads
- **THEN** it loads, and `harness describe pr-review` shows all three caps

#### Scenario: A per-run cap on a resident harness

- **GIVEN** `[harness.crush-sb]` with `enabled = true`, no prompt, and `max_tokens = 100000`
- **WHEN** the config loads
- **THEN** loading fails with an error naming `crush-sb`, `max_tokens` and the line, saying per-run caps apply only to one-shot harnesses

#### Scenario: Budget keys in a project file

- **GIVEN** a project `harness.toml` whose harness sets `max_runs_per_day = 5`
- **WHEN** `harness up` loads it
- **THEN** it is rejected with an error naming the key and saying budgets are only accepted in the global config

#### Scenario: Out-of-range values

- **WHEN** a harness sets `max_runs_per_day = 0`, or `quota_backoff_max = "48h"`, or `max_cost_usd = -1`
- **THEN** each fails the load with a located error, and the daemon keeps running on its previous config if this was a reload

### REQ-2: The `[budget]` table

The global `harness.toml` MAY carry a `[budget]` table:

| Key | Type | Meaning |
| --- | --- | --- |
| `day_starts` | string: optional `TZ=`/`CRON_TZ=` prefix, then `HH:MM` | When the budget day begins. Default `00:00` in the daemon's zone |
| `max_concurrent` | integer ≥ 1 | Triggered one-shot runs in flight at once, daemon-wide |
| `daily_cost_usd` | decimal > 0 | Dollars all harnesses together may spend per budget day |
| `prices.<model>` | table | Per-million-token prices for a served model name: `input_per_mtok`, `output_per_mtok`, and optional `cache_write_per_mtok`, `cache_read_per_mtok` (default 0) |
| `group.<name>.max_concurrent` | integer ≥ 1 | Triggered runs in flight at once among harnesses with `quota_group = "<name>"` |

The zone prefix SHALL use the same parsing, embedded IANA database and errors as
SPEC-0008 REQ "Schedule Time Zone". A `group.<name>` table naming a group no
harness uses SHALL load, and `harness doctor` SHALL warn about it. `[budget]` in
a project file SHALL be rejected.

#### Scenario: A budget day in another zone

- **GIVEN** `day_starts = "TZ=America/Los_Angeles 06:00"` and a daemon zone of UTC
- **WHEN** the clock reaches 13:00 UTC in summer (06:00 in Los Angeles)
- **THEN** a new budget day begins

#### Scenario: An unknown zone

- **WHEN** `day_starts = "TZ=Mars/Olympus 00:00"`
- **THEN** the load fails with SPEC-0008's unknown-zone error, located at the key

#### Scenario: A price with a missing required field

- **WHEN** `[budget.prices."gpt-5"]` sets `output_per_mtok` but not `input_per_mtok`
- **THEN** the load fails naming the model and the missing field

### REQ-3: The budget day

A budget day SHALL run from one `day_starts` instant, in its zone, to the next.
The rollover SHALL be evaluated on the ADR-0013 scheduler tick, with the same
clock seam as the operating-hours gate (SPEC-0012 REQ "Gate Evaluation"). A
daemon that was suspended or stopped across one or more rollovers SHALL treat
the current day as new on its first tick, and SHALL NOT carry spend from an
earlier day into it. On a day that is 23 or 25 hours long because of a DST
change, the day SHALL be as long as the wall clock makes it.

#### Scenario: Sleeping through midnight

- **GIVEN** a harness held `over-budget` at 23:30, and a laptop that sleeps from 23:45 to 08:00
- **WHEN** the first tick after waking runs
- **THEN** a new budget day has begun, the hold's budget reason clears, and the harness goes back through admission

#### Scenario: Fall-back day

- **GIVEN** `day_starts = "00:00"` in a zone that falls back at 02:00
- **WHEN** the day passes
- **THEN** it lasts 25 hours and its runs all count toward one day

### REQ-4: Admission

Every process start SHALL pass admission first. That covers: a schedule, catch-up,
channel or webhook firing; `harness trigger`; `harness start`; Autostart; a
reload that starts a harness; an operating-hours window opening; a lease
starting; a hold releasing; and the supervisor restarting a harness after an
exit.

Admission SHALL evaluate, in this order, and the first reason that applies SHALL
decide:

1. operating hours (SPEC-0012, unchanged);
2. a quota park on the harness or its quota group (REQ-12);
3. a spent `daily_cost_usd`, on the harness or on `[budget]` (REQ-10);
4. a spent `max_runs_per_day` (REQ-5);
5. no free concurrency slot, for a triggered one-shot only (REQ-6).

A hold another spec defines, such as SPEC-0020's model hold (skip reason
`model_hold`), SHALL be evaluated between checks 1 and 2, so that a harness is
never admitted past a refusal that depends on neither the meter nor the clock.

The admission decision and the run's `run_opened` ledger record (SPEC-0022)
SHALL be made under one lock, so that two starts cannot both take the last unit
of a budget. If the ledger append fails, a harness that has any REQ-1 budget key,
or is subject to a `[budget]` cap, SHALL be refused with reason
`ledger_unavailable` and the failure logged; a harness subject to no budget SHALL
start and the failure SHALL be logged and counted.

#### Scenario: The fortieth run races

- **GIVEN** `max_runs_per_day = 40`, 39 admissions today, and two webhook firings for the harness arriving in the same millisecond
- **WHEN** both reach admission
- **THEN** exactly one is admitted, and the other is recorded `skipped` with reason `budget`

#### Scenario: Hours decide before budgets

- **GIVEN** a harness out of hours whose run budget is also spent
- **WHEN** the gate tick evaluates it
- **THEN** it is held with reasons `hours` and `budget`, and `harness list` names the reason that clears last

#### Scenario: The ledger cannot be written

- **GIVEN** the ledger directory is on a full disk, a harness with `max_runs_per_day = 10`, and an unbudgeted harness
- **WHEN** both are fired
- **THEN** the budgeted firing is recorded as refused with reason `ledger_unavailable` in the durable log, the unbudgeted one starts, and `harness doctor` reports the ledger failure

### REQ-5: Run-count budget

With `max_runs_per_day = N`, the (N+1)th admission of a budget day SHALL be
refused with reason `budget`. Every admitted start SHALL count exactly once,
whether or not the process then spawned successfully. A refused one-shot firing
SHALL be recorded `skipped` with reason `budget`, coalesced as SPEC-0014 REQ
"Overlap Skip Coalescing" describes. A refused resident start SHALL leave the
harness held with reason `budget`, `enabled` unchanged, until the day rolls over.
A duplicate webhook delivery answered `duplicate` (SPEC-0014 REQ "Webhook
Filtering") SHALL NOT reach admission and SHALL NOT count.

#### Scenario: A one-shot runs out of runs

- **GIVEN** `max_runs_per_day = 2` and two runs today
- **WHEN** a third firing arrives
- **THEN** no process starts, the history gains a `skipped` record with reason `budget`, and after the next rollover a firing runs normally

#### Scenario: A crash loop spends the budget

- **GIVEN** a resident harness with `max_runs_per_day = 5` that exits non-zero on every start
- **WHEN** it has started five times today
- **THEN** the supervisor does not start it a sixth time, the harness is held `over-budget` rather than `failed`, and the durable log says why

#### Scenario: Catch-up after a budget skip

- **GIVEN** `catch_up = true` and firings skipped for `budget` during the day
- **WHEN** the day rolls over
- **THEN** one run starts, with trigger `catch_up`, and it counts toward the new day

### REQ-6: Concurrency admission

With `[budget] max_concurrent = N`, at most N triggered one-shot runs SHALL be in
flight across the daemon. With `[budget.group.<g>] max_concurrent = M`, at most M
SHALL be in flight among harnesses whose `quota_group` is `g`. Resident harnesses
SHALL NOT count toward or be limited by either cap.

A firing refused for concurrency SHALL wait in an admission queue, at most one
waiting firing per harness. Slots SHALL be granted in the order the waiting
firings arrived. A further firing for a harness that already has one waiting
SHALL coalesce into one `skipped` record with reason `concurrency`. A waiting
firing SHALL be recorded `cancelled` if its harness is stopped, and
`interrupted` if the daemon shuts down (SPEC-0008 semantics for a held firing).
A waiting firing SHALL still pass checks 1–4 of REQ-4 again when its slot is
granted.

#### Scenario: A fan-out under a cap

- **GIVEN** `max_concurrent = 4` and one webhook bound to ten harnesses
- **WHEN** one delivery arrives
- **THEN** four runs start, six firings wait, and as each run ends the oldest waiting firing starts, until all ten have run

#### Scenario: A burst for one waiting harness

- **GIVEN** `pr-review` has a firing waiting for a slot
- **WHEN** 30 more deliveries for `pr-review` arrive before a slot frees
- **THEN** one run starts when a slot frees, and the history holds one `skipped` record with `coalesced` = 30 and reason `concurrency`

#### Scenario: A group cap inside the daemon cap

- **GIVEN** `max_concurrent = 8`, `[budget.group.claude-max] max_concurrent = 2`, and five group members fired at once
- **WHEN** admission runs
- **THEN** two group members run and three wait, while harnesses outside the group are admitted up to the daemon cap

### REQ-7: Durable counters

"Runs today" for a harness SHALL be the number of its ledger records admitted
since the current budget day began, and "cost today" SHALL be the sum of their
`cost_usd`. The daemon SHALL rebuild both from the ledger at boot before it
admits any start, and SHALL keep them as running totals after that. It SHALL NOT
keep budget counters anywhere else. A run in flight at a rollover SHALL count
toward the day it was admitted in; cost it spends after the rollover SHALL
count toward the new day.

#### Scenario: A restart does not refund

- **GIVEN** `max_runs_per_day = 3` and three runs today
- **WHEN** the daemon restarts and a firing arrives
- **THEN** it is recorded `skipped` with reason `budget`

#### Scenario: A lost ledger file

- **GIVEN** today's ledger file was deleted by hand while the daemon was stopped
- **WHEN** the daemon boots
- **THEN** it rebuilds today's counters from what remains, logs that the ledger has a gap, and `harness doctor` reports it

### REQ-8: Cost resolution

For each usage item the accumulator folds (SPEC-0022), the cost SHALL be taken
from the first of: the cost the agent recorded (`recorded`); the served model's
`[budget.prices]` entry multiplied by the item's tokens (`priced`); otherwise
unknown (`unknown`). A run's `cost_source` SHALL be the weakest source among its
items. Unknown cost SHALL count as zero toward every cost cap. The daemon SHALL
NOT ship default prices.

#### Scenario: Claude Code with a price table

- **GIVEN** a claude-code one-shot served by `claude-sonnet-4-6`, and a `[budget.prices."claude-sonnet-4-6"]` entry
- **WHEN** the run spends 1,000,000 input and 100,000 output tokens
- **THEN** its cost is computed from the entry, and its `cost_source` is `priced`

#### Scenario: No price and no recorded cost

- **GIVEN** a codex one-shot with `max_cost_usd = 1.0` and no price for its model
- **WHEN** it runs
- **THEN** the cap is never tripped, the record's `cost_source` is `unknown`, and `harness doctor` lists the cap as unmeasurable, naming the model

### REQ-9: Per-run caps

While a one-shot run is in flight, the daemon SHALL compare its accumulated
tokens (input + output + cache writes) with `max_tokens`, and its accumulated
cost with `max_cost_usd`, each time the accumulator folds a usage item. When
either is crossed, the daemon SHALL stop the run as SPEC-0008 REQ "Run Timeout"
stops a timed-out run (SIGTERM, the stop grace, SIGKILL of the process group),
record the run's outcome as `budget_exceeded` with `reason` naming the cap, and
return the harness to `stopped`, not `failed`. The stop SHALL NOT count toward
crash-loop detection. A cap that cannot be measured for the run (no trace
reader, or unknown cost for `max_cost_usd`) SHALL NOT stop it.

#### Scenario: Crossing max_tokens

- **GIVEN** `max_tokens = 400000`
- **WHEN** the accumulator folds a usage item that brings the run to 410,000 tokens
- **THEN** the run is stopped, its record reads `budget_exceeded` with reason `max_tokens`, the harness is `stopped`, and the next firing starts normally

#### Scenario: A run with no trace reader

- **GIVEN** a one-shot with `max_tokens` whose runs have no trace reader (a harness with no adapter reader, or an ADR-0023 `command` harness without `transcripts`)
- **WHEN** it runs and spends any number of tokens
- **THEN** it is never stopped for tokens, and `harness doctor` reports the cap as unmeasurable

### REQ-10: Daily cost caps

A harness's `daily_cost_usd` and `[budget] daily_cost_usd` SHALL be checked at
admission (REQ-4 check 3) and each time a usage item is folded. When a one-shot
run crosses a daily cap, it SHALL be stopped as in REQ-9 with reason
`daily_cost_usd`. When a resident harness crosses one, it SHALL be closed the
way SPEC-0012 REQ "Graceful Shutdown" closes it, using its `hours_shutdown` and
`hours_shutdown_timeout` values (default graceful with a 15-minute cap), and
then held with reason `budget` until the day rolls over. The run that crossed the
cap SHALL be recorded `budget_exceeded`. Crossing `[budget] daily_cost_usd`
SHALL hold every harness whose cost is measurable.

#### Scenario: A resident spends its day

- **GIVEN** a resident crush with `daily_cost_usd = 15` mid-turn when its cost reaches 15.02
- **WHEN** the accumulator folds that item
- **THEN** it enters a graceful close, stops at its turn end or at 15 minutes, is shown `over-budget` with NEXT `budget resets 00:00`, and `enabled` is still true

#### Scenario: The daemon-wide cap

- **GIVEN** `[budget] daily_cost_usd = 50` and three metered harnesses whose day's spend reaches 50
- **WHEN** the crossing item is folded
- **THEN** all three are held with reason `budget`, and an unmetered harness keeps running

### REQ-11: Quota detection

The daemon SHALL classify model errors with the one classifier SPEC-0013 REQ-3
defines; metrics and budgets SHALL NOT keep separate tables. Errors SHALL reach it
from the observer's error marks, and, only for a one-shot run that exits
non-zero with no classified error observed during the run, from the last 4 KiB
of that run's sanitized run log. The run-log fallback SHALL NOT be used for a
resident harness or for a zero exit.

#### Scenario: A crush quota error mark

- **WHEN** the observer delivers a crush error mark whose details name `litellm.RateLimitError`
- **THEN** it is classified `quota` for both the metrics series and the park detector

#### Scenario: A Claude Code usage limit on a one-shot

- **GIVEN** agent-trace does not surface claude-code API errors (stump.wtf/agent-trace#104 open)
- **WHEN** a claude-code `-p` run exits 1 and its run log ends with "Claude AI usage limit reached|1790000000"
- **THEN** the fallback classifies it `quota` with a reset time of 1790000000

#### Scenario: The phrase in a successful run

- **WHEN** a run that exits 0 printed "usage limit reached" in its output
- **THEN** nothing is classified and nothing parks

### REQ-12: Parking

A harness SHALL be parked when a `quota` error either:

* carries a reset time the daemon can parse (REQ-12 formats in `design.md`),
  in which case the harness is parked until that time; or
* leaves the harness **stuck on quota**: for a resident, at least 3 `quota` errors
  and no successful model call within the last 10 minutes; for a one-shot, a run
  that ended with a `quota` error as its last classified model outcome. The park
  then lasts `quota_backoff`, doubled for each consecutive park without an
  intervening successful model call, up to `quota_backoff_max`.

A parsed reset time more than 8 days in the future SHALL be clamped to 8 days,
and one in the past or less than 1 minute away SHALL be treated as no reset time.
A successful model call SHALL reset the stuck count and the backoff step. The
daemon SHALL NOT park on classes other than `quota`.

#### Scenario: A reset time in the message

- **WHEN** a `quota` error reads "5-hour limit reached ∙ resets 3pm (America/Los_Angeles)" at 12:40 Los Angeles time
- **THEN** the harness is parked until 15:00 Los Angeles time

#### Scenario: A rate limit that recovers

- **GIVEN** a resident harness that gets two 429s, then a successful tool call
- **WHEN** a third 429 arrives within the 10 minutes
- **THEN** it is not parked, because the success reset the count

#### Scenario: Backoff grows, then resets

- **GIVEN** `quota_backoff = "15m"`, `quota_backoff_max = "1h"`, and refusals with no reset time
- **WHEN** the harness is parked four times in a row
- **THEN** the parks last 15m, 30m, 1h and 1h, and after a successful call the next park lasts 15m

#### Scenario: A hostile reset time

- **WHEN** an error reads "usage limit reached|99999999999"
- **THEN** the park lasts 8 days, and `harness doctor` shows the clamp

### REQ-13: Park effects

On parking, the daemon SHALL first write the park (harness or group, the reset
instant, the matched classifier rule's name, the backoff step) to `state.json`,
then:

* stop a running **resident** harness at once (the SPEC-0003 graceful-stop
  sequence, with no graceful close), leave `enabled` unchanged, not restart it,
  not count the exit toward crash-loop detection, and reset its backoff;
* record the detecting **one-shot** run's outcome as `quota_parked`, record later
  firings as `skipped` with reason `quota_parked` (coalesced), and leave the
  harness `stopped`;
* with `quota_group`, park every member with the same reset instant.

The supervisor SHALL consult the detector before applying the restart policy to
an exit. A harness SHALL NOT move to `failed` because of an exit the detector
classified as quota exhaustion. The park SHALL NOT be stored with any error text
(ADR-0008).

#### Scenario: The 2026-09-19 shape

- **GIVEN** a resident crush whose provider refuses every call with a quota error, and a restart policy with `MaxRestarts = 5`
- **WHEN** it is detected stuck on quota
- **THEN** it is parked, not restarted; it never reaches `failed`; and it starts by itself when the park expires

#### Scenario: A group parks together

- **GIVEN** three harnesses with `quota_group = "claude-max"`
- **WHEN** one of them is parked until 15:00
- **THEN** all three are parked until 15:00, and each one's `describe` names the member that triggered it

#### Scenario: A restart during a park

- **GIVEN** a harness parked until 15:00
- **WHEN** the daemon restarts at 14:00
- **THEN** the harness boots parked until 15:00 and makes no model call before then

#### Scenario: A malformed park in state.json

- **GIVEN** one harness's park record does not decode
- **WHEN** the daemon boots
- **THEN** that harness boots unparked, the error is logged, and every other harness's park and state loads normally

### REQ-14: Release and hold reasons

A harness's hold SHALL be a set of reasons drawn from `hours`, `quota` and
`budget`, and SHALL admit reasons other specs define (SPEC-0020's model hold is
one) without changing the release rule below. The gate tick SHALL clear `quota` when the park's reset instant is
reached, and `budget` when the budget day rolls over or the spent cap is raised
by a reload. A harness whose last hold reason clears SHALL go back through
admission (REQ-4); it SHALL start only if admission passes and `enabled` is true.
A `failed` harness SHALL stay failed. Every listing surface SHALL show the reason
that clears last and its time. This amends SPEC-0012 REQ "Gate Enforcement",
whose `held` becomes "held for at least one reason".

#### Scenario: A park expires out of hours

- **GIVEN** a harness parked until 15:00 whose operating hours close at 14:00
- **WHEN** 15:00 passes
- **THEN** it stays held for `hours` and starts at the next window opening

#### Scenario: A disabled parked harness

- **GIVEN** a parked harness whose `enabled` is false
- **WHEN** the park expires
- **THEN** the `quota` reason clears and the harness stays down

### REQ-15: Operator overrides

`harness start NAME` and `harness trigger NAME` on a harness parked for quota
SHALL clear the park (the harness's own; a group park is cleared for that member
only), log the override, and start it through admission checks 1, 3, 4 and 5. On
a harness refused for `budget`, they SHALL fail with an error naming the spent
counter ("40/40 runs today" or "15.02/15.00 USD today") unless given
`--over-budget`, which SHALL admit exactly one run for a one-shot, or start a
lease for a resident (default `1h`, `--for` to change, with SPEC-0012 REQ
"After-Hours Lease" semantics). An admitted override SHALL be recorded in the
ledger with `override = true`. `harness stop NAME` SHALL keep its SPEC-0003
meaning in every state and SHALL also discard any park or lease. The
`over_budget` field SHALL be accepted only on the control socket; the local MCP
surface (SPEC-0005) SHALL NOT expose or forward it, and its `harness_start` tool
SHALL respect every budget and park.

#### Scenario: Topped up credits

- **GIVEN** a harness parked until tomorrow after the operator bought credits
- **WHEN** the operator runs `harness start crush-sb`
- **THEN** the park is cleared, the harness starts, and if the provider still refuses it parks again at the next backoff step

#### Scenario: Over budget without the flag

- **GIVEN** a one-shot with `max_runs_per_day = 40` and 40 runs today
- **WHEN** the operator runs `harness trigger pr-review`
- **THEN** it fails with "over budget: 40/40 runs today (pass --over-budget to run once anyway)" and no run starts

#### Scenario: An agent tries to lift its own budget

- **GIVEN** an agent with the local MCP surface enabled whose harness is over budget
- **WHEN** it calls `harness_start` on itself or a sibling
- **THEN** the call fails with the budget error, and no override is recorded

### REQ-16: Listing surfaces

`harness list`, `describe`, `jobs` and the TUI SHALL show a harness held for
`quota` as `parked` and one held for `budget` as `over-budget`, styled like
`off-hours` (SPEC-0012 REQ "Operating Hours Visibility"): never as `stopped` and
never in failed styling. NEXT SHALL read `resets <time>` for a park, `budget
resets <time>` for a budget hold, and `waiting <in flight>/<cap>` for a firing
queued on concurrency. No column SHALL be added. The harness projection SHALL
gain `hold_reasons`, `parked_until`, `park_rule`, `park_group` and a `budget`
object (`runs_today`, `max_runs_per_day`, `cost_today_usd`, `daily_cost_usd`,
`cost_source`), each omitted when it does not apply.

#### Scenario: A parked harness in the list

- **GIVEN** `crush-sb` parked until 15:00
- **WHEN** the operator runs `harness list`
- **THEN** its STATE reads `parked`, its NEXT reads `resets 15:00`, and `--json` carries `parked_until` and `hold_reasons: ["quota"]`

### REQ-17: Doctor

`harness doctor` SHALL report: the budget day and its next rollover; each
harness's caps and today's counters; every cap it cannot measure, with the
reason (no trace reader, a `command` harness without `transcripts`, no recorded
cost and no price for a served model); parked harnesses with their reset, rule
and whether the reset was clamped; `[budget.prices]` entries no run has used;
served models with no price where a cost cap needs one; `group` tables no harness
uses; and any ledger failure that REQ-4 or REQ-7 logged. Each warning SHALL be
shown firing in a test.

#### Scenario: An unmeasurable cost cap

- **GIVEN** a claude-code one-shot with `max_cost_usd` and no price for the model its runs were served by
- **WHEN** the operator runs `harness doctor`
- **THEN** it warns that the cap cannot be measured, naming the harness and the model

### REQ-18: Metrics

When SPEC-0013's endpoint is enabled, the daemon SHALL export:

```
harness_budget_runs_today{harness}                 gauge
harness_budget_runs_limit{harness}                 gauge    absent when unset
harness_budget_cost_today_usd{harness}             gauge
harness_budget_cost_limit_usd{harness}             gauge    absent when unset
harness_budget_daemon_cost_today_usd               gauge
harness_quota_parked{harness}                      gauge    0|1
harness_quota_parked_until_timestamp{harness}      gauge    absent when not parked
harness_admission_decisions_total{harness,decision,reason}   counter
harness_runs_in_flight                             gauge
harness_runs_concurrency_limit                     gauge    absent when unset
```

`decision` SHALL be `admitted`, `held`, `skipped` or `waiting`; `reason` SHALL be
`none`, `hours`, `quota_parked`, `budget`, `concurrency` or `ledger_unavailable`.
Every harness SHALL report every `decision`/`reason` pair it can produce,
including zeros. Series SHALL follow SPEC-0013 REQ-5's cardinality cap. A
harness held for quota SHALL continue to map to `stopped` in
`harness_harness_state`, never `failed`.

#### Scenario: Alerting on a park

- **WHEN** a harness is parked
- **THEN** `harness_quota_parked{harness="crush-sb"}` reads 1 and `harness_quota_parked_until_timestamp` carries the reset on the next scrape

### REQ-19: Events and the durable log

The daemon SHALL write a durable log line (ADR-0007) and publish a lifecycle event
on every park, park clear, release, budget hold, budget stop, concurrency wait
and override, each carrying the reason and the next transition time.
`harness_hold_changed { name, hold_reasons, next }` SHALL be emitted when a
harness's hold reasons change. SPEC-0002's closed event list is amended to name
it.

#### Scenario: A park is logged

- **WHEN** `crush-sb` is parked by rule `crush/litellm.ratelimiterror` until 15:00
- **THEN** its durable log gains a line naming the rule, the reset and that no restart will happen, and subscribers receive `harness_hold_changed`

### REQ-20: Reload

Budget keys and `[budget]` SHALL apply at the next tick after a reload, without
restarting any harness. Raising a spent cap SHALL clear a `budget` hold at the
next tick; lowering a cap below today's counter SHALL hold a resident at the next
tick and refuse the next one-shot admission. Removing `quota_group` from a member
SHALL NOT clear a park already in force. A change to `day_starts` SHALL take
effect at the next rollover, and SHALL NOT start a new day immediately.

#### Scenario: Raising the cap mid-day

- **GIVEN** a harness held `over-budget` at 30/30 runs
- **WHEN** the operator raises `max_runs_per_day` to 40 and the config reloads
- **THEN** the budget hold clears on the next tick and the harness goes through admission

### REQ-21: Error handling and concurrency safety

Admission, counters, the park detector and the gate SHALL:

* wrap errors with the harness and the operation at each boundary, and define
  sentinel errors for `ErrOverBudget`, `ErrParked` and `ErrLedgerUnavailable`
  that the control op and the CLI can distinguish;
* never swallow an error: every failure is returned, logged with structured
  fields, or explicitly handled with a comment saying why;
* never block the supervisor's actor loop or the observer on a budget decision
  or a ledger write, beyond the one synchronous append that admission requires;
* protect shared counters and the admission queue with the Manager's lock or
  message passing, and be tested under the race detector in CI.

#### Scenario: A slow disk does not stall supervision

- **GIVEN** a ledger append that takes 2 seconds
- **WHEN** a resident harness with no budget exits and restarts meanwhile
- **THEN** its restart is not delayed by the pending append

## Out of Scope

* Weekly or monthly budget windows, and per-profile budgets (ADR-0027 Deferred).
* Querying a provider's usage or billing API.
* Queueing work for a held harness. Work waits in its system of record
  (Switchboard), not in Harness.
* Budgets on project harnesses.

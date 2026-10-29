---
title: "Merge train"
sidebar_position: 8.5
---

# Merge train

The merge train is a daemon-side loop that lands approved pull requests on a
repository, one at a time, so that the tree CI tested is the tree that reaches
`main`. Reviewers approve and stop; the train does the merging, and no LLM
session ever merges. The design is [ADR-0032](/decisions/adr-0032-merge-train)
and the behaviour is [SPEC-0025](/specs/merge-train/spec); the operator's
walkthrough (pilot, cutover, rollback) is the
[Run the merge train](/guides/merge-train) guide.

## Configuration

The train is off unless the `[mergetrain]` table is present and enabled:

```toml
[mergetrain]
enabled = true
mode = "report"                 # "report" (default) or "merge"
repos = ["stump.wtf/harness"]
base_branch = "main"
poll_interval = "60s"
ci_timeout = "30m"
flake_cool_down = "5m"
forge_base_url = "https://gitea.stump.rocks"
forge_token_env = "HARNESS_MERGETRAIN_TOKEN"
```

| Field | Meaning |
|-------|---------|
| `enabled` | off by default (ADR-0006); with no `[mergetrain]` section the daemon is unchanged |
| `mode` | `report` builds and tests trains but writes nothing to any PR — it logs `would merge` / `would comment` instead. `merge` actually lands. Default `report` |
| `repos` | **required when enabled** — the `owner/name` repos the train serves; empty with `enabled = true` is a config error |
| `base_branch` | the branch the train lands on (default `main`) |
| `poll_interval` | CI poll cadence on the train commit, with backoff from `poll_interval / 4` (min 5 s) up to this value |
| `ci_timeout` | give up a train whose CI has not finished after this long (comment, cause `timeout`) |
| `forge_base_url` | the forge's API base |
| `forge_token_env` | the **name** of the environment variable holding the forge token — the token itself never appears in `harness.toml`, logs, errors or argv |
| `batch` | PRs per train commit, a whole number of at least 1 (default 1). Above 1 batches heads onto one train commit and bisects on red; see [Batching](#batching) |
| `flake_cool_down` | how long a red train waits before its one retry on a runner failure (default `5m`; `0` turns the retry off, which is the pre-#1016 behaviour); see [Runner flakes](#runner-flakes) |

A change to `[mergetrain]` takes effect at the next daemon restart. The train
holds an exclusive per-repo lock
(`$XDG_STATE_HOME/harness/mergetrain/<owner>_<name>.lock`), so a second driver
for the same repo fails to start rather than racing.

## Modes

- **`report`** — the pilot mode. Everything runs: eligibility, train branch,
  CI wait. On green the driver logs `mergetrain would merge` and touches
  nothing. Watch the daemon log for `mergetrain queue`, `would merge`,
  `would comment` and `bypass detected`.
- **`merge`** — on green the train squash-merges with both sides pinned
  (PR head and base SHA re-checked immediately before, `head_commit_id` in the
  call itself), then verifies the landed tree and content, and halts loudly if
  anything disagrees.

## What it logs

Every transition is one structured line, message `mergetrain <event>`, with
`repo` and where applicable `pr`, `head`, `base`, `train`, `tree`, `cause`,
`err`: `started`, `queue`, `building`, `built`, `green`, `red`, `conflict`,
`timeout`, `stale`, `deleted`, `delete failed`, `would merge`,
`would comment`, `merged`, `verified`, `merge refused`, `commented`,
`flake retry`, `flake retry exhausted`, `tick failed`, `bypass detected`,
`halted`, `shutting down`, `stopped`.

A stale line carries one more key, note, when its ineligibility has a reading
no other field shows: an approval still left on the head that the forge no
longer counts. See the merge-train guide for what to do about it.

There is no per-poll CI line: `built` marks the start of the CI wait, and
`green`, `red` or `timeout` its end. `shutting down` (warn) means the daemon
stopped between a merge and its verification. The merge stands unverified,
and the train does not halt for it.

## Batching

`batch = N` (> 1) puts up to N eligible PRs, in merge order, on one train
commit — one CI run for the whole batch — and bisects a red batch by halves
until a single PR is isolated and gets the usual `red` comment; halves that
go green land. A conflict folds onto the heads before the first rejecting
head, which gets the usual `conflict` comment. Merging, re-checking and
verification stay per PR, so batching changes throughput, not safety
(SPEC-0025 REQ-17). `batch = 1`, the default, is the one-PR-per-train
behaviour exactly.

The value must be a whole number of at least 1 (`0`, a negative number or a
string is a config error with its line). Batch train commits live on
`train/batch/<pr>-<pr>…` branches, which are deleted when the attempt ends.

## Runner flakes

A runner that never attaches fails a job with no log at all, and the combined
CI state reads exactly like a failing test. When a train goes red and *every*
failed job on that commit carries that signature, the train logs
`mergetrain flake retry` and tries the same tree once more after
`flake_cool_down` (default 5 minutes), instead of waiting hours for `main` or
the PR head to move. Nothing is commented and nothing is remembered in the
meantime, so a PR whose tree was never shown to be wrong keeps its place in the
queue, and other PRs use the ticks in between.

The retry is strictly once per `(pr, head, base)`. If the retry fails with the
same signature the train logs `mergetrain flake retry exhausted` and comments
as it always did, saying the tree was re-run first; a genuine test failure
skips the retry entirely and comments immediately. So a broken tree cannot loop
CI, and the cool-down is the only added wait. The forge's per-job detail decides
this: a forge that cannot report it, a job with a log, a `cancelled` or
timed-out job, and a skipped job all count as real failures (SPEC-0025 REQ-18,
ADR-0056).

Runner capacity itself is not this driver's problem: a run whose jobs die this
way on several different runners is a host fault, and the train retries once
and then says so.

---
status: draft
date: 2026-09-23
implements: [ADR-0032]
requires: [SPEC-0010]
---

# SPEC-0025: Merge Train

## Overview

A **merge train** is a deterministic loop inside the Harness daemon that lands
approved pull requests on one repository, one at a time, so that the tree CI
tested is the tree that reaches `main`. ADR-0032 records the decision and the
options; this spec is the behaviour, and the [design notes](./design.md) beside it
cover the package layout and the `Forge` interface.

Terms:

- **repo** — `owner/name` on the forge.
- **base branch** — the branch the train lands on (`main`).
- **head** — the SHA a PR's head branch points at.
- **base** — the SHA the base branch points at when a train is built.
- **train branch** — `train/<pr>`, a branch the train creates for one attempt.
- **train commit** — the one commit on the train branch: `base` + a squash of
  `head`, parent `base`.
- **attempt** — one pass of the state machine for one `(pr, head, base)`.
- **tick** — one iteration of the driver loop; at most one attempt per tick.
- **qualifying approval** — a review with state `APPROVED`, by an author other
  than the PR's, left on the PR's current head.

## Requirements

### Requirement: REQ-1 Off by default, two modes

The merge train SHALL NOT run unless `[mergetrain] enabled = true` and `repos`
is non-empty. It SHALL support two modes: `report`, which builds and tests
trains exactly as `merge` does but writes nothing to any pull request — no
merge, no comment — and logs `would merge` and `would comment` instead; and
`merge`. The default mode SHALL be `report`. When enabled, the daemon SHALL log one line at startup
naming the mode and every repo it will act on.

#### Scenario: No section

- **WHEN** `harness.toml` has no `[mergetrain]` section
- **THEN** no driver starts and daemon behaviour is unchanged

#### Scenario: Enabled with no repos

- **WHEN** `enabled = true` and `repos = []`
- **THEN** config validation fails and the daemon refuses to start

#### Scenario: Report mode

- **WHEN** the mode is `report` and a train goes green
- **THEN** the driver logs `would merge` for the PR and calls no merge or
  comment endpoint

### Requirement: REQ-2 Eligibility

A PR SHALL enter the train only when all of these hold, evaluated in this
order, and the first that fails SHALL be the reason reported:

1. its base branch is the train's base branch — `base is not <branch>`;
2. it is not a draft — reason `draft`;
3. the combined CI status of its head is `success` — `ci not green`;
4. the forge reports it mergeable — `not mergeable`;
5. it has at least one qualifying approval — `no approval on current head`;
6. no review with state `REQUEST_CHANGES` was left on its current head —
   `changes requested`.

#### Scenario: A stale approval

- **WHEN** the only approval was left on an earlier head
- **THEN** the PR is ineligible with `no approval on current head`

#### Scenario: Self-approval

- **WHEN** the only approval is by the PR's author
- **THEN** the PR is ineligible with `no approval on current head`

#### Scenario: A stacked PR

- **WHEN** a PR's base branch is another PR's branch, not the train's base
  branch
- **THEN** the PR is ineligible with `base is not <branch>` and can never be
  squash-merged into its parent branch (#870)

### Requirement: REQ-3 Ordering

Eligible PRs SHALL be ordered by the `SubmittedAt` of their earliest
qualifying approval, ascending, then by PR number, ascending. The order SHALL
be a function of the set of PRs alone, independent of input order.

#### Scenario: A tie

- **WHEN** two PRs have identical earliest-qualifying-approval times
- **THEN** the lower PR number goes first

### Requirement: REQ-4 Building the train

For an attempt, the driver SHALL read the base branch's head as `base`, then
create `train/<pr>` pointing at a new commit whose parent is `base` and whose
tree is the three-way merge of `base` and the PR's `head`. It SHALL NOT push
to, rewrite or delete any branch other than `train/<pr>`. If the PR's head on
the forge is no longer `head`, the build SHALL fail as **stale**. If the merge
has conflicts, the build SHALL fail as **conflict** without creating a branch
and without polling CI.

#### Scenario: A conflict

- **WHEN** `head` and `base` both change the same lines of a file
- **THEN** the attempt ends `conflict`, no `train/<pr>` exists, and
  `CombinedStatus` is never called

### Requirement: REQ-5 Waiting for CI on the train commit

The driver SHALL poll the combined status of the **train commit** (not the PR
head) with exponential backoff from `poll_interval / 4` (minimum 5 s) capped at
`poll_interval`, until it is `success`, `failure` or `error`, or until
`ci_timeout` has elapsed since the branch was created. A commit with no
statuses SHALL be treated as `pending`. The outcomes SHALL be **green**
(`success`), **red** (`failure` or `error`) and **timeout**.

#### Scenario: CI never starts

- **WHEN** no workflow reports a status on the train commit
- **THEN** the status stays `pending` and the attempt ends `timeout` at
  `ci_timeout`

### Requirement: REQ-6 The train branch is always deleted

`train/<pr>` SHALL be deleted when the attempt leaves the testing state, on
every path: green, red, timeout, forge error, panic and context cancellation.
Deletion SHALL use a context not derived from the attempt's, bounded to 30 s,
so that cancellation does not prevent cleanup. A failed deletion SHALL be
logged at warn and SHALL NOT change the attempt's outcome.

#### Scenario: Shutdown mid-poll

- **WHEN** the daemon stops while the driver is polling CI
- **THEN** `train/<pr>` is deleted before the driver returns

### Requirement: REQ-7 Merge preconditions

On green, and only in `merge` mode, the driver SHALL, immediately before
merging, re-list the open PRs and re-read the base branch's head, and SHALL
merge only if the PR is still open, still eligible, still at `head`, and the
base branch is still at `base`. Otherwise the attempt ends **stale** with no
comment. The merge SHALL be a squash merge with `head` passed as the expected
head commit, so the forge refuses it if the head moved after the re-read.

#### Scenario: The author pushes during CI

- **WHEN** the PR's head changes while its train is being tested
- **THEN** it is not merged, no comment is posted, and the next tick evaluates
  it afresh

### Requirement: REQ-8 Verification after the merge

After every merge the driver SHALL verify, before it starts another attempt:

1. the base branch's head equals the merge commit the forge returned;
2. the merge commit's git tree id equals the train commit's tree id, compared
   with real tree ids from git, never from the forge's REST API;
3. `VerifyLanded` against the base branch succeeds for every path the train
   commit adds or modifies, with the bytes read from the train commit.

If any check fails, or cannot be performed, the driver SHALL log at error,
comment on the PR with cause `verify-failed`, and **halt**: it starts no
further attempt until the daemon restarts. The `merged` flag on the PR SHALL
NOT be used as evidence of anything.

#### Scenario: The forge merged a different tree

- **WHEN** the merge commit's tree differs from the train tree
- **THEN** the driver halts and says which PR and which two trees

### Requirement: REQ-9 One comment per failure per head

On `conflict`, `red`, `timeout`, `merge-refused`, `verify-failed` and
`sequence-conflict`, the driver SHALL post exactly one comment on the PR that
@mentions the PR's author,
states the cause and the train commit or head, and ends with the marker

```text
<!-- harness-mergetrain v1 pr=<n> head=<head> cause=<cause> -->
```

Before posting it SHALL list the PR's comments and SHALL NOT post if any
comment already carries a marker with the same `pr` and `head`. `stale` and
forge errors SHALL NOT produce a comment.

#### Scenario: The same failure on the next tick

- **WHEN** a PR's train is red, and a later attempt on the same head is red
  again after `main` moved
- **THEN** there is still exactly one merge-train comment for that head

### Requirement: REQ-10 Attempt memory

The driver SHALL remember, in memory, every `(pr, head, base)` whose attempt
ended `conflict`, `red`, `timeout` or `merge-refused`, and SHALL NOT start an
attempt for the same triple again. A change to the PR's head or to the base
branch's head SHALL make the PR attemptable again. On `stale` the triple SHALL
NOT be remembered.

#### Scenario: A red flake

- **WHEN** a train is red, then another PR lands
- **THEN** the red PR is attempted again on the new base

### Requirement: REQ-11 Per-repo singleton

A driver SHALL hold an exclusive, non-blocking lock for its repo for its whole
lifetime, on `$XDG_STATE_HOME/harness/mergetrain/<owner>_<name>.lock`. A second
driver for the same repo SHALL fail to start with an error naming the repo; it
SHALL NOT wait for the lock.

#### Scenario: Two drivers

- **WHEN** a driver for `stump.wtf/harness` is running and a second is started
- **THEN** the second returns an error and performs no forge call

### Requirement: REQ-12 Forge access and credentials

Every forge interaction SHALL go through the `forge.Forge` interface
(see the [design notes](./design.md)). The forge token SHALL be read from the environment variable
named by `forge_token_env` and SHALL NOT appear in `harness.toml`, in any log
line, in any error string, or in any process argv; git SHALL receive it through
the environment. A non-2xx response SHALL become an error naming the method,
the status code and the repo, and SHALL carry the response body only as a
bounded, credential-redacted summary (ADR-0049): the raw body SHALL NOT be
included, because a hostile or broken forge can echo the `Authorization`
header in an error body, and the error string reaches log lines and, for a
merge refusal, a public pull request comment.

### Requirement: REQ-13 No replays, no PR-branch writes

No code path in the merge train SHALL open, close or edit a pull request, or
push to a PR's head branch. The only writes are: creating and deleting
`train/<pr>`, the squash merge, and comments.

### Requirement: REQ-14 Observable transitions

Every transition SHALL produce one structured log line whose message is
`mergetrain <event>` with keys `repo` and, where they apply, `pr`, `head`,
`base`, `train`, `tree`, `cause`, `err`. Events: `started`, `queue`,
`building`, `built`, `green`, `red`, `conflict`, `timeout`, `stale`, `sequence conflict`,
`deleted`, `delete failed`, `would merge`, `would comment`, `merged`, `verified`,
`merge refused`, `commented`, `tick failed`, `bypass detected`, `halted`,
`shutting down`, `stopped`. `shutting down` is logged at warn when the
driver's context is cancelled between a merge and its REQ-8 verification: the
driver returns without halting, because a stop is not a verification failure.

### Requirement: REQ-15 Shutdown

On daemon stop the driver's context SHALL be cancelled; the driver SHALL
finish or abandon the current attempt, delete its train branch (REQ-6),
release its lock and return. The daemon SHALL wait for it.

### Requirement: REQ-16 Bypass audit

At the start of each tick, if the base branch's head is not the last head the
driver observed or produced, the driver SHALL log `bypass detected` at warn
with the previous and current heads, unless the change is explained by a merge
this driver performed.

## State machine (one attempt)

```mermaid
stateDiagram-v2
    direction left-right
    state building
    state testing
    state recheck
    state merging
    state verifying

    [*] --> queue
    queue --> building: next eligible PR
    building --> testing: build ok
    building --> sequenceConflict:::danger: two files claim one number (REQ-18)
    building --> conflict:::danger: merge conflict
    building --> stale: head moved
    testing --> recheck: CI success
    testing --> red:::danger: CI failure / error
    testing --> timeout:::danger: ci_timeout elapsed
    testing --> testing: poll (backoff)
    testing --> abort: context cancelled
    abort --> [*]: train branch deleted
    recheck --> merging: still eligible
    recheck --> stale: PR or base moved
    merging --> verifying: merged
    merging --> mergeRefused:::danger: forge refuses (4xx)
    verifying --> landed: tree + content match
    verifying --> halted:::danger: mismatch (HALT)
```

`report` mode ends at `recheck` with `would merge`. The train branch is deleted
on every exit from `testing`.

## Forge calls per transition

| Transition | Call | Gitea endpoint (see the [design notes](./design.md)) |
|---|---|---|
| queue | `ListOpenPRs` | `GET /repos/{r}/pulls?state=open`, `GET …/pulls/{n}/reviews`, `GET …/commits/{head}/status` |
| queue → building | `BranchHead` | `GET /repos/{r}/branches/{base}` |
| building → testing | `CreateTrainBranch` | git: fetch, `merge-tree --write-tree`, `commit-tree`, push `refs/heads/train/<pr>` |
| building, watched paths | `TreePaths` (train commit and base) | git: `ls-tree -r --name-only <sha> -- <dir>…` (REQ-18) |
| testing | `CombinedStatus` (repeated) | `GET /repos/{r}/commits/{train}/status` |
| testing → green | `FileContentAtRef` per changed path | `GET /repos/{r}/raw/{path}?ref={train}` |
| leave testing | `DeleteBranch` | `DELETE /repos/{r}/branches/train%2F<pr>` |
| recheck | `ListOpenPRs`, `BranchHead` | as above |
| merging | `SquashMerge` | `POST /repos/{r}/pulls/{n}/merge` `{"Do":"squash","head_commit_id":…}` |
| verifying | `BranchHead`, `TreeOf`, `FileContentAtRef` | branch read; git fetch + `rev-parse <sha>^{tree}`; raw reads at the base branch |
| any failure | `ListComments`, `Comment` | `GET`/`POST /repos/{r}/issues/{n}/comments` |

## Open Questions

- **Required contexts.** v1 requires the combined status to be `success` over
  whatever ran on the train commit. Pinning the list to branch protection's
  `status_check_contexts` would catch a workflow that silently stopped
  triggering on `train/**`.

## Requirement: REQ-17 Batching — several PRs share one CI run

The driver SHALL support a per-repo `batch` size (default 1, meaning the v1
one-PR-per-train behaviour). With `batch = N` > 1, a tick SHALL take up to N
eligible PRs in merge order, build **one** train commit whose tree is the
merge of `base` and every head in order, and wait for CI on that commit.

- On green, the driver SHALL squash-merge each PR in order, each with its
  `head_commit_id` pinned and the REQ-7 preconditions re-checked per PR, and
  SHALL run the REQ-8 verification after each merge as usual.
- On red, the driver SHALL bisect the batch: it splits the batch's PRs in
  half, retries each half as its own train commit, and recurses until a single
  PR fails red — that PR then gets the normal REQ-9 comment with cause `red`
  and REQ-10 memory. PRs in halves that went green land as usual.
- A conflict building the batch SHALL be reported against the first PR the
  three-way merge rejects.
- Every REQ-6, REQ-7, REQ-8 and REQ-9 guarantee SHALL hold per PR exactly as
  in the unbatched train; batching changes only how many PRs share a train
  commit, not any safety property.

#### Scenario: A batch goes green

- **WHEN** a train commit carrying three PRs' heads is green
- **THEN** all three PRs merge in order, each verified, and one CI run served
  three landings

#### Scenario: A batch goes red

- **WHEN** a train commit of four PRs is red and its second half is also red
- **THEN** the driver lands the first half's PRs, and the PR bisected to as
  the red one gets one `red` comment; the other half-2 PRs are retried on the
  next tick against the new base

#### Scenario: Batch of one

- **WHEN** `batch` is unset or 1
- **THEN** behaviour is identical to REQ-1 through REQ-16 as written

## Requirement: REQ-18 Sequence collisions

Two files that each add an ADR, a spec or a migration numbered the same merge
cleanly, because their paths differ, and the base branch ends up holding both.
The fix afterwards is a rename, which is a push, which costs a PR its
approvals and its CI run, so the train SHALL refuse such a train commit
instead of landing it (ADR-0052).

A file claims a sequence number with the first run of two or more digits in
its base name, scoped to the directory holding it, so an ADR and a migration
may share a number and a file whose name carries none is ignored. The watched
directories come from `sequence_paths` (REQ-1); an empty list disables the
check.

- The check SHALL run on the built train commit, before CI is asked to run on
  it, and SHALL read the watched directories at the train commit and at the
  base.
- A colliding pair the base branch already holds in full SHALL NOT be a
  verdict on the attempt: that duplicate is `main`'s own cleanup, and refusing
  every later attempt over it would stop the train forever.
- The attempt ends `sequence conflict`, and the driver SHALL NOT merge it. The
  PR that brings a colliding file gets the REQ-9 comment with cause
  `sequence-conflict`, naming every colliding file and which PR brings it, or
  that it is already on the base branch.
- In a batch the collision SHALL be bisected by halves like a red batch, so
  the PRs claiming distinct numbers land and the colliding one is isolated and
  commented.

#### Scenario: Two PRs claim one number

- **WHEN** one PR adds `docs/adrs/adr-0051-a.md` and another adds
  `docs/adrs/adr-0051-b.md`, and both are eligible for the same train commit
- **THEN** the attempt ends `sequence conflict`, neither lands, and each PR is
  told which two files claim 0051

#### Scenario: A claim already on the base branch

- **WHEN** the base branch already holds both `adr-0051-a.md` and a second
  file numbered 0051, and this attempt touches neither
- **THEN** the attempt is judged on CI alone, because the duplicate predates it

#### Scenario: A near miss

- **WHEN** one PR adds `adr-0051-a.md` and another adds `adr-0052-b.md`
- **THEN** nothing is refused, since no number is claimed twice

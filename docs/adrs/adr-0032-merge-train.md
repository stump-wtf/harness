---
status: proposed
date: 2026-09-23
decision-makers: [joestump]
related: [ADR-0005, ADR-0006, ADR-0008, ADR-0016]
---

# ADR-0032: A merge train — one deterministic merger per repo tests the exact tree that lands, and only it moves `main`

> **Not yet implemented.** Design stage for epic
> [stump.wtf/harness#540](https://gitea.stump.rocks/stump.wtf/harness/issues/540).
> SPEC-0025 (`docs/openspec/specs/merge-train/`) specifies the behaviour; the
> stories #597–#605 implement it.

## Context and Problem Statement

Our repos protect `main` with `block_on_outdated_branch`, merge by squash, and
forbid force-pushes and merging `main` into a branch. Gitea 1.27 has no merge
queue. So every merge strands every open sibling PR, and until 2026-09-23 the
only way back was a **replay** — a new PR and a new review:

- 34 of 104 PRs opened 09-20 → 09-22 were replays, 14 of them in this repo.
- A change that needed a replay took a median **50 h** to land; one that did
  not took **0.4 h**.
- 18 replays were opened by the predecessor's reviewer, which flipped the
  author and forced a fresh cross-identity review.

The block cannot simply be dropped (decision D1 of the agent-contention epic).
Gitea PR CI tests `refs/pull/N/head`, **not** the merge with `main`, so the
block is the only thing making "the tree CI tested" equal "the tree that
landed". In the window it caught zero integration failures, but it is the
guard against A and B each green while A+B is red.

Two things changed since the epic was written:

1. **The rebase update is allowed.** `POST /repos/{o}/{r}/pulls/{n}/update?style=rebase`
   replays a PR onto `main` server-side, CI then runs on that head, and under a
   squash merge that head is the tree that lands. That stops the replay churn
   on its own, at one CI run and one re-approval per PR per intervening merge.
2. **D3: LLM sessions never merge.** Reviewers approve; something
   deterministic, running as `joestump-agent`, merges.

So the train is no longer the only fix for churn. It is a **throughput and
governance** component: approved work lands without an agent (or a human)
babysitting it, in a predictable order, and every landed tree was tested as
exactly that tree.

How should a deterministic merger, running inside the Harness daemon, land
approved PRs so that the tested tree is the merged tree, without rewriting
anyone's branch, without an LLM in the loop, and without silently dropping a
PR?

## Decision Drivers

- **Tested tree == merged tree.** The property the block provides today must
  survive turning the block off.
- **No LLM merges** (D3). The merger is plain code with no model calls.
- **Nobody's branch is rewritten.** The PR head is read, never pushed to.
- **Loud failure.** A PR that cannot land is told so, once, with the reason; a
  failure the train cannot explain halts the train.
- **Testable offline.** Every forge interaction goes through one interface
  with an in-memory fake, so the whole state machine runs in `go test`.
- **Off by default** (ADR-0006): a capability that merges code ships disabled.
- **Credentials stay out of argv, logs and error strings** (ADR-0008).

## Considered Options

**A. How the train tree is built**

- **A1 — local git plumbing, pushed as a new `train/<pr>` branch** (chosen).
  Keep a bare cache clone per repo; `git merge-tree --write-tree <main> <head>`
  computes the merge without a work tree and exits 1 on conflict;
  `git commit-tree` makes one commit whose parent is `main`; push it to
  `refs/heads/train/<pr>`.
- **A2 — Gitea's contents API** (`POST /repos/{o}/{r}/contents` with
  `new_branch`), writing the PR's changed files onto `main`.
- **A3 — push the train commit to the PR branch** (the rebase update, done by
  the train).

**B. How the tested tree lands**

- **B1 — API squash merge, pinned on both sides, then verified by tree**
  (chosen).
- **B2 — fast-forward `main` to the train commit** by a direct push.

**C. Ordering**

- **C1 — earliest qualifying approval, then PR number** (chosen).
- **C2 — priority label, then approval.**
- **C3 — smallest diff first.**

**D. The per-repo singleton**

- **D1 — an exclusive, non-blocking `flock` on a per-repo lock file, held for
  the driver's lifetime** (chosen).
- **D2 — a mutex held by the daemon.**
- **D3 — a lock branch on the forge.**

**E. The author todo**

- **E1 — one PR comment that @mentions the author and carries a machine
  marker; Switchboard routes it to the author's queue** (chosen).
- **E2 — Harness writes the todo through a Switchboard client.**

## Decision Outcome

Chosen: **A1, B1, C1, D1, E1**.

### The loop

Per enabled repo, one driver goroutine. Each tick (default every 60 s):

1. List open PRs; keep those `Eligible` (not draft, CI green on the head,
   mergeable, approved by a non-author **on the current head**, no
   `REQUEST_CHANGES` on the current head); sort them with `Order`.
2. For the first PR not already attempted at this `(head, base)`:
   1. read `main`'s head → `base`;
   2. **build** `train/<pr>` = `base` + a squash of the PR head, as a new
      commit on a new branch;
   3. **wait** for CI on that exact commit, polling the combined status with
      backoff and a deadline;
   4. **delete** `train/<pr>`, whatever happened;
   5. on green: re-read the PR and `main`; if the PR's head, its eligibility,
      or `main` moved, skip (the next tick retries against the new state);
      otherwise **squash-merge** with the head pinned;
   6. **verify**: the merge commit's tree equals the train tree, `main`
      points at the merge commit, and every changed path's bytes on `main`
      equal the train's.
3. One PR per tick. After a merge, the next tick starts from the new `main`.

"Batching" in this ADR means the queue drains without a human between merges,
not that several PRs share one CI run. One PR per train keeps the tree-equality
argument simple and a red run attributable; combining PRs is a later, separate
decision (see *More Information*).

### Why tested == merged (A1 + B1)

The train commit's tree is `merge(base, head)`. CI runs on that commit. The
merge is performed only if, immediately before it, `main` is still `base` and
the PR head is still `head`; the head is additionally pinned in the merge call
itself (`head_commit_id`), so Gitea refuses the merge if it moved in between.
Gitea's squash merge then computes `merge(base, head)` with the same git merge
machinery (`ort`) and commits it.

That argument has two soft spots, and each is closed by **verification after
the fact, which halts the train**:

- Gitea's merge and ours could disagree (a git version or strategy difference).
- `main` could move in the milliseconds between the re-read and the merge (a
  human bypass, or a second train on another host — see D).

So after every merge the driver fetches the merge commit into its cache and
compares **real git tree ids**: `rev-parse <merged>^{tree}` against the train
tree. The Gitea REST API cannot be used for this — on this instance
`GET /git/commits/{sha}` returns the *commit* id in `commit.tree.sha`, and
`GET /git/trees/{sha}` echoes the commit id too (verified 2026-09-23 on
`6db13bd`, whose real tree is `679bb41`). A mismatch is an untested tree on
`main`: the driver halts, logs at error level, and comments on the PR. It is
detected within one merge and fixed by a revert, which the train then lands
like any other PR.

The content check of #601 (`VerifyLanded`) runs as well, against `main`, for
the PR's changed paths. It is weaker than tree equality and exists for the
failure the epic names: the `merged` flag says yes and the change is not on
`main`.

A2 was rejected because the contents API cannot express a file mode: a PR that
adds an executable script would be tested without its `+x`, which is exactly a
tested tree that differs from the landed one. It also cannot do a three-way
merge, so any file both sides touched would be a false conflict.

A3 was rejected because it rewrites the author's branch, which
`dismiss_stale_approvals` then answers by dismissing the approval the train is
acting on.

B2 was rejected: Gitea cannot mark a PR merged by a commit that is not its
head when the history is squashed, the bot would need push rights on `main`,
and every repo merges by squash for a linear history.

### Failure handling

| Event | Scope | What the driver does |
|---|---|---|
| merge conflict building the train | PR | comment + author todo; skip the PR until its head or `main` moves |
| red CI on the train commit | PR | comment + author todo; skip until head or `main` moves |
| CI does not finish by `ci_timeout` | PR | comment (cause `timeout`); skip until head or `main` moves |
| PR head, eligibility or `main` moved before the merge | PR | no comment; retry next tick |
| merge refused by the forge (4xx) | PR | comment (cause `merge refused`); skip until head or `main` moves |
| forge 5xx / network error | tick | no comment; log at warn; abandon the tick, retry next tick |
| tree or content verification fails after a merge | **train** | comment; log at error; **halt** the driver until restarted |
| `main` moved outside the train (bypass) | audit | log at warn with the new head; continue |

"Skip until head or `main` moves" is an in-memory record of attempted
`(pr, head, base)` triples, so a failed PR is not rebuilt every tick, but *is*
retried as soon as either side changes — which also gives a red flake a
second chance at the next merge.

**Never a replay.** No code path opens a pull request. A conflict is the one
case that needs a new tree, and the author makes it — by the rebase update if
Gitea can resolve it, or a local fix and a push.

**Never twice.** Before commenting, the driver lists the PR's comments and
does nothing if one already carries the marker for this head (below). The
dedupe lives on the forge, so it survives a daemon restart.

**Never a skipped red check.** Only `success` merges. `pending` with no
statuses at all is still pending — a commit CI never picked up does not pass
by being empty (Gitea reports `state: pending, total_count: 0` for a commit
with no statuses).

### Required checks on `train/*`

- The pipeline workflow triggers on `push` to `train/**` as well as `main`, so a
  train commit runs **the same `pipeline.yaml` a PR does**. Stages fenced to PRs
  (the AI review) or to `main` (the docs deploy) stay fenced.
- The driver requires the **combined status** of the train commit to be
  `success` with at least one status. Every job of the pipeline registers a
  pending status when the run is created, so a combined success means the
  whole run passed, not the first job to report.
- `train/*` gets a branch-protection rule whose push allowlist is
  `joestump-agent` only, so nobody else can create a train branch that looks
  tested.

Pinning an explicit list of required contexts (rather than "all that ran") is
an open question in SPEC-0025.

### Interaction with `dismiss_stale_approvals` — an invariant

**The train never pushes to a PR's head branch.** It reads the head, builds a
new commit on a new branch, and merges by API. Gitea dismisses approvals when
the head branch receives new commits; it does not dismiss them when the base
moves. So an approval survives the train merging other PRs ahead of it, and
the next tick finds the PR still eligible. If an approval *is* dismissed, a
human or an author pushed, and the PR correctly drops out of the queue until
it is re-approved on the new head.

### Singleton (D1)

A driver takes an exclusive, non-blocking `flock` on
`$XDG_STATE_HOME/harness/mergetrain/<owner>_<repo>.lock` and holds it for its
lifetime. A second driver for the same repo — in the same daemon or another
daemon on the host — fails fast with `another merge train holds <repo>`. It is
refused, never raced. The lock dies with the process, so a crash leaves no
stale lock.

D2 was rejected because it does not cover two daemons on one host (a service
unit and a foreground `harness daemon` is the common way to get two). D3 was
rejected for v1: a forge lock branch needs a lease and a reaper to survive a
crash, and Gitea gives no atomic compare-and-set on a branch's age.

Cross-host exclusion is operational: the train is enabled in one daemon's
config only (the `joestump-agent` daemon). The base re-check and the tree
verification above bound the damage if that is ever violated: at most one
untested tree lands, and the train halts.

### Ordering (C1)

Earliest **qualifying** approval first — the first `APPROVED` review by a
non-author on the current head — then PR number. It is first-come,
first-served on the only signal that means "a reviewer is done", it is
deterministic, and it cannot be gamed by the author.

C2 was rejected because every label we have is self-asserted by whoever opens
the PR. C3 starves large PRs, and they are the ones that have waited longest.

### Author todo (E1)

The failure comment @mentions the author and ends with a marker:

```
<!-- harness-mergetrain v1 pr=123 head=<sha> cause=red -->
```

The @mention is a Gitea notification. The comment's `issue_comment` webhook
reaches Switchboard, where a routing rule matching the marker makes one
`author` todo. Harness writes no todo itself.

E2 was rejected: Harness holds no Switchboard write credential and ADR-0008
keeps it that way; the forge comment is also the dedupe record, so a second
channel would be a second source of truth.

### Relationship to the rebase update

Both stay. On a repo with the train enabled the **train lands everything**;
reviewers approve and stop. The rebase update is the author's per-PR tool:
to resolve a conflict the train reported, and on repos without a train. While
`block_on_outdated_branch` is still on (the pilot), the forge refuses to merge
an outdated PR, so a train in `merge` mode could only land PRs that are
already current; that is why the pilot runs in `report` mode and the cutover
turns the block off in the same change that turns the train on (#605).

### Emergency bypass

If the train is down and something must land, a human with merge rights on
the repo:

1. runs the rebase update on the PR, so CI tests the tree that will land;
2. merges it by hand once CI is green;
3. comments `mergetrain-bypass: <reason>` on the PR.

The driver logs every observed `main` head it did not produce (`bypass
detected`, with the new head), so a bypass without the comment is still found
by grepping the daemon log. The kill switch is `[mergetrain] enabled = false`
and a daemon restart; `mode = "report"` stops merges but keeps building and
testing trains.

### Consequences

- Good: approved work lands with no agent or human in the loop, in a
  predictable order, and every landed tree was CI-tested as that tree.
- Good: turning off `block_on_outdated_branch` no longer costs the tested ==
  merged guarantee, so replays end on repos with the train.
- Good: the only thing that merges is a few hundred lines of Go with a fake
  forge under test.
- Bad: one CI run per PR, serialised. A repo with a 10-minute pipeline lands
  at most ~6 PRs an hour. Batching is the fix if that bites.
- Bad: the daemon now shells out to `git` and holds a bare clone per repo.
- Bad: a verification failure means an untested tree already reached `main`.
  It is detected, never silent, and bounded to one merge.
- Neutral: the train needs a forge token with write access; it is read from
  an environment variable named in config, never stored in `harness.toml`.

### Confirmation

- SPEC-0025's scenarios run in `go test ./internal/mergetrain/...` against the
  fake forge: order, the failure table above, the head-moved skip, the
  single comment per head, the singleton, and the verification halt.
- The pilot (#605) runs the train in `report` mode on this repo and posts the
  trains built, their CI results and the verification outcomes on the issue.
- After cutover, the #540 acceptance bar is measured over a week: replays
  under 3 % of PRs, zero untested trees merged, no merge by an LLM session,
  `main` red only from flakes.

## Pros and Cons of the Options

### A1 — git plumbing, new branch (chosen)

- Good: a real three-way merge, the same one Gitea performs; file modes,
  symlinks and binaries are exact.
- Good: `merge-tree --write-tree` needs no work tree and reports conflicts by
  exit code.
- Bad: needs `git` ≥ 2.38 on the daemon host, and disk for a bare clone.

### A2 — contents API

- Good: HTTP only.
- Bad: loses file modes; no three-way merge.

### A3 — push to the PR branch

- Bad: rewrites the author's branch and dismisses the approval.

### B1 — pinned API squash, then verify (chosen)

- Good: the forge records the merge normally; the PR is marked merged.
- Bad: tested == merged is argued, then checked; not guaranteed by identity.

### B2 — fast-forward `main`

- Good: the landed commit *is* the tested commit.
- Bad: bot push rights on `main`; breaks the PR's merged state under squash.

### C1 (chosen), C2, C3 — see *Ordering*.

### D1 (chosen), D2, D3 — see *Singleton*.

### E1 (chosen), E2 — see *Author todo*.

## Architecture Diagram

```
                ┌──────────────────── harness daemon ───────────────────┐
                │  driver (one per repo, flock-held)                     │
                │    Eligible → Order → Train → merge → Verify           │
                │                  │                                     │
                │            forge.Forge  ◀── fake (tests)               │
                │                  │                                     │
                │          forge/gitea: REST + git (bare cache clone)    │
                └──────────────────┬─────────────────────────────────────┘
                                   │ token from env (joestump-agent)
                  ┌────────────────▼───────────────┐
                  │ Gitea                           │
                  │  push train/<pr> ─▶ pipeline    │── combined status
                  │  squash-merge PR (head pinned)  │
                  │  issue_comment ─▶ Switchboard ──┼─▶ author todo
                  └─────────────────────────────────┘
```

## More Information

- Epic: https://gitea.stump.rocks/stump.wtf/harness/issues/540; contention
  epic and decisions D1–D5:
  https://gitea.stump.rocks/stumpcloud/stumpcloud/issues/466.
- SPEC-0025 (`docs/openspec/specs/merge-train/spec.md`) is the behaviour;
  its design companion records the package layout and the `Forge` interface,
  including where it departs from the interface first sketched in #599.
- Later, not decided here: batching several PRs into one train (bisect on
  red), required-context pinning, enabling the train on a second repo — each
  on its own numbers, per #605.

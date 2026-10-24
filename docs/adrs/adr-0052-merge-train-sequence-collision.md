---
status: proposed
date: 2026-10-24
decision-makers: []
extends: [ADR-0032]
related: [SPEC-0025, ADR-0032]
---

# ADR-0052: the merge train refuses a train commit where two files claim one sequence number

> **Not yet accepted.** Proposed in the PR closing stump.wtf/harness#981. The
> issue asks for detection and settled the what ("fail the attempt, name both
> PRs"); it settled nothing about where the check runs, what counts as a claim,
> or what the train does with a duplicate that is already on `main`. This ADR
> is those three decisions.

## Context and Problem Statement

ADR, spec and migration files carry their identity in a number in the file
name, and the train's safety story does not cover that number. Two PRs that
each add a new ADR numbered 0050 touch different paths, so the three-way merge
is clean, CI runs on a tree that contains both, and the train lands it. The
base branch then holds two ADR-0050 files, which is not a state anything else
in the pipeline notices.

The manual fix is a rename, and a rename is a push: it moves the head, which
dismisses approvals and re-runs CI on a PR that was approved and green. So the
renumbering happens by hand at the wrong end of the process, and it recurs:
harness#872 renumbered an ADR after two PRs both claimed ADR-0040, and
stump.wtf/switchboard #596, #599, #600 and #607 walked 0048 to 0051 over three
days in October 2026, with #600 colliding on a migration number as well.

## Decision

**The train checks the train commit, not the pull request.** A collision is a
property of the tree that would become the base branch: neither head alone is
wrong. The check therefore runs in `RunTrainBatch` on the commit
`CreateTrainBranch` produced, before CI is asked to run on it, so an attempt
doomed by a duplicate costs no CI run. This keeps ADR-0032's rule that the
train judges exactly the tree it would land.

**A claim is the first run of two or more digits in the file's base name,
scoped to its directory.** ADRs (`adr-0051-…`), specs (`SPEC-0025-…`) and
migrations (`0032_…`) share nothing but that shape, so one rule covers all
three without a per-repo table. Scoping to the directory keeps an ADR and a
migration numbered 0050 from colliding with each other, and the two-digit
floor keeps a lone version suffix from being read as a claim while a number
written without its zero padding still counts as one. The
watched directories are configuration (`sequence_paths`, default `docs/adrs/`,
`docs/openspec/specs/` and `migrations/`), because which directories carry
numbered records is a property of the repo, not of the train.

**A duplicate already on the base branch is not this attempt's fault.** The
check compares the watched directories at the train commit against the same
directories at the base. A pair the base already holds in full is reported
nowhere and blocks nothing: the alternative is a train that refuses every
future attempt over a cleanup someone owes `main`, which turns a small
duplicate into a stopped train. Only a collision the attempt introduces is a
verdict, and the file an author can rename is the one the attempt brings.

**The verdict is a new outcome, `sequence conflict`, with cause
`sequence-conflict`.** It is neither `conflict` (nothing failed to merge) nor
`red` (CI was never run), and a distinct cause is what lets the author's tool
tell "rebase" from "rename". The comment names every colliding path and, per
path, the PR that brings it or that it is already on the base branch, which is
the one sentence that makes the fix obvious.

**In a batch the collision bisects like a red batch.** Halving is already how
the driver narrows one bad PR out of a batch, and it is correct here too: the
half without a collision lands, and once the two claimants are on separate
train commits the second one is refused against the new base, so the duplicate
still cannot land.

## Consequences

- Reading a watched directory is a new Forge method, `TreePaths`, because the
  other claimant usually is not in the diff at all. The gitea client answers it
  from the cache clone with `git ls-tree`; nothing new is fetched on the
  success path, and per-head reads happen only once a collision is found.
- The train now refuses some commits it used to land, so an ADR that sat
  approved through a collision now gets a comment instead of landing a
  duplicate. That is the point of the change, and the comment says what to do.
- Renumbering moves to the front of the process, where it costs a push before
  anyone reviews, rather than to the end where it costs an approval.

## Rejected options

- **Renumber at land time, with `ADR-XXXX` placeholders.** Deterministic, since
  the train is serial per repo, and it removes the chore entirely. Rejected for
  this change: rewriting a file and its cross-references inside a merge commit
  breaks tested-equals-merged (ADR-0032 option A1), which exists to keep a
  tested tree from ever differing from a landed one. The issue lists it as a
  stretch goal to be proposed on its own.
- **A content conflict forced by writing both files to one path.** Would make
  git refuse the merge with no new code, and would destroy one ADR to save the
  other from a rename.
- **Warn and land.** Keeps the train moving, and leaves the base branch with
  two ADR-0050 files and no record of which one anyone meant.

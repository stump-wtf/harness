---
status: proposed
date: 2026-11-03
decision-makers: []
extends: [ADR-0032]
related: [ADR-0008, ADR-0032]
governs: [SPEC-0025]
---

# ADR-0061: The merge train grows a GitHub backend, and gates CI through a draft pull request

> **Not yet accepted.** Proposed in the PR for stump.wtf/harness#906 by the
> handoff lane. ADR-0032 chose Gitea and called a GitHub backend a non-goal,
> because Gitea is where our CI gates merges. The issue asks for the backend
> anyway, so that a repo which lives on GitHub can be landed by the same train
> instead of by a session that merges by hand. This records the two decisions
> the issue could not make for itself — what "CI is green" means on GitHub, and
> how a push to a `train/**` branch makes Actions run at all — plus the config
> shape that keeps one train serving two forges.

## Context and Problem Statement

`internal/forge.Forge` is the only door the train opens to a forge: ten methods
that list pull requests, build a train branch, read CI, land, and comment. The
Gitea implementation is complete and the in-memory fake covers the state
machine, so a second backend is a new package rather than a redesign — except
where GitHub's semantics differ from Gitea's in ways that decide whether the
train is safe:

1. **CI lives in a different place.** Actions publish *check runs*. Gitea
   Actions publishes combined statuses, which is what `CombinedStatus` reads
   today, and GitHub's combined-status endpoint answers `success` with
   `total_count == 0` for a commit no workflow has touched — a commit whose CI
   has not started, or will never start. A GitHub backend that read that
   endpoint would land untested trees on the exact repos the train exists to
   protect.
2. **A pushed branch triggers nothing.** The train's verdict comes from CI on a
   commit that exists only on the forge, on branch `train/<pr>`. On Gitea a
   workflow with `on: push` and `branches: train/**` runs for it. On GitHub a
   `pull_request`-triggered workflow — the common shape, and the only shape in a
   repo whose workflows the train's identity cannot edit — runs when a pull
   request is opened or synchronised, not when a branch is pushed. Pushing the
   train branch and polling it would poll a commit with no checks forever.
3. **Two forges, one table.** `[mergetrain]` today names one
   `forge_base_url` and one `forge_token_env`. A daemon that serves
   `stump.wtf/harness` on Gitea and a GitHub repo must know which forge each
   repo lives on, and must keep each forge's token in its own variable.

The issue also fixed one constraint: on a repo whose workflows the train cannot
edit (the Crush fork, named in the thread), the backend may not add a workflow
file to make CI fire. The gate has to come from the train's own writes.

## Decision

Add `internal/forge/github`, implementing the whole `Forge` interface, and keep
the interface unchanged apart from one field.

### 1. Gate on check runs, and treat "nothing has reported" as pending

`CombinedStatus` on GitHub folds two endpoints: `GET /repos/{r}/commits/{sha}/check-runs`
(Actions, the source of truth) and `GET /repos/{r}/commits/{sha}/status`
(legacy statuses, for a repo still using them). The fold is:

- any check run still in progress → `pending`;
- **no check runs and no statuses at all → `pending`**, never `success`. This is
  the case the combined-status endpoint gets wrong;
- `completed` with conclusion `action_required`, `timed_out`, `failure`,
  `stale`, `startup_failure` or `cancelled` → `failure`;
- a conclusion this code does not know → `pending`.

`CommitJobs` reports a failed check run that carries no job objects as
"never reached a runner", which is ADR-0056's flake signature and works
unchanged on Actions.

### 2. Gate the train commit with a **draft pull request**, by default

`CreateTrainBranch` builds and pushes `train/<pr>` with the shared git plumbing,
then — for `github_ci_gate = "draft-pr"`, the default — opens that branch as a
draft pull request against `base_branch`. Opening it is what runs the repo's
`pull_request` workflows against the exact train commit. The gate is not a
proposal:

- it is `draft = true`, so nothing can merge it and no reviewer is requested;
- its body says it exists to run CI and that nothing in it needs reviewing;
- eligibility already ignores drafts, so the train never lands or re-orders its
  own gate;
- `DeleteBranch` closes it and deletes the ref in one stateless step (list open
  PRs whose head is `<owner>:<branch>`, close, delete the ref), so an attempt
  cleans up after itself and a daemon restart mid-attempt leaves no orphan PR
  and needs no stored state.

For a repo that would rather gate the push itself, `github_ci_gate =
"train-push"` opens nothing: the repo's own workflows declare `train/**`, and
the backend just pushes. The choice is per daemon, not per repo, because today
a daemon's GitHub repos share one workflow convention; making it per repo is a
later widening of the same key.

The alternative the thread rejected was adding a `.github/workflows/` file that
triggers on `train/**`. That works, and is what `train-push` is for, but it
asks the train's identity to edit the repo's CI definition — in the fork named
in the thread, an edit it cannot make and would not be allowed to land. The
draft gate needs no write the train does not already have, and it is visible in
the repo's PR list, which is where a reviewer can see what a merge was gated on.

### 3. Name the forge in the `repos` entry, keep the keys flat

A `repos` entry is `owner/name`, or `github:owner/name` to put that repo on the
GitHub forge. Each forge keeps its own credentials, and the train demands a
token only for a forge a repo actually uses:

| Key | Meaning |
|---|---|
| `forge_base_url` / `forge_token_env` | unchanged: the default forge (Gitea) |
| `github_base_url` | GitHub API base (default `https://api.github.com`; GHES overrides it) |
| `github_token_env` | NAME of the GitHub token's variable — required when any repo names `github:` |
| `github_ci_gate` | `draft-pr` (default) or `train-push` |

A `repos` list that is entirely GitHub needs neither `forge_base_url` nor
`forge_token_env`. A `token` key stays refused for both forges (ADR-0008), and
`base_branch` names the gate PR's base, so it is required when the gate is
`draft-pr`.

The alternative was a `[[mergetrain.forge]]` table with an arbitrary number of
named forges. It is a better shape for a third forge and a worse one for the
second: two forges, two token variables, and one prefix per repo is what the
issue asks to configure, and a registry table would put that indirection in
every existing config file's reading path. When a third forge is real, the
table replaces the flat keys in one migration.

### 4. Keep the git plumbing in one package

`internal/forge/gitcache` holds the content-addressed build ADR-0032 mandated:
the bare cache clone, the `refs/heads/* → refs/mergetrain/heads/*` fetch, the
`merge-tree` fold, the tree-id read, and the rule that the credential reaches
git only through the child's environment. Gitea's `git.go` is now its whoami
plus a call into it; GitHub reuses it with a Basic-auth header and
`https://github.com/<repo>.git`. GitHub publishes `refs/pull/<n>/head` exactly
as Gitea publishes `refs/pull/<n>/head`, so the freshness check the train needs
is the same code. Each forge gets its own cache directory
(`<state>/mergetrain/cache/<forge>/`), because the same `owner/name` can exist
on both forges and those are not the same repository.

### 5. Rate limits are transient, and the API version is pinned

A 403 whose body says rate limit is mapped to 429 so the driver's existing
transient handling backs off instead of failing the attempt, and every request
carries `Accept: application/vnd.github+json` and a pinned
`X-GitHub-Api-Version`, so an unannounced field change cannot silently alter
what the train reads as green.

## Consequences

- The train can land a GitHub-native repo with no changes to `internal/mergetrain`:
  eligibility, ordering, batching, verification, comments and the lock are
  untouched. `forge.TrainSpec` gains `BaseRef`, which a `pull_request`-gated
  forge needs to open its gate; the Gitea backend ignores it.
- **A mis-set gate fails closed.** If `github_ci_gate` does not match how a
  repo's workflows trigger, no check run appears, CI reads `pending`, and the
  train comments `timeout` after `ci_timeout`. Slow and loud, never green.
- **A GitHub repo's PR list gains drafts** while a train runs: one per attempt,
  on branch `train/<pr>`, closed by the same attempt. In `report` mode it is
  opened and closed for real, because that push and that PR *are* how the train
  gets a verdict; `report` still writes nothing to the PRs under test.
- Existing Gitea behaviour is byte-identical, and its clone cache moves to
  `cache/gitea/`, which costs each warm cache one re-clone after upgrade.
- The train's identity on GitHub commits as `<id>+<login>@users.noreply.github.com`,
  so the train commit's author maps to the account and not to a fake address.
- Nothing here tests itself against live GitHub: the unit tests fake the REST
  surface and run a real `git` against a local bare remote, and no test reaches
  the network. A live smoke run against a throwaway repo is still owed before a
  GitHub repo runs in `merge` mode.

## Considered Alternatives

- **Add a `train/**` workflow to each GitHub repo.** Chosen only as the opt-in
  `train-push` gate: it needs a write to the repo's CI definition, which the
  named repo forbids, and it leaves a train branch that gates nothing when the
  workflow is missing.
- **Poll the combined status.** One call, no PR artifact — and it is green for a
  commit no workflow has seen, which is the one failure the train cannot afford.
- **Wait for `mergeable_state == "clean"`** instead of reading checks. That
  field also folds branch protection and merge conflicts into one word, so it
  cannot tell "CI is red" from "the base moved", and the train's author comments
  depend on that difference.
- **A `[[mergetrain.forge]]` registry table.** Right for a third forge, wrong
  for the second; see Decision 3.
- **Per-repo `[mergetrain.repos.<name>]` tables.** Repeats every tuning key per
  repo to express one bit — which forge — and would silently fork the driver's
  config.

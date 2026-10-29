---
title: "Run the merge train"
sidebar_position: 9
---

# Run the merge train

The merge train lands approved pull requests one at a time — or, with
`batch` above 1, up to `batch` of them on one train commit (SPEC-0025
REQ-17). For each attempt it builds a `train/<pr>` branch (`main` plus a
squash of the PR, or of the batch's PRs folded in order), waits for CI on
that exact commit, squash-merges each PR, and then checks that the tree that
landed is the tree that was tested. Reviewers approve and stop there. The
train does the merging, and no LLM session merges anything. The design is
ADR-0032 and the behaviour is SPEC-0025.

This guide covers turning it on for one repository, the pilot that comes
first, the cutover, and how to back out.

## Before you enable it

1. **CI runs on train branches.** The repository's pipeline must run on a
   `push` to `train/**` as well as `main`. For this repo that is the
   `branches: [main, 'train/**']` line in `.gitea/workflows/pipeline.yaml`.
   Nothing else in the pipeline changes. The image push and the docs deploy
   are already limited to `main`.
2. **A token for the train's identity**, with write access to the repo, in
   the daemon's environment. It never goes in `harness.toml`.

## Pilot: report mode

Enable the train in `report` mode alongside the existing process, with
`block_on_outdated_branch` still on:

```toml
[mergetrain]
enabled = true
mode = "report"
repos = ["stump.wtf/harness"]
forge_base_url = "https://gitea.stump.rocks"
forge_token_env = "HARNESS_MERGETRAIN_TOKEN"
```

In `report` mode the train builds and tests trains exactly as it would for
real, but it writes nothing to any pull request: no merge and no comment.
Watch the daemon log for these lines:

| Line | Meaning |
|---|---|
| `merge train enabled` | the start, naming the mode, repos and forge identity |
| `mergetrain queue` | the eligible PRs, in merge order, whenever the queue changes |
| `mergetrain would merge` | a train went green, and in `merge` mode this PR would have landed |
| `mergetrain would comment` | a conflict, red CI or timeout the author would have been told about |
| `mergetrain bypass detected` | `main` moved without the train |
| `mergetrain flake retry` | a red train whose failed jobs never reached a runner: the same tree is tried once more after the cool-down, nothing said to the author yet |
| `mergetrain flake retry exhausted` | that retry failed with the same signature; the PR now gets its `red` comment |

Run it for a week and count trains built, green versus red, and the causes of
failures. The bar for going further is set by the agent-contention epic that
proposed the train, and by stump.wtf/harness#540:

- replays under 3% of PRs;
- no untested tree merged;
- no merge made by an LLM session;
- `main` red only from flakes.

## Cutover

In one change, and for one repository only:

1. Set `mode = "merge"` and restart the daemon.
2. Turn off `block_on_outdated_branch` for that repo's `main` rule:

   ```sh
   tea api --login gitea.stump.rocks -X PATCH \
     repos/stump.wtf/harness/branch_protections/main \
     -F block_on_outdated_branch=false
   ```

While the block is still on, the forge refuses to merge any PR that is behind
`main`, so a train in `merge` mode would get `merge-refused` for nearly every
PR. That is why the two changes go together. The train deliberately does not
work around the block by running the forge's "update branch" on refused PRs:
an update creates a new head SHA, which orphans every existing approval (the
PR becomes `no approval on current head` and stale approvals are dismissed),
so the PR would loop out of the queue until a reviewer re-approved it — while
rewriting the author's branch as a side effect. The stuck-PR path is instead
the one-comment author todo, and the block comes off at cutover because the
train's tested-tree guarantee is exactly the guarantee the block was
providing. The reasoning is written up in ADR-0032, *Relationship to the
rebase update*.

### Lesson (b): reviewer must not rebase a merely-behind branch with the block off

With `block_on_outdated_branch=false`, a reviewer who force-rebases a PR that
is merely behind `main` causes the approval to be dismissed — the PR becomes
unstale, but the approval is no longer attached to its new head. Rebase only
to resolve conflicts; after conflicts are resolved, the PR should be left as
is. The train's own `update branch` style merges the base into the PR (not a
rebase) to keep approvals attached.

### Lesson (c): a harness redeploy kills scratchpads and cancels a train in flight

When the train is merging and a harness redeploy starts, scratchpad workers
are killed and in-flight train runs are cancelled by context. The daemon starts
an empty queue on restart; does not resume where it left off. A halted queue is
not a resumed run — the only recovery to a red CI after a cut-over is a fresh
deploy and redeploy.

## Rollback

Put the block back:

```sh
tea api --login gitea.stump.rocks -X PATCH \
  repos/stump.wtf/harness/branch_protections/main \
  -F block_on_outdated_branch=true
```

Then set `mode = "report"` (or `enabled = false`) and restart the daemon.
Check that the block really is on again by reading the rule back rather than
trusting the PATCH's exit code (`tea api` exits 0 even on an error):

```sh
tea api --login gitea.stump.rocks \
  repos/stump.wtf/harness/branch_protections/main \
  | jq .block_on_outdated_branch
```

Roll back if any of these happen:

- the daemon log shows `mergetrain halted`. That means a merge landed a tree
  other than the one CI tested. Revert that merge too.
- `main` goes red on a commit the train merged and a rerun of the same SHA is
  still red. That is a real integration failure the train should have caught.
- a merge on `main` has no `Merge-Train: tested as …` line and no
  `mergetrain-bypass:` comment on its PR.

## When the train halts

A halt means verification failed after a merge. Either `main` was not at the
merge commit, the merge commit's tree was not the tested tree, or a changed
file's bytes did not land. The PR gets a `verify-failed` comment and the
driver attempts nothing more until the daemon restarts. Find out why before
restarting. The daemon log's `mergetrain halted` line names the PR and both
trees.

## Landing something with the train down

1. Run the forge's rebase update on the PR, so CI tests the tree that will
   land.
2. Merge it by hand once CI is green.
3. Comment `mergetrain-bypass: <reason>` on the PR.

The train logs `bypass detected` for any `main` head it did not produce, so a
bypass that skipped step 3 still shows up in the daemon log.

## When the forge refuses a merge

A refused squash merge gets one `merge-refused` comment naming the PR's author,
and the `(pr, head, base)` triple is remembered, so nothing is attempted at that
head again until the head or base moves. Most refusals are a PR that stopped
being eligible mid-flight, and the comment quotes the forge's own answer.

The refusal worth reading twice is the approvals one: `Does not have enough
approvals`. The train only queues a PR whose approval the forge still counts, so
this answer means the approval went unofficial between that check and the merge.
Gitea clears the official mark on a reviewer's earlier reviews whenever review is
requested from them, so a review lane that re-requests review after posting its
review deletes its own approval; nothing about the change changed, and every
re-verification of it is wasted work. The cure is a fresh `APPROVED` review from
someone with approval rights. Because the triple is already remembered, the
approval alone will not re-trigger the merge: push a commit, or wait for the base
branch to move.

A `stale` line whose cause is `no longer eligible: no approval on current head`
and which carries a `note` about an unofficial approval is the same state caught
before the doomed merge attempt, and needs the same cure.

## When the runner pool flakes

A job that never attaches to a runner fails CI with no log at all, and the
combined status cannot tell that apart from a failing test. When a train goes red
and every failed job on that commit has no log, the train logs `flake retry` and
re-runs the same tree once after `flake_cool_down` instead of
waiting for `main` or the head to move. The retry is off until
`flake_cool_down` is set (the shipped default is `0` until ADR-0056 is
accepted; the proposed value is 5 minutes), which is the difference between losing
five minutes on a congested pool and losing the afternoon (#1016).

Read these lines together with the forge's own run page:

- `flake retry` once, then `green`: the pool was busy and the tree was fine.
  Nothing to do.
- `flake retry` then `flake retry exhausted`, and a `red` comment naming the
  retry: the signature persisted. That is a host question, not the PR's: several
  jobs dying this way across different runners at once is an act_runner
  capacity or scheduling fault, and the train will not retry the same tree again
  to work around it. Fix the runner, then push a commit or wait for `main`.
- a `red` comment with no `flake retry` before it: at least one failed job had a
  log, so this is a real failure and the train is right to stop.

## Known limits

- **One attempt per (head, base).** The driver merges each PR once per (head,
  base) pair and records the result (success, would merge, merge refused,
  verified, etc.). A red attempt is retried after the head or base
  moves, and once more on its own if every failed job reported no runner
  (SPEC-0025 REQ-18; a new merge attempt is then initiated, or the PR needs
  reapproval on a
  new head). Each head that reaches verification generates one author-todo
  comment on the PR (even after merge).
- **Singleton per host.** Enable the train in exactly one daemon's config.
  Two daemons on different hosts would race. The re-check before each merge
  and the tree verification after it limit the damage to one merge and a
  halt, but they don't prevent the race.
- **The train runs the PR's own pipeline.** The train commit contains the
  PR's changes, including any change to `.gitea/workflows/`. That is the
  same trust PR CI already extends to a same-repo branch, and review is what
  covers it.
- **No `train/*` branch-protection rule.** A `train/*` rule whose push
  allowlist holds only the train's identity would stop anyone with write
  access from pushing a `train/<pr>` branch, but on Gitea 1.27 it also
  stops the train: the pre-receive hook refuses deletion of a protected
  branch before it checks any allowlist, and the train deletes
  `train/<pr>` through the REST API after every attempt. With the rule in
  place, every attempt leaves its branch behind, the next attempt's push
  onto the leftover is refused as non-fast-forward, and every later tick
  for that PR fails (stump.wtf/harness#821). That cost buys little: the
  train never trusts a branch tip. It waits on the combined status of the
  SHA it built itself and pins `head_commit_id` on the merge, so a foreign
  push to `train/<pr>` cannot make an untested tree look tested; the rule
  would only protect humans reading branch names. The #605 pilot runs
  without the rule for this reason.

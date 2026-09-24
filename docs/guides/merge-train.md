---
title: "Run the merge train"
sidebar_position: 9
---

# Run the merge train

The merge train lands approved pull requests one at a time. For each one it
builds a `train/<pr>` branch (`main` plus a squash of the PR), waits for CI on
that exact commit, squash-merges the PR, and then checks that the tree that
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
2. **Only the train can create train branches.** On the forge, add a
   branch-protection rule for `train/*` whose push allowlist contains only
   the train's identity (`joestump-agent`). Without it, anyone with write
   access could push a `train/<pr>` branch that looks tested.
3. **A token for the train's identity**, with write access to the repo, in
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

Run it for a week and count trains built, green versus red, and the causes of
failures. The bar for going further is in stumpcloud/stumpcloud#466 and
stump.wtf/harness#540:

- replays under 3% of PRs;
- no untested tree merged;
- no merge made by an LLM session;
- `main` red only from flakes.

## Cutover

In one change, and for one repository only:

1. Set `mode = "merge"` and restart the daemon.
2. Turn off `block_on_outdated_branch` for that repo's `main` rule:

   ```sh
   curl -sS -X PATCH \
     -H "Authorization: token $GITEA_TOKEN" -H 'Content-Type: application/json' \
     https://gitea.stump.rocks/api/v1/repos/stump.wtf/harness/branch_protections/main \
     -d '{"block_on_outdated_branch": false}'
   ```

While the block is still on, the forge refuses to merge any PR that is behind
`main`, so a train in `merge` mode would get `merge-refused` for nearly every
PR. That is why the two changes go together.

## Rollback

Put the block back:

```sh
curl -sS -X PATCH \
  -H "Authorization: token $GITEA_TOKEN" -H 'Content-Type: application/json' \
  https://gitea.stump.rocks/api/v1/repos/stump.wtf/harness/branch_protections/main \
  -d '{"block_on_outdated_branch": true}'
```

Then set `mode = "report"` (or `enabled = false`) and restart the daemon.
Check that the block really is on again by reading the rule back rather than
trusting the PATCH's exit code:

```sh
curl -sS -H "Authorization: token $GITEA_TOKEN" \
  https://gitea.stump.rocks/api/v1/repos/stump.wtf/harness/branch_protections/main \
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

## Known limits

- **One PR per train.** A 10-minute pipeline lands at most about six PRs an
  hour. Batching is a later decision.
- **Singleton per host.** Enable the train in exactly one daemon's config.
  Two daemons on different hosts would race. The re-check before each merge
  and the tree verification after it limit the damage to one merge and a
  halt, but they don't prevent the race.
- **The train runs the PR's own pipeline.** The train commit contains the
  PR's changes, including any change to `.gitea/workflows/`. That is the
  same trust PR CI already extends to a same-repo branch, and review is what
  covers it.

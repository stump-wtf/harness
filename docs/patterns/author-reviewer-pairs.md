---
title: "Author/reviewer pairs on different model families"
sidebar_label: "Author/reviewer pairs"
sidebar_position: 1
---

# Author/reviewer pairs on different model families

One agent writes a change; a second agent, built on a **different model family**,
reviews it. The reviewer fixes what is cheap to fix, hands anything structural
back to the author, and never merges its own work. A self-hosting team built this
by hand inside their coordinator, with an identity map, a no-self-merge rule and
a reviewer charter. The stack carries most of it for you.

## Why a different family

Correlated blind spots. A reviewer from the same model family tends to approve
what its author got wrong, because it would have written the same thing. Pair,
for example, a [Crush](https://github.com/charmbracelet/crush) author on one
provider with a [Claude Code](https://claude.com/claude-code) reviewer.

Today, "different family" is whatever each harness's own model configuration
says: `model` on a one-shot, the client's own provider config on a resident
session. Fail-closed model pinning, which refuses to run when the model that
actually answered isn't the one configured, is **coming**
([ADR-0026](/decisions/adr-0026-fail-closed-model-pinning)). With it, the pairing
is enforced rather than intended.

## Harness: two identities, two harnesses

Run the author and the reviewer as separate harnesses. Each has **its own
`env_file`**, holding **its own forge token** (a different forge account for each,
so the forge can tell them apart) and **its own Switchboard endpoint token**. A
resident author and a triggered reviewer is a common shape:

```toml
# The author: a resident Crush session, woken by its own endpoint's doorbells.
[harness.author]
harness = "crush"
args = ["--yolo", "--channels", "server:switchboard"]
workdir = "~/agents/author"
env_file = "~/.config/harness/env/author.env"      # the author's forge token + endpoint token
restart = "on-failure"
restart_delay = 30
enabled = true

# The reviewer: a Claude Code one-shot, fired by review requests on its own endpoint.
[channel.reviewer]
url = "https://switchboard.example.com/mcp/reviewer-k3x9"
env_file = "~/.config/harness/env/reviewer-channel.env"
headers = { Authorization = "Bearer ${SB_TOKEN}" }

[harness.reviewer]
harness = "claude-code"
prompt_file = "~/agents/reviewer/REVIEW.md"        # the charter below
model = "claude-opus-5"
auto_accept = true
workdir = "~/agents/reviewer"
env_file = "~/.config/harness/env/reviewer.env"    # the reviewer's forge token + endpoint token
triggers = ["channel.reviewer"]
schedule = "@every 2h"                             # safety net for a missed doorbell
timeout = "45m"
```

Keep the two `env_file`s apart. A reviewer that can read the author's forge token
can approve as the author, and every guard below assumes it can't.

## Switchboard: route reviews to the reviewer, never to the author

Give each identity **its own forge webhook** into Switchboard, and put these rules
on the reviewer's. They drop review requests meant for anyone else, and any trigger
to review a pull request the reviewer itself authored. Everything else reaches the
webhook's queue:

```json
{
  "rules": [
    {"id": "review-request-not-for-me", "name": "review requests for anyone other than params.identity",
     "expr": ".kind == \"pull_request\" and ((.payload.action // \"\") | IN(\"review_requested\", \"review_request_removed\")) and ((($params.identity // \"\") == \"\") or ((.payload.requested_reviewer.login // \"\") != $params.identity))",
     "action": {"drop": true}},
    {"id": "own-pr-review-trigger", "name": "never review a pull request params.identity authored",
     "expr": ".kind == \"pull_request\" and ($params.identity // \"\") != \"\" and ((.payload.pull_request.user.login // \"\") == $params.identity) and ((.payload.action // \"\") | IN(\"opened\", \"reopened\", \"synchronized\", \"synchronize\", \"edited\", \"ready_for_review\", \"review_requested\"))",
     "action": {"drop": true}}
  ],
  "params": {"identity": "reviewer-bot"}
}
```

These are the routing cookbook's
[review recipe](https://switchboard.stump.wtf/docs/guides/routing-cookbook#send-review-requests-only-to-the-requested-reviewer).
A pool of reviewers on one webhook uses the cookbook's `pool-review` pack instead.
Installing a pack as a one-command preset is **coming**
([Switchboard ADR-0036](https://switchboard.stump.wtf/docs/decisions/ADR-0036-rule-packs-as-installable-presets));
until then, apply the rules with `set_webhook_rules`, after checking them against
real deliveries with `test_webhook_rules`.

:::caution Match `.kind` against what the forge actually sends

`.kind` is the `X-GitHub-Event` / `X-Gitea-Event` header, verbatim. Gitea puts the
review *outcome* there (`pull_request_approved`, `pull_request_rejected`,
`pull_request_comment`) and a sub-type in `X-Gitea-Event-Type`. A rule written
against the sub-type matches nothing, and a rule that matches nothing looks
exactly like one that is working. Dry-run every rule against stored deliveries
before you trust it
([the header table](https://switchboard.stump.wtf/docs/guides/handoff-lanes#optional-cut-a-pool-off-from-its-own-prs-review-feedback)).

:::

## The forge: the merge gate lives here

Routing decides who is woken, not what they may do once awake. Put the hard rules
in the forge, where no agent's judgment can get around them:

- **Branch protection requires one approval** on the default branch.
- **An author can't approve their own pull request.** Forges refuse it; separate
  identities make that refusal apply.
- **Stale approvals are dismissed** when new commits are pushed.
- **The reviewer arms auto-merge only after approving a finished PR.** An
  auto-merge armed before a late push merges the SHA that was armed, and silently
  drops the push. After any push to a PR you thought had merged, check the change
  reached the default branch by reading the file, not the PR's `merged` flag.

## The reviewer's charter

Put it in the reviewer's prompt (`REVIEW.md` above):

- **Fix** nits, failing tests and missing coverage yourself, as separate commits
  on the author's branch, then leave one summary comment saying what you changed.
- **Defer** anything architectural to the author, as a review comment. Don't
  redesign someone else's change.
- **Never merge your own PR**, and never approve one you pushed fixes to without
  reading the result.

## Claiming: one review, one reviewer

A review request can reach several live sessions of the same identity, such as a
triggered one-shot and an interactive session someone left open. Take the work
with `claim_next`, which hands each todo to exactly one caller, and re-read the
PR's state (open, merged, head SHA) before anything expensive, because the PR
may have moved on while the todo waited.

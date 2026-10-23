---
status: proposed
date: 2026-10-23
decision-makers: [joestump]
extends: [ADR-0032]
related: [SPEC-0025, ADR-0032, ADR-0049]
---

# ADR-0050: an approval qualifies only while the forge marks it official

> **Not yet accepted.** Proposed in the PR that closes stump.wtf/harness#977;
> no maintainer decision exists yet. The issue records the failure
> and proposes two fixes; what it does not settle is what "a qualifying
> approval" means once the forge holds an `APPROVED` review that counts toward
> nothing. This ADR is that settlement.

## Context and Problem Statement

On 2026-10-22 the train refused its squash merge of stump.wtf/switchboard PR
#608 with `HTTP 405: Does not have enough approvals`, at head `e8f6cb6d`. The
PR looked approved, green and up to date to every reader, and it was right about
two of those three.

The forge's own records explain the third. Review `4854` is an `APPROVED` review
by the reviewer identity on exactly that head, left at 22:15:01Z. At 22:18:33Z
that same lane posted a re-verification `COMMENT`, and at 22:18:42Z it requested
review again, from itself. The review now reports `official: false`, with
`dismissed: false` and `stale: false`: the approval is still there, and it no
longer counts. When the train merged at 22:19:53Z the forge counted zero
approvals and refused. Sibling lanes re-verified the PR three times over the
next hour without the head or the base moving, which changed nothing, and #608
landed the following morning only once the recorded state had been cleared by
other traffic.

SPEC-0025 REQ-2 makes eligibility turn on "at least one qualifying approval",
and `internal/mergetrain` reads qualifying as `APPROVED`, not the author, on the
current head. That reading is now provably narrower than the forge's: the forge
counts an approval only while it marks the review official, and it clears that
mark when review is requested from the reviewer who left it. So a PR can satisfy
the train's check and fail the merge on approvals with no change to the code
under review, and the train's one comment ("the forge refused the squash
merge") points every reader at the wrong work.

Two things follow, and neither is settled by the existing records: what the
train should require of an approval, and what it should say when the forge
answers with the approvals refusal anyway.

## Decision

1. `forge.Review` carries `Official *bool`: whether the forge counts the review
   toward its approval requirement. The Gitea adapter reads the `official` field
   of the reviews it already fetches, so this costs no extra calls.
2. An approval the forge reports as not official never qualifies. An approval
   whose officiality the forge never reported does qualify: silence is not
   evidence, and a forge that does not answer the question must still be able
   to merge. The 405 handling below is the backstop for that case.
3. When a squash merge is refused and the response body is the approvals
   refusal, the merge-refused comment names the state and its cure: a fresh
   `APPROVED` review from someone with approval rights, plus the warning that
   because the `(pr, head, base)` triple is already remembered as refused
   (ADR-0032 REQ-10) the approval alone will not re-trigger the merge. The
   marker's `cause` stays `merge-refused`, so REQ-9's comment contract and the
   routing rules that read it are unchanged. A refusal whose body says anything
   else, including a 405 with no body, keeps today's wording.
4. The lane-side rule ships as guidance, not code: harness never calls a
   review-request endpoint (its only forge writes are `SquashMerge` and
   `Comment`), so it cannot order a review and a re-request it does not make.
   The built-in `reviewer` persona and the author/reviewer pattern now say
   plainly that a review post comes last, because a re-request posted after it
   deletes the approval.

## Alternatives rejected

- **A new marker cause** (`merge-refused-approvals`). It would let a routing
  rule wake a reviewer instead of the author, but it changes the machine-readable
  contract in SPEC-0025 REQ-9 and every rule built on it, to carry one sentence
  that fits in the comment body.
- **Blocking on unknown officiality.** Treating `official` absent as "does not
  count" would let the train refuse to merge a PR on a forge that never answers
  the question, and would make an unreadable field a silent merge stopper.
- **Commenting when a PR becomes ineligible for want of an official approval.**
  Ineligibility is logged, not commented (REQ-9), and the person to ping is a
  reviewer, not the author the comment addresses. The stale line's `note` field
  carries it instead, and the next tick queues the PR again the moment the
  approval is refreshed.
- **Dismissal-style handling**, mapping the review to `DISMISSED`. It would
  assert a state the forge did not report and hide the distinction between "the
  approval was withdrawn" and "the approval was de-officialized by a request",
  which is the distinction that tells a reviewer what to do.

## Consequences

- The train no longer attempts a merge whose deciding approval the forge has
  stopped counting, so the 405 becomes the exception rather than the outcome,
  and when it still happens it explains itself.
- A re-approval now suffices on its own. Nothing is remembered as refused at a
  head the train never attempted, so the PR is queued again on the next tick
  after a fresh approval. Before this change, getting unstuck required a head or base
  change or a daemon restart.
- A PR parked on an unofficial approval produces no forge-visible signal: the
  daemon log's stale line, with its `note`, is where it shows. Deciding whether
  that deserves a comment of its own is a follow-up for whoever owns REQ-9.
- Whoever owns the review lane that produced this state still has to fix its
  prompt. The rule is now in the charter harness ships; the lane's own prompt is
  not in this repo.

---
status: proposed
date: 2026-10-22
decision-makers: [joestump]
extends: [ADR-0032]
related: [SPEC-0025, ADR-0032]
---

# ADR-0049: StatusError carries a bounded, credential-redacted body summary

> **Not yet accepted.** Proposed in the PR for stump.wtf/harness#975 by the
> s lane; no maintainer decision exists yet. The issue is Joe's own from the
> 2026-10-22 morning brief, and asks for the body to be carried; what it does
> not settle is how that squares with
> SPEC-0025 REQ-12's "SHALL NOT include the response body". This ADR is that
> square.

## Context and Problem Statement

The merge train's squash merge of stump.wtf/switchboard PR #606 at head
`88b03152` was refused with `SquashMerge ... HTTP 405`. Every static gate
checked out at that attempt and still does: a qualifying approval on the exact
head, all three protected contexts green, `mergeable=true`, an unmoved base.
The 405 was almost certainly transient forge merge-state, but the exact reason
is unrecoverable from the recorded error, because the Gitea merge adapter
discards the response body before building the `StatusError`. The train then
records the `(pr, head, base)` triple as attempted and only retries when the
head or base moves, so the refusal sits unexplained until then — and an
operator diagnosing it on the spot has nothing to read.

The no-body rule exists for a reason. SPEC-0025 REQ-12 says a non-2xx
response SHALL become an error naming the method, status code and repo, and
SHALL NOT include the response body, and the `forge` package documents the
rationale: a body can echo a credential. A hostile or broken forge (or a
proxy in front of it) can reflect the `Authorization` header back in an error
body, and the error string flows into log lines (REQ-14 carries `err`) and,
for a merge refusal, into a public pull request comment. Carrying the raw
body verbatim is therefore not an option; the question is what a *safe*
summary of it can be.

The repo already has a precedent from the other direction: the Switchboard
client (`internal/stackclient/switchboard`) carries a bounded 300-byte body
summary in its status errors, on the basis that its bodies are server-authored
validation text. The forge bodies are from a less trusted endpoint, so they
need the extra redaction step that side skips.

## Decision

Amend SPEC-0025 REQ-12: a non-2xx response SHALL become an error naming the
method, status code and the repo, and SHALL carry the response body only as a
bounded, credential-redacted summary. The raw body SHALL NOT be included.

The summary is built by `forge.SanitizeBody(token, body)`:

1. The client's own token string is replaced wherever it appears. This is the
   credential the client actually holds, and the exact-string match is the
   redaction no pattern can replace.
2. Scheme credentials are redacted (`Bearer`, `Basic`, `token` + value),
   before the field rules, because a field value that begins with a scheme
   word would otherwise leave the credential standing behind it.
3. Credential-bearing fields are redacted in the two forms an error page or
   JSON envelope uses: quoted (`"Authorization":"..."`, `"api_key":"..."`)
   and plain (`Cookie: ...`, `api-key=...`), covering
   `authorization`, `proxy-authorization`, `cookie`, `x-api-key`,
   `api-key`/`api_key`, `x-csrf-token` and `x-xsrf-token`.
4. The result is truncated to 512 bytes on a rune boundary, with an ellipsis.

`StatusError` gains a `Body` field holding the summary, and `Error()` appends
it after the status line only when non-empty, so an empty body leaves the
existing `forge: SquashMerge on …: HTTP 405` shape untouched. The Gitea
adapter's `do()` and `FileContentAtRef` read the non-2xx body up to the bound
and store the summary; the rest of the body is still drained and discarded.

Redaction is defense in depth, not a guarantee: the token exact-match covers
the credential this client holds, the pattern rules cover header and scheme
echoes, and truncation bounds the exposure. Anything the patterns do not name
still reaches the summary — that is the trade the issue asks for, and it is
why the bound stays small.

## Alternatives rejected

* **Carry the raw body.** Violates REQ-12 outright: a reflected
  `Authorization` header would land in a public PR comment and in log lines.
  The whole point of the original rule survives the incident that motivated
  this issue.
* **Carry only the status line, with more detail in a separate field the
  driver chooses not to print.** The driver already prints `err` into the
  merge-refused comment (REQ-14); a second channel would mean every consumer
  (train, tests, future adapters) re-implementing the "which parts are safe"
  decision. One safe string is the simpler contract.
* **Hash or fingerprint the body instead of summarizing it.** A hash confirms
  nothing an operator can act on: it cannot distinguish "please try again
  later" from "branch is protected". The refusal reason is the diagnostic.
* **Leave the adapter as-is and fix the train retry policy to retry transient
  405s faster.** Out of scope for the issue, and it treats the
  symptom: a *non*-transient refusal would still be unexplained.

## Consequences

* A merge refusal is diagnosable on the spot: the merge-refused comment and
  log line carry the forge's own message, e.g. `HTTP 405: {"message":"please
  try again later"}`.
* REQ-12's security property is preserved in a stronger form than before in
  one respect: the token exact-match plus the pattern rules mean the
  credential the client holds cannot reach the summary even if the forge
  echoes it; the token still SHALL NOT appear in any log line or error
  string.
* The `StatusError` shape changes (one new field); `Transient` classification
  is untouched, and an empty body keeps the exact old `Error()` string, so
  no existing behavior moves except where the body is now carried.
* The 512-byte bound and the redaction set are choices of this ADR; both are
  one-constant / one-regex changes if a future refusal message needs them
  widened.

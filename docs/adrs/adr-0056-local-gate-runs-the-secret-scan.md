---
status: proposed
date: 2026-10-29
decision-makers: []
extends: []
related: []
---

# ADR-0056: The local gate runs the secret scan, first

> **Not yet accepted.** Proposed in the PR for stump.wtf/harness#1015 by the
> s handoff lane. The Makefile change in that PR implements it; whether
> `make check` should include the scan needs a maintainer's call.

## Context and Problem Statement

A PR on this repo (stump.wtf/harness#1014) added a test fixture whose
literal value the gitleaks `generic-api-key` rule reads as a credential.
The pipeline's first stage failed in about five seconds and the run red,
with every dependent job skipped and branch protection wedged on a change
whose local gate was green. From the outside the run read as an
infrastructure fault: the `uses:` wrapper job (`secrets`) records a failed
callee as `completed/failure` with no runner attached and a log that
answers `job not started`, so the run was read twice as "the workflow call
never scheduled" and an infrastructure investigation started (issue #1015).

It was not. The child jobs of the `uses:` call — the jobs that actually run
gitleaks — were scheduled and ran on a runner in every attempt (run 2059,
attempts 1–3, on `delta`, `eta` and `epsilon`), and each one failed with
`leaks found: 1`: the finding on the PR's own fixture
(`internal/relay/fake_switchboard_test.go:99`, a fake `lt-` lease token).
The wedge is cleared on that PR (28d147b assembles the fixture at run time
and fingerprints the superseded commits in `.gitleaksignore`).

What the episode exposed is the local side: the scan a diff has to pass is
not part of the local gate. `make check` runs lint, test, race and fuzz —
not gitleaks — so a fixture literal that reads as a credential is
discoverable only as a wedged PR, in a failure shape that does not point at
the finding.

## Decision

`make check` runs the same gitleaks invocation the pipeline's first stage
runs — full history, the repo's `.gitleaksignore`, redacted output — as its
first step, so the ordering matches the pipeline: a leaked credential must
not wait behind a compile. The scan is also available on its own as
`make scan`. Locally, a missing `gitleaks` binary fails the gate with an
install hint rather than skipping silently: a silently shorter local gate
is how this class of wedge happens. On a runner the shared CI image ships
no `gitleaks`, so there the check warns loudly and skips the duplicate: the
pipeline's dedicated secrets stage has already run the same scan earlier in
the run, so no coverage is lost. Bundling gitleaks into the runner image
would let the build job gate on it too; that is an infrastructure
follow-up, not part of this decision.

## Consequences

- Authors need gitleaks installed for a complete local `make check`; the
  gate says so instead of passing short. CI needs no change for coverage:
  the dedicated secrets stage runs the scan on every run.
- `make check` grows by a few seconds locally (a history scan of this repo
  is about 3s warm). The CI build job runs in the shared runner image,
  which has no gitleaks, so it warns and skips the scan there instead of
  failing the run.
- Fixture literals that read as credentials must be assembled at run time
  or fingerprinted in `.gitleaksignore` (the existing convention); the scan
  makes that requirement visible locally, as a finding on screen, instead
  of as a five-second red PR that reads like an outage.

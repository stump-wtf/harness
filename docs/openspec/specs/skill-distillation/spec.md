---
status: draft
date: 2026-09-22
implements: [ADR-0012, ADR-0030]
requires: [SPEC-0005, SPEC-0006, SPEC-0011]
---

# SPEC-0007: Cross-Harness Skill Distillation

> **Not yet implemented.** Design stage. No distiller, distill ledger or
> skill repo support exists today. Tracked by the SPEC-0007 epic, harness#69.

## Overview

This spec covers how a lesson the fleet keeps relearning becomes a reviewed
skill, and how that skill is served. See **ADR-0012** for the search-only skill
tier, and **ADR-0030** for grounding, verification, proposal, skill repos and
distillers.

Sessions show where agents struggled. The merged, green, unreverted pull request
that ended the struggle supplies the content. Before any human sees a candidate,
a model that never saw that pull request must rebuild the change from the skill
alone. Every change to a skill then arrives as a pull request, and its merge
promotes it.

The operator declares **skill repos** (a remote, a path, and the harnesses they
serve) and **distillers** (triggered harnesses that name the harnesses they
learn from, the skill repo they propose to, and how many distinct repositories
a lesson must span). Skills are served by search, never projected, so delivery
does not depend on which adapter a harness runs.

The daemon makes no model request, holds no model or forge credential, and
writes to no repository. Deterministic work runs in `harness distill`. Each
model call runs in its own one-shot scratch run (SPEC-0011), with inputs
assembled by code.

## Requirements

### Requirement: Harvest Boundary

Distillation SHALL obtain session data only through the SPEC-0006
opt-in-enforcing trajectory interface. A harness without `harvest_trajectory`
MUST contribute nothing to any stage. Every string taken from a transcript SHALL
be redacted before it is stored or passed to a model run.

Sessions of any distiller, and of any model run a distiller starts, MUST NOT
contribute evidence to any distiller.

#### Scenario: Non-harvesting harnesses contribute nothing

- **WHEN** a harness has not opted into harvesting
- **THEN** none of its sessions appear in the ledger, in any candidate, or in any
  evidence bundle

#### Scenario: Distillation does not learn from itself

- **WHEN** a distiller's `from` glob matches another distiller, or matches a
  scratch run a distiller started
- **THEN** that harness's sessions are excluded from the pass

#### Scenario: Transcript text is redacted before it leaves

- **WHEN** a session excerpt that contains a credential-shaped string is placed
  in an evidence bundle
- **THEN** the bundle carries the redacted form, and the unredacted form is
  written nowhere

### Requirement: Session-To-Pull-Request Linking

`harness distill` SHALL link each attributed session to the pull requests it
worked toward. Linking SHALL resolve the session's working directory to its
repository's canonical host, and MUST NOT link to a copy that declares
`downstream-mirror`. The repository remote, the branch and the HEAD SHA SHALL
be captured when the session is harvested — a local, read-only git call — so
linking does not depend on a working directory that pass time finds deleted
(worktrees removed, scratch directories cleaned). Linking SHALL use the last
branch and working directory recorded in the transcript, not the first — a
session that starts in a primary checkout parked on someone else's branch and
later moves to a worktree must link to the worktree's work — and SHALL treat a
branch that is the repository's default branch as no branch at all. It SHALL
match by the transcript's recorded branch when one
exists, and otherwise by commits reachable in the captured working directory
whose committer time falls within the session's window. A session that matches no pull
request, or matches several in one repository with no branch to choose between
them, SHALL be left unlinked. The number of unlinked sessions SHALL be reported.

For each linked pull request the ledger SHALL record: state, base SHA, merge
SHA, the combined status of the head at merge, each review with its state and
comment text, the commits pushed after the first review, and whether a commit on
the default branch reverted it within the configured revert window.

#### Scenario: Branch match

- **WHEN** a Claude Code session records branch `feat/x` in `stump.wtf/reduit`,
  and a pull request in that repository has head `feat/x`
- **THEN** the session is linked to that pull request

#### Scenario: Commit match without a branch

- **WHEN** a Crush session records no branch, and a commit made in its working
  directory during its window is the head of a pull request
- **THEN** the session is linked to that pull request

#### Scenario: Ambiguity fails closed

- **WHEN** a session without a branch matches two open pull requests in the same
  repository
- **THEN** it is linked to neither, and the unlinked count increases

#### Scenario: Mirrors are never targets

- **WHEN** a working directory's `origin` points at a copy tagged
  `downstream-mirror`
- **THEN** linking resolves to the canonical copy its `canonical-*` topic names,
  or leaves the session unlinked if that topic is absent

#### Scenario: A session that starts on the default branch

- **WHEN** a session is harvested in a checkout whose branch is the repository's
  default branch
- **THEN** it is treated as recording no branch, and links only through the
  commit match — ambiguity still leaves it unlinked

### Requirement: Grounding Evidence

A pull request SHALL count as grounding evidence only if it merged, its head's
combined status was success at merge, and it was not reverted within the revert
window. Candidates SHALL be found by three signals, all detected without a
model:

1. **Review correction**: a changes-requested review or line comment, followed by
   a commit touching the commented span, followed by merge.
2. **Red to green**: a required check failing on a head, followed by a commit
   that makes it pass, followed by merge.
3. **Struggle, then resolution**: in a linked session, at least the configured
   number of failed `exec` or `verify` actions, or repeated edit→verify cycles on
   the same targets, before a passing `verify`, where the linked pull request's
   merged hunks touch those targets.

A session whose own trajectory shows a retrieval of skill X MUST NOT contribute
evidence to X's purpose key.

#### Scenario: Detection needs no model

- **WHEN** `harness distill` links and detects over the available sessions
- **THEN** it completes without starting any model run

#### Scenario: Closed or reverted pull requests ground nothing

- **WHEN** a candidate's only evidence is a pull request that was closed
  unmerged, red at merge, or reverted within the window
- **THEN** no candidate is emitted

#### Scenario: Retrieval does not reinforce itself

- **WHEN** a session called `get_skill` for skill X and later ended in a merged
  pull request matching X's purpose key
- **THEN** that session is not counted in X's evidence

### Requirement: Evidence Key, Purpose Key And Scope

Detection SHALL group evidence by an **evidence key** computed without a model
from the signal kind, the normalized class of the touched paths, and the failing
check name or error signature. After authoring, each skill SHALL carry a
**purpose key**, `(task_family, action, target)`, computed by code from the
record's closed-vocabulary fields. A model MUST NOT assign either key directly.
The ledger SHALL remember which evidence key produced which purpose key, so a
suppressed or already-active lesson is recognized before any model run. The
number of distinct canonical repositories under an evidence key SHALL be
compared with the distiller's `min_repos`. Below it, the candidate SHALL wait in the ledger. At or
above it, the candidate is eligible. A distiller SHALL skip an eligible
candidate whose purpose key is already active in some skill repo served to every
harness its evidence came from.

#### Scenario: Below the distiller's threshold, a candidate waits

- **WHEN** a distiller has `min_repos = 3` and a candidate's evidence spans two
  repositories
- **THEN** nothing is proposed, and the candidate stays in the ledger with its
  count

#### Scenario: A single-project distiller acts on one repository

- **WHEN** a distiller has `min_repos = 1` and a candidate's evidence comes from
  one repository
- **THEN** the candidate is eligible

#### Scenario: Already reachable lessons are skipped

- **WHEN** a candidate's purpose key is active in a skill repo whose `serve_to`
  matches every harness in its evidence
- **THEN** no distiller proposes it again

#### Scenario: Out-of-vocabulary values fall back

- **WHEN** an author run returns a `task_family` outside the closed vocabulary
- **THEN** it is replaced with the vocabulary's fallback value before the key is
  computed, and the substitution is logged

### Requirement: Distillation Runs Outside The Daemon

Linking, detection, deduplication and proposal SHALL run in `harness distill`,
a client process started by the distiller harness. Every model call SHALL run in
a separate one-shot scratch run. The daemon MUST NOT issue model requests, MUST
NOT issue forge requests, MUST NOT hold model or forge credentials, and MUST NOT
write to any repository working tree, including a skill repo's clone. `harness
distill` SHALL hold the forge credential, read from the distill table's
`credential_file` — a path that never enters the wrapper harness's environment.
Model runs, and every build or test a verification step executes, SHALL be
spawned with an allowlisted environment containing only the variables their role
needs — never the daemon's inherited environment, the daemon's `env_file`, or
any variable carrying a forge credential. A model run SHALL be spawned with no
MCP bridge (`mcp_bridge = false`) and no path to the daemon socket.

#### Scenario: Daemon makes no model or forge request

- **WHEN** a full distillation pass runs
- **THEN** every model request comes from a one-shot run, every forge request
  comes from `harness distill`, and none comes from the daemon process

#### Scenario: Model runs cannot reach the forge

- **WHEN** a model run starts, or a build or test command runs in a clean room
  during verification
- **THEN** its environment contains no forge credential at all: no variable
  inherited from the daemon's environment or `env_file`, and nothing that can
  push, comment or merge

### Requirement: Isolated Model Runs

Each model run SHALL receive exactly the inputs listed for its role. No other
file SHALL be placed in its working directory.

| Run | Receives | MUST NOT receive |
|---|---|---|
| Author | the evidence bundle: redacted session excerpts, review text, merged hunks, cited spans | forge credentials |
| Reconstructor | the skill record, the task statement, a clean-room snapshot at the base SHA | the merged diff, later history, review text, the pull request's identity |
| Judge | the merged hunks, the reconstructed hunks, the skill's summary render | the full skill record, sessions |
| Adjudicator | the judge's reason, both sets of hunks, the full skill record | sessions |
| Control | the task statement, the same clean-room snapshot | the skill record |

Author, judge and adjudicator runs SHALL start with tools disabled. Reconstructor
and control runs MAY have file tools confined to their clean room and restricted
exec. Any call to the `harness` CLI, or to an `mcp__harness__*` tool, from any
model run SHALL mark that run contaminated.

#### Scenario: Reconstructor never receives the answer

- **WHEN** a reconstructor run starts
- **THEN** its working directory contains no `.git`, and no file whose contents
  match the post-merge version of any cited file

#### Scenario: Model runs cannot reach Harness's own surfaces

- **WHEN** a model run starts
- **THEN** it has no MCP bridge and no path to the daemon socket, so the facade
  read tools and `harness logs` are unreachable from it; a `harness` CLI call or
  an `mcp__harness__*` tool call marks the run contaminated

### Requirement: Blind Reconstruction Verification

Every candidate SHALL pass verification before it is proposed. Verification
SHALL:

1. Build a clean room from `git archive` of the base SHA in a fresh temporary
   directory outside the distill state directory — away from the evidence
   bundles and the ledger — containing no
   repository metadata; remove it when verification ends. Take the task
   statement from the linked issue's body,
   or, when there is no linked issue, from the session's first user message,
   redacted. Record which source was used. If the statement contains any
   non-blank line of the merged hunks verbatim, record `task_leak: true`.
2. Run the reconstructor on the files the skill cites.
3. Audit the reconstructor's transcript through agent-trace. A read outside the
   clean room — including a relative path that escapes it, which the audit SHALL
   treat as a read outside: agent-trace is required to report such escapes, not
   drop them silently — or an exec that fetches, clones or calls a forge, SHALL
   mark the run contaminated.
4. Run the judge. `equivalent: true` SHALL pass directly. Anything else SHALL go
   to the adjudicator, and `keep: true` SHALL pass as adjudicated.
5. `harness distill` runs `make test` in the reconstructor's clean room when the
   repository defines that target, under the reconstructor's allowlisted
   environment — never inside a model run, and never with `harness distill`'s
   own environment — and records the result.
6. When the merged change is under `replay_max_lines`, run the control. If the
   control's test result and judge verdict are at least as good as the
   reconstructor's, the candidate SHALL be dropped and its purpose key
   suppressed.

A candidate SHALL be proposed only with a fidelity pass (direct or adjudicated)
and a clean audit. Test and control results SHALL travel with the proposal as
evidence and are not required to pass.

#### Scenario: A leaky task statement is flagged

- **WHEN** a session's first message contains a line that appears verbatim in
  the merged hunks
- **THEN** verification proceeds, and the dossier marks the rebuild
  `task_leak: true`

#### Scenario: The test run gets no forge credential

- **WHEN** `make test` runs during verification
- **THEN** it executes under the reconstructor role's allowlisted environment,
  and no forge credential is present in it

#### Scenario: Contaminated runs stop the candidate

- **WHEN** the reconstructor's transcript shows a read outside its clean room
- **THEN** the run is recorded as contaminated, and the candidate is not
  proposed in this pass

#### Scenario: A failed rebuild is adjudicated, not rejected

- **WHEN** the judge rules a reconstruction not equivalent
- **THEN** the adjudicator decides whether the skill is faithful, and a
  `keep: true` verdict lets the candidate proceed as adjudicated

#### Scenario: A skill that teaches nothing is dropped

- **WHEN** the control run's tests pass and its judge verdict is equivalent,
  matching the reconstructor's
- **THEN** the candidate is not proposed, and its purpose key is suppressed in
  the ledger

### Requirement: Skill Artifact

A skill SHALL be a markdown file at `<path>/<slug>/SKILL.md` in a skill repo,
where `<path>` is the skill repo's configured path. Its frontmatter SHALL carry: `name`; `description`, of at most
300 characters; `level` (`atomic`, `composite` or `pattern`); `status`
(`active`, `superseded` or `retired`); `task_family` and `tags`, from the closed
vocabularies; `purpose_key`; `symptoms`, the literal strings from its evidence;
`applies_to`, path globs; and `provenance`. `provenance` SHALL record the scope
counts, first- and last-seen dates, one entry per evidence item (signal,
repository, pull request, merge SHA, span, session count), and the verification
results. The body SHALL contain the sections *When to use*, *Steps*,
*Invariants*, *Failure modes*, *Do not* and *Evidence*. The steps SHALL describe
the procedure against the repository and stack, not against a particular agent's
tools.

#### Scenario: Artifact carries its evidence

- **WHEN** a skill is authored
- **THEN** each evidence entry names a pull request, a merge SHA and a span, and
  the verification results are present

#### Scenario: Lint rejects an incomplete record

- **WHEN** `harness skills lint` reads a skill missing the *Do not* section, or
  whose `description` exceeds 300 characters
- **THEN** it exits non-zero, naming the file and the rule

### Requirement: Default-Branch Gate

For each skill repo, the daemon SHALL index only files under its `path` on the
clone's checked-out default branch whose `status` is `active`. If a clone's
`HEAD` is not the default branch, or its working tree is dirty, the daemon SHALL
keep serving that repo's previous index and SHALL report the condition through
`harness doctor`. Harness MUST NOT project skill-repo content at any status: the
SPEC-0006 projection path SHALL exclude every skill repo unconditionally. The
daemon MUST NOT fetch, pull, commit or push in any clone. `harness skills sync`
SHALL fast-forward every declared clone, and every distiller pass SHALL run it
first.

#### Scenario: Proposals are inert

- **WHEN** a skill exists only on a proposal branch or an open pull request
- **THEN** no search returns it, and no harness has it projected

#### Scenario: Merge promotes

- **WHEN** the proposal is merged and the store is synced
- **THEN** the next index refresh makes the skill searchable, and it is still
  projected into no harness

#### Scenario: A detached store keeps the last good index

- **WHEN** a clone's `HEAD` points at a branch other than the default
- **THEN** searches return the previous index's results, and `harness doctor`
  warns

### Requirement: Pull Request Proposals

`harness distill propose` SHALL submit every new skill, revision, supersession
and retirement as a pull request.

- Every proposal SHALL target the distiller's `to` skill repo, on that
  repository's canonical host, at `<path>/<slug>/SKILL.md`. `propose` MUST NOT
  modify any other file, including a repository's `AGENTS.md` or `CLAUDE.md`.
- Each pull request SHALL carry exactly one skill, on a branch named
  `<branch_prefix><slug>` and cut from a freshly fetched default branch, in a
  clone under `harness distill`'s state directory.
- The body SHALL contain the marker `<!-- harness-distill key=<purpose_key> -->`
  and the evidence dossier: the claim; an evidence table; the fidelity, test,
  control and audit results; the scope; the context cost in characters of the
  summary and full renders; and any skill it supersedes.
- If an open pull request against the same skill repo already carries the same
  marker, `propose` SHALL push a new commit to it and MUST NOT open another,
  whichever distiller opened the first.
- `propose` MUST NOT force-push, merge, approve, or enable auto-merge.
- At most `max_open` distillation pull requests SHALL be open per target
  repository. Candidates beyond the cap SHALL wait in the ledger, ranked by
  distinct repositories × occurrences × signal weight.
- `reviewers` SHALL be requested, and `labels` applied, when the pull request is
  created.

#### Scenario: Re-running does not duplicate

- **WHEN** two passes produce the same purpose key while the first pass's pull
  request is open
- **THEN** there is one pull request, and the second pass added a commit to it

#### Scenario: The cap holds

- **WHEN** `max_open` distillation pull requests are open against a repository
- **THEN** a pass opens no new pull request there, and the waiting candidates
  keep their rank in the ledger

#### Scenario: Diverged remote is refused, not overwritten

- **WHEN** the remote proposal branch has commits the local clone lacks
- **THEN** `propose` fetches and adds its commit on top, or fails with
  `ErrBranchDiverged`, and never rewrites the remote branch

#### Scenario: Two distillers converge

- **WHEN** two distillers with the same `to` produce the same purpose key
- **THEN** one pull request exists, carrying both distillers' evidence

#### Scenario: Only the skill file changes

- **WHEN** a proposal targets a skill repo that is also a project repository
- **THEN** the pull request's diff touches only files under the skill repo's
  `path`

### Requirement: Review Feedback And Suppression

`harness distill` SHALL list the review comments on its open pull requests and
pass them to a new author run as untrusted data. The revised skill SHALL be
pushed as a new commit, with a comment summarizing the change. A merged pull
request SHALL mark its key `promoted` for that skill repo in the ledger. A pull
request closed unmerged SHALL mark its key `rejected` for that skill repo, keep
the reviewer's last comment as the reason, and suppress the key for that repo
until the evidence count for it has doubled. A
later proposal for a suppressed key SHALL link the rejected pull request.

#### Scenario: A rejection is remembered

- **WHEN** a distillation pull request is closed without merging and the next
  pass sees the same evidence
- **THEN** no pull request is opened for that key

#### Scenario: New evidence reopens the question

- **WHEN** the evidence count for a rejected key has doubled since the rejection
- **THEN** a new pull request may be opened, and its body links the rejected one

### Requirement: Embedded Retrieval Index

Retrieval SHALL use an embedded full-text index that requires no external
service, no network access and no model weights. It SHALL record which skill
repo each skill came from. The index SHALL cover `name`, `description`,
`symptoms`, `tags` and `applies_to`, and SHALL stem terms so that morphological
variants match. It SHALL be a rebuildable cache: deleting it and
reindexing from the skill repos' default branches MUST restore equivalent
behavior.

#### Scenario: Search runs with no external dependency

- **WHEN** a search runs on a machine with no other software installed and no
  network access
- **THEN** it returns results

#### Scenario: Literal symptom text matches

- **WHEN** a query contains a literal string from an active skill's `symptoms`
- **THEN** that skill is among the results

#### Scenario: Index is rebuildable

- **WHEN** the index is deleted and rebuilt
- **THEN** the same active skills are searchable, with equivalent ranking

### Requirement: Search And Retrieval Tools

Skill repos SHALL be exposed through the SPEC-0005 facade as two tools.

- `search_skills` SHALL search only the skill repos whose `serve_to` matches the
  calling harness, as attributed by SPEC-0005 REQ "Caller Identity". An
  unattributed caller SHALL search only skill repos whose `serve_to` is
  `["*"]`. It SHALL take a query and an optional `paths` list, SHALL
  return identifiers (qualified by skill repo) and descriptions only, and SHALL
  return 3 results by default and at most 5. A skill whose `applies_to` matches any given path SHALL rank above
  an otherwise equal skill.
- `get_skill` SHALL return one skill. By default it SHALL return a summary render
  (*When to use*, *Steps*, *Invariants*, *Do not*) of at most
  `summary_max_chars`. `render: "full"` SHALL return the whole file.
- The tool descriptions SHALL tell callers to search while planning and while
  reviewing a concrete draft or diff, and to supply several phrasings, including
  any literal error text.
- Each active skill SHALL also be available as an MCP resource under a stable
  identifier.

Each retrieval SHALL be counted per (skill repo, skill), together with the
session that made it, in the daemon's state.

#### Scenario: Search returns descriptions, not bodies

- **WHEN** a search matches several skills
- **THEN** the result carries at most the requested number of identifiers and
  descriptions, and no bodies

#### Scenario: Search is scoped to the caller

- **WHEN** harness `spotter/agent` calls `search_skills` and a skill repo has
  `serve_to = ["reduit/*"]`
- **THEN** no skill from that repo is returned

#### Scenario: Unattributed callers see only unscoped repos

- **WHEN** a session started outside Harness calls `search_skills` without a
  valid token
- **THEN** only skill repos with `serve_to = ["*"]` are searched

#### Scenario: Summary by default

- **WHEN** `get_skill` is called without `render`
- **THEN** the response is the summary render, no longer than
  `summary_max_chars`

#### Scenario: Paths narrow the search to the diff

- **WHEN** `search_skills` is called with `paths: [".gitea/workflows/ci.yml"]`
- **THEN** skills whose `applies_to` matches that path rank first

### Requirement: Supersession

If a skill's purpose key equals that of an active skill in the same skill repo,
`propose`
SHALL submit it as a change to the existing file rather than as a new one. If
its evidence contradicts the existing skill, it SHALL instead be a pull request
that sets the old skill's `status` to `superseded`, sets `superseded_by`, and
adds the new skill. Supersession SHALL take effect only on merge. Two active
skills on one skill repo's default branch MUST NOT share a purpose key, and
`harness skills lint` SHALL enforce this.

#### Scenario: Same purpose updates, not duplicates

- **WHEN** a candidate's purpose key matches an active skill
- **THEN** the pull request modifies that skill's file, and no second file is
  added

#### Scenario: Unmerged supersession changes nothing

- **WHEN** a supersession pull request is open
- **THEN** the old skill is still searchable

### Requirement: Staleness Re-Verification

On each pass, `harness distill` SHALL compare every span cited by an active
skill against the current default branch of its repository. A skill SHALL be
re-verified when a cited span has changed or its file no longer exists. A skill
that fails re-verification SHALL produce a supersede or retire pull request.

#### Scenario: Deleted evidence triggers re-verification

- **WHEN** a file cited in a skill's evidence has been deleted on its
  repository's default branch
- **THEN** the skill is re-verified against current code, and a failure opens a
  retire pull request

### Requirement: Retrieval-Count Retirement

An active skill SHALL become eligible for retirement only after
`retire_grace` has elapsed since its merge, and only if it has had no
retrievals within `retire_window`. A skill still inside its grace period MUST NOT
be retired. Retirement SHALL be a pull request setting `status: retired`. The
file SHALL remain. Retrieval counts SHALL be stored in daemon state separate from
the index. If they are lost, every skill SHALL be treated as newly promoted.

#### Scenario: Grace period protects new skills

- **WHEN** a skill merged within `retire_grace` has zero retrievals
- **THEN** no retire pull request is opened

#### Scenario: Lost counts fail safe

- **WHEN** the retrieval-count store is missing at startup
- **THEN** no skill becomes eligible for retirement until `retire_grace` has
  elapsed again

### Requirement: Skill Repos

A skill repo SHALL be declared by a `[skill_repo.<name>]` table with `remote`
(required), `path` (default `skills`) and `serve_to`, a list of harness
selectors (default `["*"]`). Harness SHALL keep its own clone of each skill repo
under its state directory. The clone MAY be sparse to `path`. It MUST NOT be an
operator's working checkout. Skill repo tables SHALL be accepted in global
configuration only. With no skill repo declared, the facade SHALL NOT register
the skill tools, and no index file SHALL be created.

#### Scenario: Project files cannot declare skill repos

- **WHEN** a project `harness.toml` contains a `[skill_repo.*]` table
- **THEN** the load fails with an error naming the file and the table

#### Scenario: Inert by default

- **WHEN** no skill repo is declared
- **THEN** `search_skills` and `get_skill` are not registered, and no index file
  exists

### Requirement: Distillers

A harness SHALL become a distiller by carrying a `[harness.<name>.distill]`
table with:

- `from`: harness selectors naming whose sessions and pull requests count. A
  selector is an exact harness name or a glob over qualified names.
- `to`: the name of one skill repo.
- `min_repos`, default 1. Optional per-distiller policy: `max_open` (default 3),
  `max_candidates` (default 5), `replay_max_lines` (default 400),
  `revert_window` (default 14 days), `reviewers`, `labels`, `branch_prefix`,
  `verifier` and `verifier_env_file`.

`harness distill run <name>` SHALL run one pass for the named distiller. Loading
SHALL fail when `to` names an undeclared skill repo; when an exact `from` name is
undeclared or lacks `harvest_trajectory`; when a distiller lists itself in
`from`; when the harness carrying the table has neither `schedule` nor
`triggers`; or when the table appears in a project `harness.toml`. Glob
selectors SHALL be resolved at each pass, and a glob that matches no harness
SHALL be reported in the pass summary.

#### Scenario: A dangling target fails the load

- **WHEN** a distiller sets `to = "go-stack"` and no `[skill_repo.go-stack]`
  exists
- **THEN** the configuration fails to load, naming the distiller and the missing
  skill repo

#### Scenario: A non-harvesting source fails the load

- **WHEN** a distiller's `from` lists an exact harness name whose
  `harvest_trajectory` is false
- **THEN** the configuration fails to load, because that source would silently
  contribute nothing

#### Scenario: Two tiers are two distillers

- **WHEN** one distiller has `from = ["reduit/*"]`, `to = "reduit"`,
  `min_repos = 1`, and another has `from = ["*"]`, `to = "go-stack"`,
  `min_repos = 3`
- **THEN** a lesson seen only in reduit is proposed to `reduit`, and a lesson
  seen in three repositories is proposed to `go-stack`

#### Scenario: An empty glob is visible

- **WHEN** a pass runs and the `from` glob `spotter/*` matches no registered
  harness
- **THEN** the pass summary reports the selector as matching nothing

### Requirement: Error Handling Standards

All operations that can fail MUST follow structured error handling:

- Errors MUST be wrapped with context at each layer boundary (for example,
  "distill: link: resolve canonical repo for ~/src/reduit: no canonical topic").
- Sentinel errors MUST be defined at minimum for: skill not found, skill repo
  not found, index unavailable, malformed frontmatter, session not linked, run
  contaminated, and branch diverged.
- Errors MUST NOT be silently swallowed. Every error MUST be returned, logged
  with context, or explicitly handled with a documented reason for suppressing
  it.
- Errors MUST be reported with structured logging (key-value pairs, not string
  interpolation).

#### Scenario: A malformed skill does not break the index

- **WHEN** one skill has frontmatter that cannot be parsed during a
  reindex
- **THEN** the other skills index successfully, and a warning names the file
  and the cause

#### Scenario: One bad candidate does not stop a pass

- **WHEN** verification of one candidate fails with an error
- **THEN** the pass records the error against that candidate and continues with
  the next

### Requirement: Database Operation Standards

All operations against the retrieval index, the retrieval-count store and the
distill ledger MUST follow structured data access:

- Multi-step mutations that must be atomic, including a full reindex and a
  ledger update for one pull request, MUST run in a transaction.
- Connection lifecycle MUST be managed explicitly: connections are released
  after use, and timeouts are configured.
- Queries MUST be parameterized. String interpolation into a query MUST NOT
  occur, including for search terms supplied by users or agents.

#### Scenario: Reindex is atomic

- **WHEN** a reindex fails partway through
- **THEN** the previous index contents remain queryable, and no partial state is
  visible

#### Scenario: Search terms are parameterized

- **WHEN** a search query contains characters that mean something in the query
  language
- **THEN** they are bound as parameters and cannot change the query's structure

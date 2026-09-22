---
status: draft
date: 2026-09-22
implements: [ADR-0012, ADR-0030]
requires: [SPEC-0005, SPEC-0006, SPEC-0008, SPEC-0014]
---

# SPEC-0007: Cross-Harness Skill Distillation

> **Not yet implemented.** Design stage. No distiller, distill ledger or skill
> repo support exists today. Tracked by the SPEC-0007 epic in the Harness issue
> tracker. A distiller runs as a `command` one-shot, a kind that ADR-0023 (in
> review) introduces.

## Overview

This spec covers how a lesson the fleet keeps relearning becomes a reviewed
skill, and how that skill is served. See **ADR-0012** for the search-only skill
tier, and **ADR-0030** for grounding, verification, proposal, skill repos and
distillers.

Sessions show where agents struggled. The merged, green, unreverted pull request
that ended the struggle supplies the content. Before a candidate is proposed, a
model that never saw that pull request must rebuild the change from the skill
alone. Every change to a skill then arrives as a pull request, and its merge
promotes it.

The operator declares **skill repos** (a remote, a path, and the harnesses they
serve) and **distillers** (command one-shots that name the harnesses they learn
from, the repositories evidence may come from, the skill repo they propose to,
and how many distinct repositories a lesson must span). Skills are served by
search, never projected.

The daemon records each harvested session's git provenance, serves the index,
and counts retrievals. It makes no model or forge request, holds no model or
forge credential, and writes to no repository. `harness distill` does the
deterministic work and holds the forge token. Every model call is a child
process of `harness distill`, and code fixes that child's inputs, environment
and tools.

## Requirements

### Requirement: Harvest Boundary

Distillation SHALL obtain session data only through the SPEC-0006
opt-in-enforcing trajectory interface and the provenance records of REQ
"Session Git Provenance". A harness without `harvest_trajectory` MUST contribute
nothing to any stage. Every string that reaches an evidence bundle, a model run,
a dossier or a skill SHALL be redacted first: transcript text, review text,
merged hunks and error excerpts alike. Sessions of any distiller, and of any
model run a distiller starts, MUST NOT contribute evidence.

#### Scenario: Non-harvesting harnesses contribute nothing

- **WHEN** a harness has not opted into harvesting
- **THEN** none of its sessions appear in the ledger, in any candidate, or in any
  evidence bundle

#### Scenario: Distillation does not learn from itself

- **WHEN** a distiller's `from` glob matches another distiller
- **THEN** that harness's sessions are excluded from the pass

#### Scenario: Hunks are redacted too

- **WHEN** a merged hunk contains a credential-shaped string
- **THEN** the evidence bundle, the dossier and the skill carry the redacted
  form, and the unredacted form is written nowhere

### Requirement: Session Git Provenance

For each harvested session it attributes, the daemon SHALL record the working
directory's origin remote URL, with any credentials stripped, and every distinct
(branch, HEAD SHA) pair observed there. It SHALL sample when it delivers the
session's events and when the session goes idle. The records SHALL be made with
local, read-only git operations that need no credentials. They SHALL be kept in
daemon state, and they SHALL outlive the working directory. A session whose
working directory is not a git repository SHALL have an empty record.

#### Scenario: Branch changes are all recorded

- **WHEN** a session starts on `main`, then switches to `feat/x` and commits
  twice
- **THEN** its record holds `main` with its HEAD, and `feat/x` with each HEAD
  observed after the commits

#### Scenario: Provenance survives a deleted worktree

- **WHEN** a session's worktree is removed after the session ends
- **THEN** its record is unchanged and still available to linking

#### Scenario: Credentials never enter the record

- **WHEN** the origin remote URL embeds a user and token
- **THEN** the recorded URL contains neither

### Requirement: Session-To-Pull-Request Linking

`harness distill` SHALL link a session to a pull request only from the session's
provenance record. The recorded remote SHALL be resolved to its canonical host,
and linking MUST NOT target a copy that declares `downstream-mirror`. The
repository MUST match the distiller's `evidence_repos`. A pull request SHALL
link when its head branch is a recorded branch and its commit list contains a
recorded HEAD SHA, or, if the branch no longer matches, when its commit list
contains a recorded HEAD SHA. A branch name alone MUST NOT link, and a pair
observed on the repository's default branch SHALL count as no branch. A session
that
matches no pull request, or several in one repository, SHALL be left unlinked,
and the number of unlinked sessions SHALL be reported.

For each linked pull request the ledger SHALL record its state, base SHA, merge
SHA, the combined status of the head at merge, each review with its state and
comment text, the commits pushed after the first review, and any revert on the
default branch.

#### Scenario: Branch and HEAD together

- **WHEN** a session recorded branch `feat/x` with HEAD `abc123` in
  `your-org/reduit`, and a pull request there has head `feat/x` with `abc123` in
  its commits
- **THEN** the session is linked to that pull request

#### Scenario: A branch name alone does not link

- **WHEN** a session recorded branch `feat/y` but none of its HEADs appear in the
  pull request whose head is `feat/y`
- **THEN** the session is not linked to it

#### Scenario: A default-branch session links only by HEAD

- **WHEN** a session's only recorded pairs are on the repository's default
  branch
- **THEN** it links to a pull request only if one of those HEADs appears in the
  pull request's commit list

#### Scenario: Squash-merged work still links

- **WHEN** a session's recorded HEAD appears in a squash-merged pull request's
  commit list, and the branch has since been deleted
- **THEN** the session is linked to that pull request

#### Scenario: Ambiguity fails closed

- **WHEN** a session's recorded HEADs appear in two pull requests in one
  repository
- **THEN** it is linked to neither, and the unlinked count increases

#### Scenario: Mirrors are never targets

- **WHEN** a recorded remote points at a copy tagged `downstream-mirror`
- **THEN** linking resolves to the canonical copy its `canonical-*` topic names,
  or leaves the session unlinked if that topic is absent

### Requirement: Grounding Evidence

A pull request SHALL count as grounding evidence only if it merged, its head's
combined status was success at merge, at least `revert_window_days` have passed
since the merge, and it has not been reverted. A younger pull request SHALL be
recorded as pending. Pull requests carrying the distillation marker, pull
requests against any declared skill repo, and pull requests in repositories
outside `evidence_repos` MUST NOT count. Candidates SHALL be found by three
signals, all detected without a model:

1. **Review correction**: a changes-requested review or line comment, followed by
   a commit touching the commented span, followed by merge.
2. **Red to green**: a required check failing on a head, followed by a commit
   that makes it pass, followed by merge.
3. **Struggle, then resolution**: in a linked session, at least the configured
   number of failed `exec` or `verify` actions, or repeated edit→verify cycles on
   the same targets, before a passing `verify`, where the linked pull request's
   merged hunks touch those targets.

A session MUST NOT contribute evidence to purpose key K if its trajectory shows a
retrieval of a skill with key K, or if it reviewed a distillation pull request
for K.

#### Scenario: Detection needs no model

- **WHEN** `harness distill` links and detects over the available sessions
- **THEN** it completes without starting any model run

#### Scenario: Closed, red or reverted pull requests ground nothing

- **WHEN** a candidate's only evidence is a pull request that was closed
  unmerged, red at merge, or reverted
- **THEN** no candidate is emitted

#### Scenario: Young merges wait

- **WHEN** a pull request merged less than `revert_window_days` ago
- **THEN** it is recorded as pending and contributes to no candidate in this
  pass

#### Scenario: Retrieval does not reinforce itself

- **WHEN** a session called `get_skill` for a skill with purpose key K and later
  ended in a merged pull request whose evidence maps to K
- **THEN** that session is not counted toward K

### Requirement: Evidence Key, Purpose Key And Scope

Detection SHALL group evidence by an **evidence key**, computed without a model:
`(signal kind, path class, failure signature)`.

- **Path class** is the sorted set obtained by mapping each touched path through
  a fixed table of well-known files and globs (at minimum `go.mod`, `go.sum`,
  `package.json`, `Makefile`, `Dockerfile`, `.gitea/workflows/*`,
  `.github/workflows/*` and `*_test.go`), falling back to the file extension.
- **Failure signature** is the failing check's context name, or the first line of
  the error excerpt, with digits, hex strings, paths and quoted strings replaced
  by fixed placeholders. It is empty for review corrections.

A candidate SHALL be eligible once its evidence key spans the distiller's
`min_repos` distinct canonical repositories, and SHALL otherwise wait in the
ledger. After authoring, the skill's **purpose key** SHALL be
`(task_family, action, target)`, read from its frontmatter. Each value MUST come
from the closed vocabulary in REQ "Skill Artifact". An out-of-vocabulary value
MUST NOT be coerced. The author run SHALL be retried once with the vocabulary
restated, and a second miss SHALL drop the candidate for the pass. The slug SHALL
be derived from the purpose key. The ledger SHALL map each evidence key to the
purpose key it produced. A distiller SHALL skip a candidate whose mapped purpose
key is suppressed for its target repo, or is active in some skill repo served to
every harness its evidence came from, before starting any model run.

#### Scenario: Below the distiller's threshold, a candidate waits

- **WHEN** a distiller has `min_repos = 3` and a candidate's evidence key spans
  two repositories
- **THEN** nothing is proposed, and the candidate stays in the ledger with its
  count

#### Scenario: Out-of-vocabulary is retried, then dropped

- **WHEN** an author run returns `task_family = "misc"` twice
- **THEN** the candidate is dropped for this pass with reason `vocabulary`, and
  no skill carries a fallback value

#### Scenario: Known lessons cost no model run

- **WHEN** a candidate's evidence key already maps to a purpose key that is
  suppressed for the distiller's target repo
- **THEN** the pass starts no model run for it

### Requirement: Distillation Runs Outside The Daemon

Linking, detection, deduplication, verification orchestration and proposal SHALL
run in `harness distill`, a client process. Model calls SHALL run as child
processes of `harness distill` (REQ "Isolated Model Runs"), never as daemon
runs. The daemon MUST NOT issue model or forge requests, MUST NOT hold model or
forge credentials, and MUST NOT write to any repository working tree, including a
skill repo's clones. `harness distill` SHALL hold the forge credential, read from
the distill table's `credential_file`. The credential MUST NOT be placed in the
environment of any process.

#### Scenario: Daemon makes no model or forge request

- **WHEN** a full distillation pass runs
- **THEN** every model request comes from a child of `harness distill`, every
  forge request comes from `harness distill`, and none comes from the daemon
  process

### Requirement: Isolated Model Runs

Each model run SHALL be a child process that `harness distill` starts directly,
running the configured `verifier` adapter's prompt command. For each child:

- Its environment SHALL contain only `PATH`, the locale variables, `TMPDIR`, a
  `HOME` set to a new directory inside the run's temporary directory, and the
  entries of `verifier_env_file`. Nothing SHALL be inherited from the daemon's or
  `harness distill`'s environment.
- Its working directory SHALL be a new temporary directory outside every Harness
  state directory, containing exactly the inputs listed for its role.
- Author, judge and adjudicator runs SHALL have no tools. Reconstructor and
  control runs SHALL have file read and write tools only, with no shell and no
  network tool. The Harness MCP bridge MUST NOT be wired into any model run.
- It SHALL run under the distiller's timeout.

| Run | Receives | MUST NOT receive |
|---|---|---|
| Author | the evidence bundle: redacted session excerpts, review text, merged hunks, cited spans; for a revision, the existing skill | forge credentials |
| Reconstructor | the skill record, the task statement, a clean-room snapshot at the base SHA | the merged diff, later history, review text, the pull request's identity |
| Judge | the merged hunks, the reconstructed hunks, the skill's summary render | the full skill record, sessions |
| Adjudicator | the judge's reason, both sets of hunks, the full skill record | sessions |
| Control | the task statement, the same clean-room snapshot | the skill record |

#### Scenario: No inherited environment

- **WHEN** `harness distill` runs with a forge token and `HARNESS_SOCKET` in its
  environment and starts a model run
- **THEN** the child's environment contains neither, and contains only the
  allowlisted variables and the entries of `verifier_env_file`

#### Scenario: Reconstructor never receives the answer

- **WHEN** a reconstructor run starts
- **THEN** its working directory contains no `.git`, and no file whose contents
  match the post-merge version of any cited file

#### Scenario: Judges have no tools

- **WHEN** a judge run is started with the Claude Code adapter
- **THEN** its argv disables all tools, and carries no MCP configuration

### Requirement: Blind Reconstruction Verification

Verification SHALL run only after deduplication has cleared the candidate.
Verification SHALL:

1. Build a clean room with `git archive` of the base SHA from the distiller's
   proposal clone, containing no repository metadata. If a cited file is absent
   from the archive, record `not_replayable` and stop.
2. Take the task statement from the linked issue's body, or, when there is no
   linked issue, from the session's first user message, redacted. Record which
   was used. If the statement contains any non-blank line of the merged hunks
   verbatim, record `task_leak: true`.
3. Run the reconstructor on the cited files.
4. Audit the reconstructor's transcript. Resolve every path in its tool-call
   inputs against the clean room, including relative `..` paths. Do not rely
   only on agent-trace's `OutsideTouch`. A path outside the clean room, an
   invocation of the `harness` CLI or of an `mcp__harness__*` tool, or any
   fetch, clone or forge call SHALL mark the run contaminated.
5. Run the judge. `equivalent: true` SHALL pass directly. Anything else SHALL go
   to the adjudicator, and `keep: true` SHALL pass as adjudicated.
6. Run `make test` on the reconstruction only when the distiller sets
   `test_sandbox`. In that case, prefix the command with `test_sandbox` and use
   the model runs' environment allowlist. Otherwise, record that tests were not
   run. `make test` MUST NOT run outside `test_sandbox`, and MUST NOT run in a
   process that holds the forge credential.
7. When the merged change is under `replay_max_lines` and `task_leak` is false,
   run the control. If its test result and judge verdict are at least as good as
   the reconstructor's, drop the candidate for this pass. Suppress its purpose
   key once the control has matched on two different evidence pull requests.

A candidate SHALL be proposed only with a fidelity pass (direct or adjudicated)
and a clean audit.

#### Scenario: A relative escape is contamination

- **WHEN** the reconstructor reads `../../evidence/bundle.json`
- **THEN** the run is recorded as contaminated, and the candidate is not
  proposed in this pass

#### Scenario: Calling harness is contamination

- **WHEN** the reconstructor's transcript contains an invocation of `harness`
- **THEN** the run is recorded as contaminated

#### Scenario: A failed rebuild is adjudicated, not rejected

- **WHEN** the judge rules a reconstruction not equivalent
- **THEN** the adjudicator decides whether the skill is faithful, and a
  `keep: true` verdict lets the candidate proceed as adjudicated

#### Scenario: Tests never run bare

- **WHEN** the distiller does not set `test_sandbox`
- **THEN** no `make test` is executed, and the dossier records that tests were
  not run

#### Scenario: One matching control does not suppress

- **WHEN** the control matches the reconstructor for evidence from one pull
  request
- **THEN** the candidate is dropped for this pass, and its purpose key is not
  suppressed

#### Scenario: A leaky task skips the control

- **WHEN** the task statement contains a line that appears verbatim in the
  merged hunks
- **THEN** the dossier marks `task_leak: true`, and no control run starts

### Requirement: Skill Artifact

A skill SHALL be a markdown file at `<path>/<slug>/SKILL.md` in a skill repo,
where `<path>` is the skill repo's configured path. Its frontmatter SHALL carry:
`name`; `description`, of at most 300 characters; `level` (`atomic`, `composite`
or `pattern`); `status` (`active` or `retired`); `task_family`, `action` and
`target`, each from the closed vocabularies below; `tags`; `purpose_key`;
`symptoms`, the literal strings from its evidence; `applies_to`, path globs; and
`provenance`. `provenance` SHALL record the scope counts, the first- and
last-seen dates, one entry per evidence item (signal, repository, pull request,
merge SHA, span, the cited file's blob SHA, and session count), and the
verification results. The body SHALL contain the sections *When to use*,
*Steps*, *Invariants*, *Failure modes*, *Do not* and *Evidence*. The steps SHALL
describe the procedure against the repository and stack, not against a
particular agent's tools.

The closed vocabularies are:

- `task_family`: aggregation, configuration, dispatch, initialization, io,
  lookup, orchestration, parsing, recovery, security, state_transition,
  transformation, troubleshooting, validation, verification, workflow.
- `action`: add, remove, rename, configure, pin, migrate, validate, retry,
  handle_error, order, escape, authenticate, release, test, document.
- `target`: dependency, build, ci, config, schema, api, cli, test, auth,
  network, filesystem, concurrency, logging, docs, release.

#### Scenario: Artifact carries its evidence

- **WHEN** a skill is authored
- **THEN** each evidence entry names a pull request, a merge SHA, a span and a
  blob SHA, and the verification results are present

#### Scenario: Lint rejects an incomplete record

- **WHEN** `harness skills lint` reads a skill missing the *Do not* section, one
  whose `description` exceeds 300 characters, or one whose `action` is outside
  the vocabulary
- **THEN** it exits non-zero, naming the file and the rule

### Requirement: Default-Branch Gate

Each skill repo SHALL have a serving clone at
`$XDG_STATE_HOME/harness/skills/<name>`, created and fast-forwarded only by
`harness skills sync`. For each serving clone, the daemon SHALL index only files
under the repo's `path`, on the checked-out default branch, whose `status` is
`active`. The daemon SHALL reindex a serving clone when it starts, when files in
the clone change, and when `harness skills sync` reports a fast-forward over the
control socket. If a clone's `HEAD` is not the default branch, or its working
tree is dirty, the daemon SHALL keep serving that repo's previous index, and
SHALL report the condition through `harness doctor`. Harness MUST NOT project
skill repo content at any status: the SPEC-0006 projection path SHALL exclude
every skill repo unconditionally. The daemon MUST NOT fetch, pull, commit or push
in any clone.

#### Scenario: Proposals are inert

- **WHEN** a skill exists only on a proposal branch or an open pull request
- **THEN** no search returns it, and no harness has it projected

#### Scenario: Merge and sync promote

- **WHEN** the proposal is merged and `harness skills sync` runs
- **THEN** the daemon reindexes that repo, the skill becomes searchable, and it
  is still projected into no harness

#### Scenario: A detached clone keeps the last good index

- **WHEN** a serving clone's `HEAD` points at a branch other than the default
- **THEN** searches return the previous index's results, and `harness doctor`
  warns

### Requirement: Pull Request Proposals

`harness distill propose` SHALL submit every new skill, revision and retirement
as a pull request:

- Target the distiller's `to` skill repo, on its canonical host, at
  `<path>/<slug>/SKILL.md`. `propose` MUST NOT modify any other file, including
  a repository's `AGENTS.md` or `CLAUDE.md`.
- Carry exactly one skill per pull request, on a branch named
  `<branch_prefix><slug>`, cut from a freshly fetched default branch in the
  distiller's proposal clone under its state directory.
- Put the marker `<!-- harness-distill key=<purpose_key> -->` and the evidence
  dossier in the body: the claim; an evidence table; the fidelity, test, control
  and audit results; the scope; and the context cost in characters of the
  summary and full renders.
- If an open pull request against the same skill repo already carries the same
  marker, push a new commit to it and MUST NOT open another, whichever distiller
  opened the first.
- Fetch the proposal branch before committing. Commit on top of the remote tip
  when the remote has commits the clone lacks. Fail with `ErrBranchDiverged`
  when the remote lacks the distiller's own earlier commits. `propose` MUST NOT
  force-push, merge, approve or enable auto-merge.
- Count every open distillation pull request in the target repository against
  the distiller's `max_open`. Candidates beyond the cap SHALL wait in the ledger,
  ranked by distinct repositories × occurrences × signal weight.
- If the skill repo is public, or has a public mirror, hold any candidate whose
  evidence includes a private repository, with reason `visibility`.
- Request `reviewers` and apply `labels` at creation. A reviewer equal to the
  forge token's own login SHALL fail the pass with a configuration error.

#### Scenario: Re-running does not duplicate

- **WHEN** two passes produce the same purpose key while the first pass's pull
  request is open
- **THEN** there is one pull request, and the second pass added a commit to it

#### Scenario: The cap counts every distiller

- **WHEN** another distiller already has `max_open` distillation pull requests
  open against the same repository
- **THEN** this distiller opens none there, and its candidates keep their rank in
  the ledger

#### Scenario: A reviewer's fix is built on, not overwritten

- **WHEN** a reviewer has pushed a commit to an open proposal branch
- **THEN** `propose` commits on top of it, and the reviewer's commit remains

#### Scenario: Private evidence never goes public

- **WHEN** the target skill repo is public and one evidence pull request is in a
  private repository
- **THEN** no pull request is opened, and the candidate waits with reason
  `visibility`

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
until the evidence count for it has doubled. A later proposal for a formerly
suppressed key SHALL link the rejected pull request. The pass summary SHALL flag
any distillation pull request that was merged by the forge token's own login.

#### Scenario: A rejection is remembered

- **WHEN** a distillation pull request is closed without merging, and the next
  pass sees the same evidence
- **THEN** no pull request is opened for that key

#### Scenario: New evidence reopens the question

- **WHEN** the evidence count for a rejected key has doubled since the rejection
- **THEN** a new pull request may be opened, and its body links the rejected one

#### Scenario: A self-merge is flagged

- **WHEN** a distillation pull request was merged by the distiller's own token
  login
- **THEN** the pass summary names it

### Requirement: Embedded Retrieval Index

Retrieval SHALL use an embedded full-text index that requires no external
service, no network access and no model weights. It SHALL record which skill
repo each skill came from. The index SHALL cover `name`, `description`,
`symptoms`, `tags` and `applies_to`, and SHALL stem terms so that morphological
variants match. It SHALL be a rebuildable cache: deleting it and reindexing from
the serving clones MUST restore equivalent behavior.

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
  unattributed caller SHALL search only skill repos whose `serve_to` is `["*"]`.
  It SHALL take a query and an optional `paths` list. It SHALL return
  identifiers, qualified by skill repo, and descriptions only: 3 by default and
  at most 5. A skill whose `applies_to` matches any given path SHALL rank above
  an otherwise equal skill.
- `get_skill` SHALL return one skill. By default it SHALL return a summary render
  (*When to use*, *Steps*, *Invariants*, *Do not*) of at most
  `summary_max_chars`. `render: "full"` SHALL return the whole file.
- The tool descriptions SHALL tell callers to search while planning and while
  reviewing a concrete draft or diff, and to supply several phrasings, including
  any literal error text.
- Each active skill SHALL also be available as the MCP resource
  `harness://skills/<repo>/<slug>`.

Each retrieval SHALL be counted per (skill repo, skill), together with the
calling harness and run, in daemon state.

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

### Requirement: Revisions

If a skill's purpose key equals that of an active skill in the same skill repo,
the author run SHALL receive the existing skill, and `propose` SHALL submit the
result as a change to the existing file, never as a new file. No model SHALL
decide whether new evidence contradicts an existing skill: the reviewer judges
the diff. Retiring one skill in favour of another SHALL happen only through a
pull request that a reviewer merges. Two active skills on one skill repo's
default branch MUST NOT share a purpose key, and `harness skills lint` SHALL
enforce this.

#### Scenario: Same purpose updates, not duplicates

- **WHEN** a candidate's purpose key matches an active skill
- **THEN** the pull request modifies that skill's file, and no second file is
  added

#### Scenario: Lint catches a duplicate key

- **WHEN** two active skills on the default branch share a purpose key
- **THEN** `harness skills lint` exits non-zero, naming both files

### Requirement: Staleness Re-Verification

On each pass, `harness distill` SHALL compare the recorded blob SHA of every file
cited by an active skill in its target repo against the default branch of the
evidence repository, using one tree listing per repository. A skill SHALL become
stale when a cited file is gone, or when the lines its span covers have changed.
At most `max_reverify` stale skills (default 2) SHALL be re-verified per pass,
oldest first. A skill that fails re-verification SHALL produce a revise or
retire pull request.

#### Scenario: Deleted evidence triggers re-verification

- **WHEN** a file cited in a skill's evidence has been deleted on its
  repository's default branch
- **THEN** the skill is re-verified against current code, and a failure opens a
  retire pull request

#### Scenario: Re-verification is bounded

- **WHEN** a pass finds five stale skills and `max_reverify` is 2
- **THEN** two are re-verified, and three wait for a later pass

### Requirement: Retrieval-Count Retirement

An active skill SHALL become eligible for retirement only after
`retire_grace_days` have passed since its merge, and only if it has had no
retrievals within `retire_window_days`. A skill still inside its grace period
MUST NOT be retired. Retirement SHALL be a pull request setting
`status: retired`. The file SHALL remain. Retrieval counts SHALL be stored in
daemon state, separate from the index. If they are lost, every skill SHALL be
treated as newly promoted.

#### Scenario: Grace period protects new skills

- **WHEN** a skill merged within `retire_grace_days` has zero retrievals
- **THEN** no retire pull request is opened

#### Scenario: Lost counts fail safe

- **WHEN** the retrieval-count store is missing at startup
- **THEN** no skill becomes eligible for retirement until `retire_grace_days`
  have passed again

### Requirement: Skill Repos

A skill repo SHALL be declared by a `[skill_repo.<name>]` table with `remote`
(required), `path` (default `skills`) and `serve_to` (default `["*"]`).
`serve_to` SHALL be a list of harness selectors. The selector `"*"` on its own
SHALL match every harness. Any other selector SHALL be a glob over qualified
harness names in which `*` does not match `/`. Serving settings SHALL be read
from a `[skills]` table: `summary_max_chars` (default 1000), `retire_grace_days`
(default 30) and `retire_window_days` (default 60). Skill repo and `[skills]`
tables SHALL be accepted in global configuration only. With no skill repo
declared, the facade SHALL NOT register the skill tools, and no index file SHALL
be created.

#### Scenario: Project files cannot declare skill repos

- **WHEN** a project `harness.toml` contains a `[skill_repo.*]` table
- **THEN** the load fails with an error naming the file and the table

#### Scenario: The bare star matches project harnesses

- **WHEN** a skill repo has `serve_to = ["*"]`, and `reduit/agent` calls
  `search_skills`
- **THEN** that repo is searched

#### Scenario: Inert by default

- **WHEN** no skill repo is declared
- **THEN** `search_skills` and `get_skill` are not registered, and no index file
  exists

### Requirement: Distillers

A harness SHALL become a distiller by carrying a `[harness.<name>.distill]`
table. The harness MUST be a triggered `command` one-shot (ADR-0023) whose argv
runs `harness distill run <name>`. The table has:

- `from`: harness selectors, with the semantics of REQ "Skill Repos", naming
  whose sessions count.
- `to`: the name of one skill repo.
- `credential_file`: required. A path to the forge token, read only by
  `harness distill`.
- `reviewers`: required, and non-empty.
- `evidence_repos`: owner/name globs naming the canonical repositories evidence
  may come from. The default is the owner of `to`'s remote.
- `min_repos`: default 1.

Optional per-distiller policy: `max_open` (default 3), `max_candidates` (default
5), `max_reverify` (default 2), `replay_max_lines` (default 400),
`revert_window_days` (default 7), `labels`, `branch_prefix`, `verifier`,
`verifier_env_file` and `test_sandbox`.

Loading SHALL fail when:

- `to` names an undeclared skill repo;
- an exact global `from` name is undeclared or lacks `harvest_trajectory`;
- a distiller lists itself;
- `reviewers` is empty, or `credential_file` is unset;
- the harness carrying the table is not a triggered `command` one-shot;
- the table appears in a project `harness.toml`.

Project-qualified names and globs SHALL be resolved at each pass, and the pass
summary SHALL list the harnesses each selector matched, including none.

#### Scenario: A dangling target fails the load

- **WHEN** a distiller sets `to = "go-stack"` and no `[skill_repo.go-stack]`
  exists
- **THEN** the configuration fails to load, naming the distiller and the missing
  skill repo

#### Scenario: A non-harvesting source fails the load

- **WHEN** a distiller's `from` lists an exact global harness name whose
  `harvest_trajectory` is false
- **THEN** the configuration fails to load, because that source would silently
  contribute nothing

#### Scenario: A prompt harness cannot distill

- **WHEN** a `claude-code` harness carries a `distill` table
- **THEN** the configuration fails to load, naming the required `command` kind

#### Scenario: A clone cannot name itself into evidence

- **WHEN** a cloned repository declares project `reduit`, matches the `from`
  glob `reduit/*`, and its remote is outside `evidence_repos`
- **THEN** its sessions link to nothing, and contribute no evidence

#### Scenario: An empty glob is visible

- **WHEN** a pass runs and the `from` glob `spotter/*` matches no registered
  harness
- **THEN** the pass summary reports that selector as matching nothing

### Requirement: Error Handling Standards

All operations that can fail MUST follow structured error handling:

- Errors MUST be wrapped with context at each layer boundary (for example,
  "distill: link: resolve canonical repo for ~/src/reduit: no canonical topic").
- Sentinel errors MUST be defined at minimum for: skill not found, skill repo
  not found, index unavailable, malformed frontmatter, session not linked, run
  contaminated, not replayable, and branch diverged.
- Errors MUST NOT be silently swallowed. Every error MUST be returned, logged
  with context, or explicitly handled with a documented reason for suppressing
  it.
- Errors MUST be reported with structured logging (key-value pairs, not string
  interpolation).

#### Scenario: A malformed skill does not break the index

- **WHEN** one skill has frontmatter that cannot be parsed during a reindex
- **THEN** the other skills index successfully, and a warning names the file
  and the cause

#### Scenario: One bad candidate does not stop a pass

- **WHEN** verification of one candidate fails with an error
- **THEN** the pass records the error against that candidate and continues with
  the next

### Requirement: Database Operation Standards

All operations against the retrieval index, the retrieval-count store, the
provenance records and the distill ledger MUST follow structured data access:

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

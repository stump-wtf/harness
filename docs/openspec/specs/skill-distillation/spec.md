---
status: draft
date: 2026-07-27
implements: [ADR-0012]
requires: [SPEC-0005, SPEC-0006]
---

# SPEC-0007: Cross-Harness Skill Distillation

> **Not yet implemented.** Design-stage; no distiller or learned-skill store exists today. See harness issue #3.

## Overview

Detection of patterns recurring across independent harnesses, and the **learned
skill tier** they are promoted into: markdown on disk, never projected, reachable
only through a search tool backed by an embedded full-text index. See
**ADR-0012**.

Detection clusters literal error strings and repeated failed tool calls — a
count, not a judgment — and the promotion gate is *scope*: a pattern in one
project leaves as a pull request against that repo, a pattern across several
becomes a learned skill. The authoring step runs in a **distiller harness**, not
in the daemon, which never calls a model and never writes to a repository.

Retrieval is `modernc.org/sqlite` FTS5 with `bm25()` — pure Go, no CGo, no model
weights, no external service.

## Requirements

### Requirement: Pattern Detection Inputs

Detection SHALL operate on literal error strings and repeated failed tool calls
extracted from trajectories obtained through the SPEC-0006 read-only interface.
Detection MUST NOT require a language model. Only harnesses that have opted into
trajectory harvesting SHALL contribute. Each detected occurrence SHALL retain
the project provenance of the harness that produced it.

#### Scenario: Detection needs no model

- **WHEN** a detection pass runs over available trajectories
- **THEN** it completes without issuing any model request

#### Scenario: Non-harvesting harnesses contribute nothing

- **WHEN** a harness has not opted into harvesting
- **THEN** none of its output appears in any cluster

### Requirement: Cross-Project Promotion Gate

A cluster SHALL be classified by the number of **distinct projects** it appears
in. A cluster confined to one project SHALL be treated as project knowledge and
SHALL NOT produce a learned skill. A cluster spanning at least the configured
threshold of distinct projects SHALL be eligible for the learned tier. The
threshold SHALL be configurable.

#### Scenario: Single-project pattern stays local

- **WHEN** a cluster's occurrences all carry the same project provenance
- **THEN** no learned skill is created for it

#### Scenario: Cross-project pattern is eligible

- **WHEN** a cluster spans at least the configured number of distinct projects
- **THEN** it becomes eligible for authoring into the learned tier

### Requirement: Distillation Runs Outside The Daemon

The detection and authoring steps SHALL run in a supervised harness, not in
the daemon. The daemon MUST NOT issue model requests, MUST NOT hold model
credentials, and MUST NOT write to any repository working tree. The daemon's
only obligations are supervising the distiller, answering read-only trajectory
queries, and serving the retrieval tools.

#### Scenario: Daemon issues no model requests

- **WHEN** distillation runs end to end
- **THEN** every model request originates from the distiller harness and none
  from the daemon

#### Scenario: Daemon writes no repositories

- **WHEN** a project-scoped finding is produced
- **THEN** the change reaches the repository as a pull request authored by the
  distiller, and no daemon code path writes to a working tree

### Requirement: Learned Skill Artifact

An authored learned skill SHALL be written as markdown to a Harness-owned
directory under the state directory, one directory per skill. Its frontmatter
SHALL carry a `status` field, a `symptoms` list containing the literal strings
that formed its cluster, and provenance recording the contributing trajectories,
the distinct project count, and first- and last-seen timestamps. Learned skills
MUST NOT be written into any project repository working tree or into a user's
dotfiles; the Harness-owned learned store is itself git-tracked by design.

#### Scenario: Artifact carries its evidence

- **WHEN** a learned skill is authored
- **THEN** its frontmatter lists the literal error strings that formed the
  cluster and the number of distinct projects observed

#### Scenario: Learned tier is Harness-owned

- **WHEN** a learned skill is written
- **THEN** the path is inside the Harness state directory and no file outside it
  is modified

### Requirement: Two-Channel Gate

A learned skill with `status: proposed` MUST NOT be projected into any harness
and MUST NOT be indexed for retrieval. Only a skill whose `status` is `promoted`
SHALL be indexed. Learned skills MUST NOT be projected at any status — the
SPEC-0006 projection path SHALL exclude the learned tier unconditionally.

#### Scenario: Proposed skills are inert on both channels

- **WHEN** a skill is written with `status: proposed`
- **THEN** it appears in no harness's projected skill set and is returned by no
  search

#### Scenario: Promotion enables retrieval only

- **WHEN** a skill's `status` is changed to `promoted` and the index is refreshed
- **THEN** it becomes searchable, and it is still projected into no harness

### Requirement: Embedded Retrieval Index

Retrieval SHALL use an embedded full-text index requiring no external service,
no network access, and no model weights. The index SHALL cover at minimum the
skill name, description, and `symptoms` fields, and SHALL apply stemming so
morphological variants of a term match. The index SHALL be treated as a
rebuildable cache: deleting it and reindexing from the markdown files MUST
restore equivalent behavior.

#### Scenario: Search runs with no external dependency

- **WHEN** a search is performed on a machine with no other software installed
  and no network access
- **THEN** it returns results

#### Scenario: Literal symptom text matches

- **WHEN** a query contains a literal error string recorded in a promoted
  skill's `symptoms`
- **THEN** that skill is returned among the results

#### Scenario: Index is rebuildable from source

- **WHEN** the index is deleted and rebuilt from the markdown files
- **THEN** the same promoted skills are searchable with equivalent ranking

### Requirement: Search And Retrieval Tools

The learned tier SHALL be exposed through the SPEC-0005 facade as two tools: one
returning skill identifiers and descriptions for a query, and one returning a
single skill's full body by identifier. The search tool MUST NOT return full
bodies. Each promoted skill SHALL additionally be addressable as an MCP resource
so a human may reference one directly. The search tool's description SHALL
instruct callers to supply multiple phrasings including any literal error text.

#### Scenario: Search returns descriptions, not bodies

- **WHEN** a search matches several skills
- **THEN** the result carries identifiers and descriptions only

#### Scenario: Bodies are fetched individually

- **WHEN** a caller requests a skill by identifier
- **THEN** the full body is returned for that skill alone

#### Scenario: Promoted skills are addressable by humans

- **WHEN** a promoted skill exists
- **THEN** it is reachable as an MCP resource under a stable identifier

### Requirement: Supersession

When a newly authored skill contradicts or subsumes an existing learned skill,
it SHALL supersede rather than accompany it. Supersession SHALL take effect
only when the superseding skill is promoted; a `proposed` skill MUST NOT alter
the retrieval status of any existing skill. On promotion, the superseded skill
SHALL be removed from the retrieval index, SHALL record the identifier of the
skill replacing it in its frontmatter, and its `status` SHALL become
`superseded` — so a rebuilt index excludes it. Two learned skills describing
the same pattern MUST NOT both be retrievable.

#### Scenario: Superseding replaces rather than accumulates

- **WHEN** a new skill supersedes an existing one
- **THEN** only the new skill is retrievable and the old one records its
  successor

### Requirement: Retrieval-Count Retirement

The system SHALL record how often each promoted skill is returned by search. A
promoted skill SHALL become eligible for retirement only after a configurable
grace period has elapsed **since its promotion**, and only if it has accrued no
retrievals within a configurable window. A skill inside its grace period MUST
NOT be retired. Retirement SHALL remove the skill from the index; it MUST NOT
delete the markdown file. Retirement SHALL be recorded in the skill's
frontmatter, so that rebuilding the index from the markdown files does not
resurrect a retired skill.

#### Scenario: Grace period protects new skills

- **WHEN** a skill was promoted within the grace period and has zero retrievals
- **THEN** it is not eligible for retirement

#### Scenario: Unused skills retire without data loss

- **WHEN** a skill past its grace period accrues no retrievals within the window
- **THEN** it is removed from the index and its markdown file remains on disk

### Requirement: Error Handling Standards

All error-producing operations MUST follow structured error handling:

- Errors MUST be wrapped with contextual information at each layer boundary
  (e.g., "distillation: index refresh: open learned store: permission denied")
- Sentinel errors MUST be defined for domain-specific failure modes callers need
  to distinguish programmatically — at minimum: skill not found, index
  unavailable, and malformed frontmatter
- Silent error swallowing MUST NOT occur — every error MUST be returned to the
  caller, logged with sufficient context, or explicitly handled with a
  documented reason for suppression
- Structured logging MUST be used for error reporting (key-value pairs, not
  string interpolation)

#### Scenario: A malformed skill does not break the index

- **WHEN** one learned skill has unparseable frontmatter during a reindex
- **THEN** the remaining skills index successfully and a warning names the
  offending file and cause

### Requirement: Database Operation Standards

All operations against the retrieval index MUST follow structured data access
patterns:

- Transactions MUST be used for multi-step mutations that require atomicity,
  including a full reindex
- Connection lifecycle MUST be explicitly managed — connections MUST be released
  after use, with timeouts configured
- Query parameters MUST use parameterized queries — string interpolation into
  queries MUST NOT occur, including for user- or agent-supplied search terms

#### Scenario: Reindex is atomic

- **WHEN** a reindex fails partway through
- **THEN** the previous index contents remain queryable and no partial state is
  visible

#### Scenario: Search terms are parameterized

- **WHEN** a search query contains characters significant to the query language
- **THEN** they are bound as parameters and cannot alter the query structure

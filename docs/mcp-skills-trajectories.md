# Agent awareness: MCP, shared skills, and distilled trajectories

> **Status:** design exploration — the historical record of the conversation
> that preceded the ADRs, kept for its reasoning trail.
>
> **Date:** 2026-07-27
>
> **Superseded by** [ADR-0010](adrs/adr-0010-local-mcp-surface.md),
> [ADR-0011](adrs/adr-0011-agent-adapters.md), and
> [ADR-0012](adrs/adr-0012-cross-harness-distillation.md) (with SPEC-0005,
> SPEC-0006, and SPEC-0007). Where this record and the ADRs disagree — e.g.
> the Cairn review gate below, which the ADRs generalized to plain human
> review — the ADRs win.

Three proposals, and how they turned into one architecture.

## The governing constraint

[`CLAUDE.md`](../CLAUDE.md) and the README both commit to the same rule:

> The daemon (`harness daemon`) is deliberately agnostic about what runs inside
> a harness. Keep it that way; agent-awareness bolts on later as a detector.

Every proposal below presses on that rule. The test applied throughout: **does
this need the daemon to know what's inside a harness, or only that it's a
process?** Where the answer is "what's inside," the capability belongs in an
adapter or in a harness — never in the daemon core.

Two corollaries that fall out and are load-bearing everywhere below:

- **The daemon never calls an LLM.** [ADR-0008](adrs/adr-0008-security-and-secrets.md)
  deliberately fences the daemon away from the secret backend. An LLM client in
  a supervisor is a category error, and it would drag credentials in with it.
- **The daemon never writes to your repos or your dotfiles.**
  [ADR-0006](adrs/adr-0006-configuration-and-profiles.md) fought to keep the
  global config hand-authored and chezmoi-tracked. The same reasoning applies to
  source trees.

---

## 1. MCP — two separate ideas

### 1a. An MCP facade over the control plane

[`internal/protocol/messages.go`](../internal/protocol/messages.go) already
defines a clean verb set — `list`, `describe`, `logs`, `start`, `stop`,
`restart`, `project_up`, `project_down` — consumed identically by the CLI and
the TUI. An MCP server is simply a third thin client under
[ADR-0002](adrs/adr-0002-daemon-client-architecture.md). No daemon changes.

What it buys: the product statement becomes literal. `systemctl` for your
agents, *callable by the agents*. One harness notices a sibling is flapping and
restarts it.

### 1b. Harness as a local MCP broker — the stronger idea

Today every harness spawns its own copy of every stdio MCP server. Twelve
harnesses running twenty servers is 240 processes serving twelve consumers.

Define them once in `harness.toml`; the daemon supervises them and fans out one
multiplexed endpoint into every harness. **This is the same job the daemon
already does** — own a process, own its lifecycle — just over stdio instead of a
PTY. It stays agnostic about what the servers do.

Side-loaded **prompts** come free: MCP has a `prompts` primitive, so a prompt
defined once is a slash command in every harness.

### The security position

[ADR-0008](adrs/adr-0008-security-and-secrets.md) already concedes that "anyone
who can attach can drive the agent." Exposing write ops over MCP generalizes
that: any prompt injection into any harness's *output* can drive the fleet, and
because these are background agents nobody is watching when it happens.

**Decision: accept it, deliberately.** The stated goal is removing a human from
loops they add no value to; a confirmation gate on every action re-adds the
human. Guardrails are handled upstream at the routing layer (Switchboard),
not by making the fleet ask permission.

One honest gap, recorded rather than resolved: Switchboard gates *inbound work
routing* — `create_for` requires a human-approved friend edge. It does not sit
in the path of an agent calling `harness_stop` on a sibling. If fleet control
should be gated too, the move is to route write ops through Switchboard as
todos rather than direct MCP calls.

---

## 2. Skills — the format was never the problem

Skills already are portable Markdown with frontmatter. What isn't portable is
**discovery**, which is per-tool: Claude Code reads `~/.claude/skills` and
`.claude/skills`, Crush reads its own paths, Codex wants `AGENTS.md`.

### Why globbing `$HOME` is the wrong shape

Measured on one working machine (depth 8, excluding `node_modules`/`.git`):

- **407 `SKILL.md` files** under `$HOME`
- `ops-runbook`, `playbook`, `ops-check`, `app-catalog`, `stack-guide` each
  appear **14 times**; `crush-config`, `crush-hooks`, `jq` **12 times** — the
  same skills sitting in a dozen worktrees of the same repos

A flat glob yields ~5× duplication and no way to tell which copy wins.

### Configured paths instead

Adapter-supplied defaults per provider, plus user-added paths in
`harness.toml`. You never walk `src/`, so the duplication never arises.

```toml
# additive by default; one escape hatch for full control
skill_paths = ["~/work/team-skills"]
# use_default_skill_paths = false
```

Precedence, lowest to highest:

```
adapter defaults → global skill_paths → project skill_paths → project-local dirs
```

Nearest wins on a name collision; shadowed copies stay listable so you can debug
which one won. `skill_paths` is legal in **both** the global and project files —
explicitly *not* joining `[server]`/`[profile.*]` on ADR-0009's global-only list,
since a repo shipping its own skills is the whole point.

`$PROJECT_ROOT` needs no new machinery: ADR-0009's up-walk from `cwd` already
defines it, and the daemon already tracks per-harness provenance via
`HarnessInfo.Project`.

---

## 3. Trajectories — harvest, don't judge

The daemon already owns every PTY and tees output to disk
([ADR-0007](adrs/adr-0007-state-persistence-scrollback.md)). That is a
tool-agnostic trajectory nobody else captures.

But raw ANSI scrollback is a poor substrate — it's the *rendered screen*:
spinners, repaints, cursor moves. Meanwhile ~1.1 GB across 1,738 JSONL
transcripts already sits in `~/.claude/projects` in a far better-structured form.

**So harvesting means detecting the harness's native transcript and indexing
that**, falling back to the ring only when there isn't one. The PTY is the
differentiator and the worst source, simultaneously.

### The unified loader

Trajectory discovery and skill discovery are the same question asked twice, so
they get one adapter interface. Given a harness, an adapter answers:

1. **Where do skills come from?** (this tool's native discovery paths)
2. **Where do skills go?** (the dir to project the merged set into before exec)
3. **Where does this tool write its trajectory?**

`claude-code`, `crush`, `codex`, `generic`. The daemon holds a registry and
knows nothing about any entry — structurally identical to the existing
`backend = "tmux" | "native"` field. So: `harness = "claude-code"` as the key
itself — required, with no inference and no default. This is the detector
`CLAUDE.md` promised.

### Distillation: measure repetition, not quality

"Was this session good?" needs an LLM judging 1.1 GB and is subjective. "Did N
independent harnesses hit the same thing?" is a frequency count.

**Start dumber than feels right: cluster on literal error strings and repeated
failed tool calls.** Greppable, cross-harness comparable, no model required.

**The cross-*project* threshold is the entire product.** A pattern recurring in
one repo is project knowledge; recurring across six unrelated repos it is *stack*
knowledge. Harness is the only thing that can tell them apart, because it is the
only thing that sees every harness at once. Same moat as the broker.

This works as well as it does because the fleet is homogeneous — one stack
across nearly every repo. On a heterogeneous fleet the same algorithm produces
mush. That's not a reason not to build it; it's a reason to know which you're
building.

### Where output goes, by scope

| Scope | Destination | Gate |
|---|---|---|
| Recurs in one project | **PR** against that repo's `CLAUDE.md`/`AGENTS.md` | Human review + merge |
| Recurs across projects | **Learned tier**, a Harness-owned directory | Cairn review before promotion |

The daemon writes to neither. The distiller does — and the distiller is *a
harness*, not a daemon subsystem.

### The mechanism

1. **Reads** trajectories via the Harness MCP server (read-only ops).
2. **Clusters** on error strings, grouped by project provenance. No LLM.
3. **Authors** — the only LLM step. Writes `SKILL.md`; the `description` gets
   the most care because it is the entire retrieval surface.
4. **Writes** to `$XDG_STATE_HOME/harness/skills/learned/<slug>/SKILL.md` with
   `status: proposed` and provenance frontmatter.
5. **Posts** to Cairn for review. Promotion flips the status field.

The daemon's total involvement: keep the distiller alive, answer read-only
trajectory queries. It never learns what a skill is.

### Storage and delivery are orthogonal

The learned tier is **markdown on disk, never projected, reachable only by
search**.

- **Disk is the substrate** — git history (which matters *most* for
  machine-written content), human and agent editability, reviewable diffs,
  grep, and the ability to rebuild any index from scratch. The files are truth;
  an index is a cache.
- **Search is the delivery** — nothing sits in context but the tool.

Projection puts every skill's `description` in context permanently. Correct at
15 skills, fatal at 200 — and ADR-0012's premise is a corpus that grows. Hence:

| | Delivery | Why |
|---|---|---|
| Hand-written (few, curated) | Filesystem projection | Always-visible descriptions are the feature |
| Distilled (many, growing) | MCP search tool | O(1) context regardless of corpus size |

`status: proposed` gates *both* channels at once — an unreviewed skill is never
projected and never indexed. Inert on both paths until a human promotes it. The
void is a git repo.

### Tools, not resources

MCP's split: tools are **model**-controlled, resources are **application**-
controlled, prompts are **user**-controlled. Background agents with nobody
watching means search must be model-invoked.

- `search_skills(query)` → ids + descriptions only (cheap)
- `get_skill(id)` → full body (expensive)

Two tools, mirroring native progressive disclosure. One tool returning bodies
would blow context on a five-hit query.

Additionally expose each promoted skill as a **resource** at
`harness://skills/learned/<slug>` — same files, human-controlled path, so a
person can `@`-mention one to review it.

### Retirement by retrieval count

Because learned skills are *served* rather than projected, retrievals are
countable. Two caveats:

- **Retrieval ≠ usefulness.** A skill can be returned and ignored. Start with
  retrieval count as the cheap 80% signal; the stronger signal is whether the
  agent acted on it — which the *next* harvest cycle can detect, so distillation
  generates its own quality feedback.
- **Cold start.** A freshly promoted skill has zero retrievals by definition.
  Gate on "zero retrievals in N weeks **since promotion**," with a grace period,
  or the LRU eats the corpus faster than distillation fills it.

---

## 4. The search substrate: no external dependency

**Requirement:** semantic search over skills without installing another app.

### What's ruled out

qmd is excellent but is TypeScript on Node/Bun (`better-sqlite3` +
`sqlite-vec`) — not embeddable in a Go binary. And true neural embeddings
in-binary means one of:

- **ONNX via CGo** — needs `libonnxruntime` at runtime. Still an external
  dependency, just one Homebrew won't manage.
- **Weights via `embed.FS`** — the binary is **11 MB** today; quantized MiniLM
  is ~23 MB, fp32 ~90 MB. The Homebrew formula builds from source.
- **Pure-Go transformer inference** — doesn't exist in production-grade form.

### What works

**Verified:** `modernc.org/sqlite` v1.54.0 ships **FTS5 with `bm25()`, pure Go,
no CGo**. One dependency, no external anything.

Embeddings buy exactly one thing: closing **vocabulary mismatch**. Three ways to
close it here without a model:

**1. The distiller already harvested the vocabulary.** Detection clusters on
literal error strings — so by the time a skill exists, we are holding the exact
text agents emit when they hit it. Write it into the document:

```yaml
---
name: sse-reconnect-go
description: Reconnecting server-sent event streams in Go...
symptoms:
  - "context deadline exceeded"
  - "http: response body closed"
---
```

Index `symptoms` as an FTS5 column and the match becomes *literal*. General
semantic search needs embeddings because query vocabulary is unpredictable;
here it was observed and recorded.

**2. The caller is a frontier model.** Have the calling agent expand its own
query — several phrasings, OR'd — before invoking `search_skills`. That's what
qmd's `expand:`/`hyde:` modes do, performed by a far better model than any
23 MB MiniLM, and *outside* the daemon. The daemon stays dumb because the
intelligence is already in the caller.

**3. Stemming is free.** `tokenize='porter unicode61'` collapses
reconnect/reconnecting/reconnected at zero cost.

**Reserved upgrade:** LSA/LSI — term-document matrix over the local corpus,
reduced via SVD (`gonum`, no CGo). Real semantic similarity with **no pretrained
weights**, because the model is derived from the documents. A few hundred lines,
milliseconds at this corpus size. Hold it until recall demonstrably misses.

### Net

```
modernc.org/sqlite (FTS5 + bm25, pure Go, verified)
  + symptoms[] harvested by the distiller
  + query expansion by the calling agent
  + porter stemming
```

No external app, no model weights, no CGo, +1 dependency, binary stays in the
low tens of MB. Brokering qmd via the MCP broker remains worthwhile for the
6,276-doc personal markdown corpus — a genuinely different problem. It just
stops being load-bearing for skills.

---

## Proposed ADRs

| ADR | Scope |
|---|---|
| **ADR-0010** | Local MCP broker + prompt side-loading; capability scoping for write ops |
| **ADR-0011** | Agent adapters: configured skill paths, projection, trajectory discovery |
| **ADR-0012** | Cross-harness distillation: frequency-gated, markdown-on-disk, search-delivered, retrieval-retired |

Sequencing: 0010 first — 0012's delivery path depends on the broker existing.

Orthogonal and deliberately unentangled: **schema governance**. The protocol
source ([`internal/protocol/messages.go`](../internal/protocol/messages.go))
documents minor bumps as "additive changes only" but nothing checks it;
[Omnist](https://github.com/omnist-dev/omnist) does decidable backward-
compatibility checking over TOML/JSON/YAML and could make that a CI gate. It's
Python, so CI-time only, never in the daemon. Worth its own ADR once the three
above land three new schema surfaces.

## Open questions

- Does the "hop" remain the signature interaction? For background agents the
  signature moment is arguably **review**, not hop — which cuts against the
  direction set in [`docs/design/`](design/). Worth deciding deliberately rather
  than drifting.
- Should fleet write ops route through Switchboard as todos rather than direct
  MCP calls?
- Does the learned tier want a derived claims ledger (provenance, retrieval
  counts, supersession edges) in something like
  [dialog-db](https://github.com/dialog-db/dialog-db)? It's explicitly
  experimental with no migration guarantees — survivable only because the ledger
  is rebuildable from the markdown.
- Supersession policy: new evidence contradicting a learned skill should replace
  it, not append. Unspecified.

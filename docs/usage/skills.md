---
title: "Skill search and retrieval"
sidebar_position: 4.75
---

# Skill search and retrieval

A **skill repo** is a git repository of skill files that the daemon serves to
agents. When you declare one or more skill repos in the global
`harness.toml`, the daemon keeps a serving clone of each, maintains an embedded
full-text index over the active skills, and the MCP facade registers two
read-class tools for your agents:

- **`search_skills`** — search the skill repos the calling harness may see.
  Returns identifiers and descriptions only, never bodies.
- **`get_skill`** — read one skill by id, as a short summary or the whole file.

Each active skill is additionally exposed as the MCP resource
`harness://skills/<repo>/<slug>`. Skills are never written into a harness's
directory: a caller searches, then reads what it needs
([ADR-0030](/decisions/adr-0030-grounded-skill-distillation)). The facade's
surface is [SPEC-0005](/specs/mcp-surface/spec); the full behaviour, including
the distillers that author skills, is
[SPEC-0007](/specs/skill-distillation/spec).

## Configuration

The tables are in [Configuration → Skill repos](./configuration#skill-repos):
`[skill_repo.<name>]` declares one repo (`remote`, `path`, `serve_to`,
`public`) and `[skills]` tunes serving (`summary_max_chars`,
`retire_grace_days`, `retire_window_days`). Both are global config only — a
project `harness.toml` is refused them, so a cloned repository cannot declare
its own skill repo.

With **no skill repo declared the facade registers no skill tools and creates
no index file**: the feature is inert by default.

## How skills are indexed

The daemon serves from a **serving clone** per repo at
`$XDG_STATE_HOME/harness/skills/<name>`. You move the clones; the daemon never
fetches, pulls, commits or pushes in them:

```bash
harness skills sync   # clone or fast-forward each serving clone, then reindex
harness doctor        # reports a missing, detached, or dirty clone
```

A clone that has diverged, is detached, or is dirty is left exactly as the
operator left it: the **default-branch gate** keeps serving the previous index
until the checkout is back on the default branch and clean, so searches stay
correct while the condition persists.

The index is an embedded SQLite full-text database
(`$XDG_STATE_HOME/harness/skills/index.db`) over `name`, `description`,
`symptoms`, `tags` and `applies_to`, with the `porter unicode61` tokenizer so
morphological variants match. It bundles no model weights and is a rebuildable
cache: deleting it and reindexing from the serving clones restores the same
behaviour. The vector half of the spec (embeddings, reciprocal-rank-fused
ranking, relation edges) is designed in SPEC-0007 and lands with the rest of
the spec.

## The tools

### `search_skills`

| Argument | Meaning |
| --- | --- |
| `query` | Free text. An exact lexical match — an error string, a tool name — is the strongest query there is |
| `paths` | Optional list of paths you are touching; skills whose `applies_to` matches a path rank above otherwise-equal skills |
| `limit` | Optional. 0 means the default of 3; at most 5 either way |

The answer carries repo-qualified identifiers (`<repo>/<slug>`) and
descriptions, nothing more. The tool's own description is the usage guidance:
search while planning, and again against a concrete draft or diff, and try
several phrasings.

### `get_skill`

| Argument | Meaning |
| --- | --- |
| `id` | `<repo>/<slug>`, or the skill's `harness://skills/<repo>/<slug>` URI |
| `render` | Optional. `""` or `"summary"` (default) or `"full"` |

The summary render keeps the *When to use*, *Steps*, *Invariants* and *Do not*
sections, capped at `summary_max_chars` (default 1000); a summary cut by the
cap says so, and a full render is never truncated. The
`harness://skills/<repo>/<slug>` resource serves the whole file.

Both tools are **read-class**: the default `mcp_allow = ["read"]` admits them
and they never require a `write` grant.

### Retrieval counting

Every search hit and every skill read is one retrieval, counted per
(repo, skill, harness, run) in daemon state
(`$XDG_STATE_HOME/harness/skills/counts.db`). The counts feed a later
retirement decision — a skill newer than `retire_grace_days` cannot be
retired, and the zero-retrieval window is `retire_window_days` — so a counting
failure is reported in the answer (`countError`) rather than dropped: the
answer stays intact, the gap is visible.

### Error codes

Failures carry a stable code, not a free-form sentence:

| Code | Meaning |
| --- | --- |
| `bad_request` | A skill id or render the tool does not understand |
| `skill_repo_not_found` | The id names a repo that is not declared |
| `skill_not_found` | The repo does not hold that skill |
| `not_permitted` | The caller's `serve_to` scoping does not admit the repo |
| `index_unavailable` | The index could not be read |
| `unknown_resource` | A resource URI outside the `harness://skills/` namespace |

## Who sees what

Scoping is by the repo's `serve_to`, checked against the caller the facade
attributes (SPEC-0005 REQ "Caller Identity"):

- The bare `"*"` matches every harness — unattributed callers included.
- Any other selector is a glob over qualified harness names in which `*` does
  not match `/`: `"reduit/*"` never reaches `"other/agent"` and never reaches
  an unattributed caller.
- An **unattributed** call — a session started outside Harness, or one whose
  token did not resolve — sees only repos whose `serve_to` contains `"*"`.
- A caller scoped out of **every** repo gets an empty answer, not an error.

`public` is not a serving scope: it records whether the repo is public and
matters to the distillers that propose into it.

## How an agent calls them

:::note The tools are built; the live endpoint ships with the gateway

The two tools and the per-skill resources are registered on the facade
whenever at least one skill repo is declared. Serving them over a live
endpoint arrives with the MCP gateway ([ADR-0035](/decisions/adr-0035-mcp-gateway)):
per-harness filtered endpoints, with a per-spawn token attributing every call,
is what makes `serve_to` and `mcp_allow` enforceable on a live connection.
Today the facade's control tools are reachable unattributed over the
`harness mcp` stdio bridge ([ADR-0054](/decisions/adr-0054-mcp-stdio-bridge-before-the-broker));
the skill tools are not among them yet. Until the gateway lands, the serving
side is fully live — the daemon clones, indexes, scopes and counts — and an
unattributed caller would see only the `serve_to = ["*"]` repos.

:::

The intended pattern once served is: plan, call `search_skills` with the
words of the task; when the work is concrete, search again with the draft,
the diff's paths, and any literal error text; then `get_skill` the hits that
look right, summary first, `render: "full"` only for the ones you act on.

## Observability

- `harness doctor` reports each declared repo's default-branch-gate state
  (missing, detached, dirty, reindexed with malformed files) and the fix for
  it, usually `harness skills sync`.
- A degraded index state and the search-degradation counter
  (`harness_skill_search_degraded_total`) arrive with the vector half.

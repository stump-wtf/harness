---
status: proposed
date: 2026-08-19
decision-makers: [joestump]
extends: [ADR-0009, ADR-0011]
related: [ADR-0002, ADR-0005, ADR-0007]
governs: [SPEC-0011]
---

# ADR-0017: Ephemeral scratchpad harnesses (`harness run`)

## Context and Problem Statement

Harness has two ways to have a supervised process: the global config
(ADR-0006, durable, dotfiles-tracked) and projects (ADR-0009, durable
Compose-style, repo-local). Neither answers the most casual gesture of all:
*"run me a `claude opus-5` for a bit, I'll throw it away when I'm done."*
Today that means either editing `harness.toml` (absurd for a throwaway) or
reaching for `screen`/`tmux`/`shpool` — which is exactly the class of tool
Harness replaces. How do we give Harness a zero-config, ad-hoc session that
needs no file, no project, and no ceremony — and that cleans up after itself?

## Decision Drivers

* **Zero ceremony.** Creating a scratchpad must not touch any file: no
  `harness.toml` edit, no project file, no `--name` required. `harness run
  claude opus-5` is the whole interface.
* **Genuinely ephemeral.** Unlike projects (durable until `down`/`rm` since the
  2026-08-19 ADR-0009 amendment), a scratchpad is throwaway by construction: it
  is never persisted to state.json and vanishes on daemon restart. The two
  concepts must not blur again — that ambiguity is what created this ADR.
* **Names are the daemon's problem.** The user should never have to invent a
  unique name; the daemon mints one (`claude-opus-5-x4yx`-shaped: a slug of the
  invocation plus a short random suffix) and the client prints it.
* **Full supervisor, not a stripped-down one.** A scratchpad gets PTY, attach,
  scrollback, logs — everything a configured harness has. `harness attach` on a
  scratchpad is the tmux-attach replacement.
* **Explicit teardown only.** A scratchpad runs until `harness rm NAME`
  (or daemon exit). Normal exit of the process leaves it registered (exit code
  inspectable) until rm — sessions don't silently disappear under you.
* **Daemon stays the supervisor** (ADR-0002/0005): the client verb pushes a
  definition over the control plane; there is no second execution engine.

## Considered Options

* **Option 1 — Synthetic "scratch" project.** Implement `run` as a
  `project_up` against a reserved project name (`scratch`). Maximum reuse; zero
  new daemon machinery.
* **Option 2 — A dedicated `scratch` provenance class + `scratch_run` op.** A
  third registration kind alongside global (config) and project (project:NAME):
  registered by a new control op, provenance-tagged `scratch`, excluded from
  persistence, torn down by the existing `remove` op.
* **Option 3 — Client-side temp config.** The client writes a throwaway
  harness.toml to a temp dir and project_ups it as a project rooted there.

## Decision Outcome

Chosen option: **Option 2 — a dedicated `scratch` provenance class**, because
it keeps the three lifetimes honestly distinct — global (config-owned, durable),
project (repo-owned, durable-until-down/rm), scratch (operator-owned, ephemeral)
— while reusing the supervisor, the `remove` op from ADR-0009's amendment, and
the attach/log planes wholesale. Option 1 was rejected because a synthetic
project would inherit project persistence semantics (or need special-casing to
avoid them) and would show up in `harness down`/`ps` project scoping; Option 3
because it smuggles a temp-file lifecycle into a gesture that must touch no
files.

### The gesture and the name

`harness run [flags] ARG...` maps its positionals onto the harness argv the
same way a `[harness.NAME]` table's `harness` + `args` keys would: the first
positional selects the harness kind (`crush`, `claude-code`, `codex`,
`generic`, with `claude` aliased to `claude-code` — the word people type —
and a bare unknown word treated as a `generic` command to run,
so `harness run claude opus-5` and `harness run htop` both work), the rest are
its args. The daemon mints the name: `<slug>-<suffix>`, where the slug is the
sanitized invocation (e.g. `claude-opus-5`) and the suffix is 4 random base36
characters, retried on collision with any registered name. The reply carries
the name; the client prints it, then attaches to it — **resolved 2026-08-22**:
`harness run` is `tmux new-session`, not `tmux new-session -d`, so the default
completes the gesture rather than leaving a second `harness attach` step
required. `--detach` opts out (the `-d` equivalent), as do `--json` and
piped/redirected stdio at either end, since none of those have a terminal to
attach to.
`--name` overrides the slug for people who care; uniqueness is still enforced
by the suffix.

### Lifecycle

`scratch_run` registers the supervisor with provenance `scratch` and starts it.
Scratchpads are **never written to state.json** — on daemon restart they are
gone, exactly like a tmux server dying takes its sessions. Because the
provenance value `scratch` doubles as the sentinel `Save` excludes, the
project name `scratch` is reserved (refused at `project_up`), so the
sentinel can never collide with a real project. Teardown is the
existing `remove` op (`harness rm`), which accepts any provenance-tagged
(registered) harness. A scratchpad whose process exits stays registered until
rm (state `exited`); the default restart policy for a scratchpad is `no`,
matching session semantics rather than service semantics.

### Consequences

* Good, because `screen`/`tmux`/`shpool`'s exact use case — "a terminal thing
  for a while, then gone" — becomes `harness run` (which drops you straight
  in, per the 2026-08-22 auto-attach resolution above) + rm, inside the one
  supervisor that already owns PTYs and scrollback. `--detach` is there for
  the rarer "start it and walk away" case.
* Good, because the durable/ephemeral split is now structural: persistence is
  keyed on provenance, and `scratch` provenance is excluded from Save by
  construction — no flag-day, no per-callsite discipline.
* Good, because `rm`, attach, logs, and the TUI list needed no new code paths —
  scratchpads are supervisors with a provenance tag.
* Bad, because a daemon restart silently discards every scratchpad (accepted:
  that is the semantic — and the same failure mode as the tmux server).
* Neutral, because `run`'s first-positional dispatch (kind vs. generic command)
  is a heuristic; an explicit `--kind` flag disambiguates the rare collision
  (e.g. a command literally named `crush` you did not mean as a kind).

### Confirmation

* SPEC-0011 formalizes `scratch_run`, name minting (slug + suffix, collision
  retry), the no-persistence invariant, restart policy, and the rm teardown as
  testable requirements.
* Acceptance tests: `run` registers and starts a uniquely-named supervisor with
  `scratch` provenance that `list` shows; state.json after any scratchpad
  lifecycle contains no scratch entry; a daemon restart leaves zero
  scratchpads; `harness rm` removes one; two identical `run` invocations never
  collide.

## Pros and Cons of the Options

### Option 1 — Synthetic "scratch" project

* Good, because project_up/project_down/remove are reused verbatim.
* Bad, because projects are durable since ADR-0009's amendment; a scratch
  project would either persist (wrong) or need special-casing in Save/Restore
  that amounts to implementing Option 2 anyway.
* Bad, because `harness down`/`ps` project scoping and the `<project>/`
  namespace would surface an operator-internal bookkeeping project to the user.

### Option 2 — Dedicated `scratch` provenance class

* Good, because each lifetime (config/project/scratch) is a first-class
  provenance with its own persistence rule, and `rm` needs no changes.
* Good, because the name is minted where uniqueness is checkable — under the
  registry lock.
* Neutral, because it adds a third branch wherever provenance is inspected
  (save, restore, list projection).

### Option 3 — Client-side temp config

* Good, because the daemon changes not at all.
* Bad, because the gesture then writes files (temp dirs leak, crash residue),
  inherits project persistence, and the "ephemeral" property becomes a lie the
  first time someone inspects state.json.

## More Information

* ADR-0009 (projects — the durable sibling; its 2026-08-19 amendment is why
  scratchpads must NOT be projects).
* ADR-0011 (the harness-kind enum and argv synthesis `run` reuses).
* SPEC-0011 (requirements and scenarios).

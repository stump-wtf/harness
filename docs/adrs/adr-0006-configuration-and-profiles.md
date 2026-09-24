---
status: accepted
date: 2026-07-18
decision-makers: [joestump]
related: [ADR-0002, ADR-0005, ADR-0007, ADR-0008]
---

# ADR-0006: Configuration and profiles — configurations of harnesses

## Context and Problem Statement

The pitch is *"hop into different **configurations** of AI harnesses."* The
predecessor's config is flat: one TOML file, one `[name]` table per harness, no
notion of a *set* of harnesses you switch between. We need to (a) preserve the
existing harness schema so nothing breaks, and (b) add first-class **profiles** —
named groups of harnesses — as the thing you "hop into."

We also need to decide **who owns the config**: the file (daemon reads it) or the
daemon (TUI writes it, file is an export)?

## Decision Drivers

* Don't churn the existing, well-liked harness schema (`cmd`/`args`/`workdir`/
  `env_file`/`restart_delay`/`tmux_socket`). Operators have working config files.
* Make "a configuration" a real, nameable, switchable object.
* Config should be **hand-editable** (operators edit TOML directly) *and*
  editable from the TUI (Huh forms), without the two fighting.
* Keep secrets out of the config (they already live in `env_file`).

## Considered Options

### Decision 1 — How profiles are expressed

* Option 1 — Profiles as tags on harnesses
* Option 2 — Profiles as explicit tables listing member harnesses
* Option 3 — Directory-per-profile

### Decision 2 — Who owns the config

* Option 1 — The file is truth; the daemon hot-reloads it
* Option 2 — The daemon is truth; the config file is a generated export
* Option 3 — A database is truth, with TOML import/export

## Decision Outcome

Chosen options: **Decision 1, Option 2 — Profiles as explicit tables**, and
**Decision 2, Option 1 — The file is truth; the daemon hot-reloads it**, because
TOML stays, profiles become first-class objects with their own metadata, and the
config remains a hand-editable, version-controllable file.

### Schema (backward compatible)

Harness tables are namespaced under `[harness.*]`, and the loader **accepts the
predecessor's bare `[name]` tables** as `[harness.name]` for compatibility (a
migration nicety, not a break).

```toml
# ~/.config/harness/harness.toml

# ── harnesses ─────────────────────────────────────────────
[harness.claude-src]
cmd = "claude"
args = ["--remote-control", "--dangerously-skip-permissions"]
workdir = "~/src"
# env_file, restart_delay, tmux_socket, backend …

[harness.crush-worker]
cmd = "crush"
args = ["--yolo", "--data-dir", "{workdir}", "--channels", "server:signal"]
workdir = "~/.local/share/crush-worker"
env_file = "~/.config/vault/secrets-static.env"

[harness.reduit-agent]
cmd = "claude"
args = ["--dangerously-skip-permissions"]
workdir = "~/src/reduit"

# ── profiles: named sets you "hop into" ───────────────────
[profile.default]
harnesses = ["claude-src"]
autostart = true            # daemon starts this profile's harnesses on boot

[profile.signal-ops]
description = "Headless agents wired to Signal"
harnesses = ["crush-worker", "claude-src"]

[profile.reduit]
description = "Everything for the reduit project"
harnesses = ["reduit-agent", "claude-src"]
```

### New per-harness keys (additive)

* `backend = "native" | "tmux"` — ADR-0003's escape hatch (default `native`).
* `description` — shown in the TUI list.
* `enabled` — whether the daemon autostarts it independent of profiles (optional;
  profiles are the primary autostart mechanism).

### What a "profile" means operationally

* A profile is a **named view + an autostart set**. Switching profiles in the TUI
  filters the dashboard to that profile's harnesses and (optionally) starts any of
  them that aren't running. It does **not** kill harnesses outside the profile —
  hopping profiles is non-destructive; you can run several profiles' harnesses at
  once. (A "focus mode" that stops others is a possible toggle, deferred.)
* A harness can belong to multiple profiles (it's just a name reference).
* `autostart = true` profiles are what the daemon brings up on start (ADR-0005).

### Authority: the file is truth, the daemon hot-reloads

* The daemon **watches** `harness.toml` with fsnotify and reloads on change. It
  watches the file's directory and filters on the file name, because tools such
  as chezmoi write through a temp file and rename; a 500 ms debounce coalesces a
  burst of writes. Files under the `[server] harness_d` directory
  (`harness.d/*.toml`) and per-harness `env_file`s are **not** watched.
* The watcher is on by default. `[daemon] watch_config = false` (or
  `HARNESS_WATCH_CONFIG=false`) turns it off. `SIGHUP` and `harness reload` always
  reload, watcher or not, and are how a `harness.d/` or `env_file` change is
  picked up.
* On reload, new harnesses appear (and start if they are in the autostart set —
  ADR-0014), edited fields apply on the harness's next (re)start, and removed
  harnesses are stopped and dropped.
* **TUI edits write back to the TOML** (via Huh forms → serialize → atomic write),
  then the reload path picks them up. The file stays the human-authoritative,
  version-controllable artifact, which matters because operators keep this kind
  of thing in dotfiles managers such as chezmoi. The daemon never becomes a config
  black box.
* Keep the config **valid-at-all-times**: writes are atomic (`write tmp + rename`),
  and a parse error on reload keeps the last-good config and surfaces the error
  rather than crashing.

### Consequences

* Good, because existing harness definitions need no changes; profiles are purely
  additive.
* Good, because "configurations" become a real, switchable, describable object —
  directly serving the product pitch.
* Good, because config stays a hand-editable, git-committable TOML file
  (dotfiles-friendly) while *also* being TUI-editable — no black-box daemon state
  for config.
* Good, because cross-references (a harness in several profiles) are trivial name
  refs.
* Bad, because two-way editing (file ↔ TUI) needs care: atomic writes, reconcile
  on external edit, and a clear rule that the file wins. We accept the small
  complexity for the big "still just a TOML in my dotfiles" win.
* Neutral, because `tmux_socket` only means anything under `backend = "tmux"`; we
  keep it for compat but it's inert for native harnesses.

### Confirmation

* A config containing only bare `[name]` harness tables loads, and each table
  appears as `harness.<name>`.
* Rewriting `harness.toml` (including via temp file + rename) reloads the daemon
  within the debounce window when `watch_config` is on; a syntax error leaves the
  running harness set unchanged and logs the failure.
* With `watch_config = false`, a file change does nothing until `SIGHUP` or
  `harness reload`.

## Pros and Cons of the Options

### Decision 1 — How profiles are expressed

#### Option 1 — Profiles as tags

* Good, because membership sits on the harness itself.
* Bad, because tags can't carry per-profile metadata (description, autostart) or
  an explicit ordering; weaker as a first-class object.

#### Option 2 — Profiles as explicit tables

* Good, because a profile is a named object that carries its own description and
  autostart flag.
* Good, because a harness joins several profiles by name reference.
* Bad, because membership lives away from the harness definition.

#### Option 3 — Directory-per-profile

* Good, because each profile is a self-contained file tree.
* Bad, because a harness shared by several profiles has to be duplicated or
  linked across directories.

### Decision 2 — Who owns the config

#### Option 1 — The file is truth, the daemon hot-reloads

* Good, because the config stays a file an operator can edit, diff, and commit.
* Bad, because TUI edits and external edits must be reconciled through the file.

#### Option 2 — The daemon is truth, the file is an export

* Good, because the daemon never has to reconcile an external edit.
* Bad, because it breaks the "it's a file in my dotfiles" property operators rely
  on and turns config into daemon state we'd have to export. Rejected.

#### Option 3 — A database is truth, TOML import/export

* Good, because structured queries and history come for free.
* Bad, because, like Option 2, the file stops being the source of truth. Rejected.

## More Information

* **Related ADR-0002** — the registry holds parsed config.
* **Related ADR-0005** — autostart profiles.
* **Related ADR-0007** — the daemon persists *runtime* state, not config,
  separately.
* **Related ADR-0008** — `env_file` is where secrets stay.
* **Related ADR-0014** — what a reload does with newly-introduced harnesses.

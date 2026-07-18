# ADR-0006 — Configuration & profiles ("configurations of harnesses")

- **Status:** Proposed
- **Date:** 2026-07-18

## Context and problem statement

The pitch is *"hop into different **configurations** of AI harnesses."* Today's
config is flat: one `harnessd.toml`, one `[name]` table per harness, no notion of
a *set* of harnesses you switch between. We need to (a) preserve the existing
harness schema and file so nothing breaks, and (b) add first-class **profiles** —
named groups of harnesses — as the thing you "hop into."

We also need to decide **who owns the config**: the file (daemon reads it) or the
daemon (TUI writes it, file is an export)?

## Decision drivers

- Don't churn the existing, well-liked harness schema (`cmd`/`args`/`workdir`/
  `env_file`/`restart_delay`/`tmux_socket`). People have working `harnessd.toml`s.
- Make "a configuration" a real, nameable, switchable object.
- Config should be **hand-editable** (Joe edits TOML directly) *and* editable from
  the TUI (Huh forms), without the two fighting.
- Keep secrets out of the config (they already live in `env_file`).

## Considered options

**On profiles:** (1) profiles as tags on harnesses; (2) profiles as explicit
tables listing member harnesses; (3) directory-per-profile.

**On authority:** (A) file is truth, daemon hot-reloads; (B) daemon is truth,
config file is a generated export; (C) DB is truth, TOML import/export.

## Decision outcome

**Chosen: TOML stays; add explicit `[profile.*]` tables (option 2); file is the
source of truth with hot reload (authority option A).**

### Schema (backward compatible)

Existing harness tables are unchanged. We namespace them under `[harness.*]`
going forward but **accept today's bare `[name]` tables** as `[harness.name]` for
compatibility (a migration nicety, not a break).

```toml
# ~/.config/harnessd/harnessd.toml

# ── harnesses ─────────────────────────────────────────────
[harness.claude-src]
cmd = "claude"
args = ["--remote-control", "--dangerously-skip-permissions"]
workdir = "~/src"
# env_file, restart_delay, tmux_socket, backend … all as today

[harness.crush-signal]
cmd = "crush"
args = ["--yolo", "--data-dir", "{workdir}", "--channels", "server:signal"]
workdir = "~/.local/share/crush-signal-channel"
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
harnesses = ["crush-signal", "claude-src"]

[profile.reduit]
description = "Everything for the reduit project"
harnesses = ["reduit-agent", "claude-src"]
```

### New per-harness keys (additive)

- `backend = "native" | "tmux"` — ADR-0003's escape hatch (default `native`).
- `description` — shown in the TUI list.
- `enabled` — whether the daemon autostarts it independent of profiles (optional;
  profiles are the primary autostart mechanism).

### What a "profile" means operationally

- A profile is a **named view + an autostart set**. Switching profiles in the TUI
  filters the dashboard to that profile's harnesses and (optionally) starts any of
  them that aren't running. It does **not** kill harnesses outside the profile —
  hopping profiles is non-destructive; you can run several profiles' harnesses at
  once. (A "focus mode" that stops others is a possible toggle, deferred.)
- A harness can belong to multiple profiles (it's just a name reference).
- `autostart = true` profiles are what the daemon brings up on start (ADR-0005).

### Authority: file is truth, daemon hot-reloads

- The daemon **watches** `harnessd.toml` (fsnotify) and reloads on change:
  new harnesses appear, edited fields apply on next (re)start, removed harnesses
  are stopped after confirmation/marked orphaned.
- **TUI edits write back to the TOML** (via Huh forms → serialize → atomic write),
  then the reload path picks them up. The file stays the human-authoritative,
  version-controllable artifact — which matters because Joe keeps this kind of
  thing in dotfiles/chezmoi. The daemon never becomes a config black box.
- Keep the config **valid-at-all-times**: writes are atomic (`write tmp + rename`),
  and a parse error on reload keeps the last-good config and surfaces the error in
  the TUI rather than crashing.

## Consequences

**Positive**

- Zero-churn for existing harness definitions; profiles are purely additive.
- "Configurations" become a real, switchable, describable object — directly serves
  the product pitch.
- Config stays a hand-editable, git-committable TOML file (dotfiles-friendly)
  while *also* being TUI-editable — no black-box daemon state for config.
- Cross-references (a harness in several profiles) are trivial name refs.

**Negative / costs**

- Two-way editing (file ↔ TUI) needs care: atomic writes, reconcile on external
  edit, and a clear rule that the file wins. We accept the small complexity for the
  big "still just a TOML in my dotfiles" win.
- `tmux_socket` only means anything under `backend = "tmux"`; we keep it for compat
  but it's inert for native harnesses.

**Rejected options**

- **Profiles as tags (opt 1)** — can't carry per-profile metadata (description,
  autostart) or an explicit ordering; weaker as a first-class object.
- **Daemon-owned / DB-of-record (authority B/C)** — breaks the "it's a file in my
  dotfiles" property Joe values and turns config into daemon state we'd have to
  export. Rejected; the file stays the source of truth.

## Related

ADR-0002 (registry holds parsed config), ADR-0005 (autostart profiles),
ADR-0007 (daemon persists *runtime* state — not config — separately),
ADR-0008 (`env_file` is where secrets stay).

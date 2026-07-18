# ADR-0003 — Terminal multiplexing: bake it in (keep tmux optional)

- **Status:** Proposed
- **Date:** 2026-07-18
- **This is the load-bearing decision.** Everything about the UX ceiling and the
  dependency footprint hangs off it.

## Context and problem statement

Today, tmux *is* the product's spine: each harness is a detached tmux session on
a shared socket, the restart loop runs inside the pane, and `attach` is literally
`tmux attach`. Joe's steer: *"tmux feels good to me, but if we can just bake it in
that's great."*

The question: does the new daemon **shell out to tmux** for per-harness terminals
and attach, or **own the PTY + terminal emulation itself** (`x/xpty` + `x/vt`)?

## Decision drivers

- **Embedded, styled UI.** We want harness terminals rendered *inside* our TUI —
  a sidebar of harnesses, a framed terminal pane, a status bar — not a raw
  handoff to a fullscreen tmux client. That's hard to do well when tmux owns the
  screen.
- **Dependency footprint.** tmux is an external runtime dep with its own config,
  version quirks, and socket semantics. A single static binary is a goal.
- **Control over the "hop" UX.** Instant switch between harnesses, shared attach,
  in-TUI scrollback search, per-harness activity indicators — all easier when we
  own the emulator.
- **Robustness we'd have to re-earn.** tmux has 15+ years of PTY/resize/reflow/
  copy-mode hardening. Rolling our own means re-earning some of that.
- **Remote attach.** Must compose with ADR-0004 (Wish/SSH). A Bubble Tea program
  served over SSH can't cleanly "become" a tmux client.

## Considered options

### Option A — Shell out to tmux (evolve today's model)

Daemon runs `tmux new-session -d` per harness; `attach` execs `tmux attach`.

- ➕ Battle-tested PTY/scrollback/reflow/copy-mode for free.
- ➕ Least new code; the risky terminal-emulation problem is already solved.
- ➕ Users who love tmux keep their muscle memory and config.
- ➖ Our TUI can't *embed* a tmux pane — attach means yielding the whole screen to
  a tmux client. The dashboard and the terminal can't coexist in one styled view.
- ➖ Remote attach over Wish is awkward: we'd be nesting a tmux client inside an
  SSH-served Bubble Tea app, or bypassing our TUI entirely.
- ➖ External dependency, external config surface, version drift, socket edge cases
  (the current code already carries `-L harness` socket handling).
- ➖ Two sources of truth (tmux's session state vs our registry) to reconcile.

### Option B — Bake in the multiplexer (`x/xpty` + `x/vt`) ✅ chosen

Daemon allocates a PTY per harness via `x/xpty`, feeds the master's output into an
`x/vt` **virtual terminal emulator** (which maintains screen + scrollback and is
`Draw`able), and forwards client keystrokes into the emulator's `InputPipe`.
"Attach" = subscribe to that emulator's screen; the TUI renders it as a Lip Gloss
pane.

- ➕ **Terminals live *inside* our UI.** Sidebar + framed terminal + status bar in
  one coherent, styled surface. The "hop between harnesses" UX becomes native and
  instant. Multi-pane (several agents on screen) becomes *possible* later.
- ➕ **Single static binary.** No tmux runtime, no external config.
- ➕ **Remote is symmetric with local** — the SSH-served Bubble Tea client renders
  the same `vt` screen the local client does (ADR-0004). One code path.
- ➕ One source of truth (the daemon's emulator == the state).
- ➕ We control scrollback, search, activity detection, resize policy, recording.
- ➖ **We re-earn terminal-emulator robustness.** `x/vt` is young and experimental;
  edge cases (wide/combining chars, obscure escape sequences, mouse reporting,
  reflow-on-resize) may bite. This is the real cost.
- ➖ More novel engineering than any other part of the system.
- ➖ Resize semantics are on us: when two clients attach at different sizes, whose
  dimensions win? (Policy needed — see below.)

### Option C — Hybrid: native by default, tmux as an optional backend ✅ recommended shape

Define a `Backend` interface — `Spawn`, `Resize`, `Subscribe(screen)`, `Input`,
`Signal`, `Kill`. Ship a **native backend (Option B)** as the default, and a
**tmux backend (Option A)** behind a per-harness/global `backend = "tmux"` config
knob for people who want their tmux world, or as a fallback if `vt` bites us on
some exotic program.

## Decision outcome

**Chosen: Option C — bake in the native multiplexer as the default (Option B),
behind a `Backend` interface, with tmux retained as an optional backend
(Option A).**

Rationale: the entire reason to leave the zsh-plugin world is to get an embedded,
styled, "hop-between" dashboard and a symmetric remote story — both of which *only*
Option B delivers. But we don't have to bet the robustness of the product on a
young emulator on day one: the `Backend` seam lets a user (or us, during a bug)
fall back to `backend = "tmux"` for a specific harness. tmux moves from
*foundation* to *escape hatch*.

### Resize policy (native backend)

- A harness PTY has one authoritative size at a time.
- When exactly one client is attached, the PTY tracks that client's viewport.
- When multiple clients are attached at different sizes, **the smallest attached
  viewport wins** (tmux's default "constrain to smallest" behavior), so no client
  sees clipped output. A future per-harness "pin size" option can override.
- Detaching re-computes the authoritative size from remaining clients.

### What we deliberately defer

- **Multi-pane / tiling** (several harnesses visible at once). Option B makes it
  *possible*; spec-tui.md keeps v1 to single-attach ("hop", don't tile") to keep
  scope sane. Revisit once the single-attach path is solid.
- **Copy-mode parity** with tmux. v1 gives scrollback + search (ADR-0007); full
  copy-mode selection semantics can come later.

## Consequences

**Positive** — native embedded terminals, one binary, symmetric remote, full
control of the "hop" and scrollback UX, tmux available as a safety valve.

**Negative** — the emulator is the project's core technical risk; we own resize,
reflow, and escape-sequence fidelity. Mitigation: the `Backend` interface + tmux
fallback + a corpus of real agent-CLI sessions (Claude Code, Crush) as a golden
test set for the emulator.

### The ecosystem de-risks this more than it first appears

The native backend is *assembling first-party Charm packages*, not writing a
terminal stack from scratch (see [charm-ecosystem-map.md](#file-charm-ecosystem-map-md)):

- **PTY:** `x/xpty` (+ `x/conpty` for eventual Windows).
- **Emulation:** `x/vt` (screen+scrollback+`InputPipe`), `x/ansi` (sequences),
  `x/cellbuf` (diff-based repaints), and crucially **`x/wcwidth`** for wide/CJK/
  combining-char widths — *the specific correctness hazard* called out above.
- **Rendering:** `ultraviolet` (the `Drawable` the `vt` screen composites into) +
  Lip Gloss for the frame around it.
- **Input on attach:** `x/input` decodes keyboard/mouse events.
- **Testing the risk down:** `x/vttest` (VT conformance helpers) + **`x/vcr`**
  (record/replay of terminal interactions) let us capture real Claude Code / Crush
  sessions as fixtures and assert the emulator reproduces them — the golden-test
  corpus mentioned above, with tooling provided. `sequin` (human-readable ANSI
  decoder) is the debugging aid while hardening it.

So the "young emulator" risk is real but *instrumented*: we have conformance
tests, a record/replay fixture harness, width tables, and — as the ultimate
backstop — `backend = "tmux"`.

## Related

ADR-0001 (`x/vt`, `x/xpty` come from the chosen ecosystem), ADR-0002 (daemon owns
the emulator), ADR-0004 (remote renders the same screen), ADR-0007 (scrollback).

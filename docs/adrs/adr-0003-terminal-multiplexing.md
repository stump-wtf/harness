---
status: accepted
date: 2026-07-18
decision-makers: [joestump]
extends: [ADR-0002]
related: [ADR-0001, ADR-0004, ADR-0007]
---

# ADR-0003: Terminal multiplexing — bake it in, keep tmux optional

This is the load-bearing decision: everything about the UX ceiling and the
dependency footprint hangs off it.

## Context and Problem Statement

In the predecessor, tmux *is* the product's spine: each harness is a detached
tmux session on a shared socket, the restart loop runs inside the pane, and
`attach` is literally `tmux attach`. tmux feels good to operate, but baking the
multiplexer in would remove it as a dependency.

The question: does the new daemon **shell out to tmux** for per-harness terminals
and attach, or **own the PTY + terminal emulation itself** (`x/xpty` + `x/vt`)?

## Decision Drivers

* **Embedded, styled UI.** We want harness terminals rendered *inside* our TUI —
  a sidebar of harnesses, a framed terminal pane, a status bar — not a raw
  handoff to a fullscreen tmux client. That's hard to do well when tmux owns the
  screen.
* **Dependency footprint.** tmux is an external runtime dep with its own config,
  version quirks, and socket semantics. A single static binary is a goal.
* **Control over the "hop" UX.** Instant switch between harnesses, shared attach,
  in-TUI scrollback search, per-harness activity indicators — all easier when we
  own the emulator.
* **Robustness we'd have to re-earn.** tmux has 15+ years of PTY/resize/reflow/
  copy-mode hardening. Rolling our own means re-earning some of that.
* **Remote attach.** Must compose with ADR-0004 (Wish/SSH). A Bubble Tea program
  served over SSH can't cleanly "become" a tmux client.

## Considered Options

* Option 1 — Shell out to tmux (evolve the predecessor's model)
* Option 2 — Bake in the multiplexer (`x/xpty` + `x/vt`)
* Option 3 — Hybrid: native by default, tmux as an optional backend

## Decision Outcome

Chosen option: **Option 3 — Hybrid: native by default, tmux as an optional
backend**, because the entire reason to leave the zsh-plugin world is to get an
embedded, styled, "hop-between" dashboard and a symmetric remote story — both of
which *only* the native multiplexer (Option 2) delivers — while the `Backend`
seam means we don't bet the robustness of the product on a young emulator on day
one.

Define a `Backend` interface — `Spawn`, `Resize`, `Subscribe(screen)`, `Input`,
`Signal`, `Kill`. Ship a **native backend (Option 2)** as the default, and a
**tmux backend (Option 1)** behind a per-harness/global `backend = "tmux"` config
knob for operators who want their tmux world, or as a fallback if `vt` bites us
on some exotic program. A user (or we, during a bug) can fall back to
`backend = "tmux"` for a specific harness. tmux moves from *foundation* to
*escape hatch*.

### Resize policy (native backend)

* A harness PTY has one authoritative size at a time.
* When exactly one client is attached, the PTY tracks that client's viewport.
* When multiple clients are attached at different sizes, **the smallest attached
  viewport wins** (tmux's default "constrain to smallest" behavior), so no client
  sees clipped output. A future per-harness "pin size" option can override.
* Detaching re-computes the authoritative size from remaining clients.

### What we deliberately defer

* **Multi-pane / tiling** (several harnesses visible at once). Option 2 makes it
  *possible*; the TUI specification (SPEC-0001) keeps v1 to single-attach ("hop",
  don't tile) to keep scope sane. Revisit once the single-attach path is solid.
* **Copy-mode parity** with tmux. v1 gives scrollback + search (ADR-0007); full
  copy-mode selection semantics can come later.

### The ecosystem de-risks this more than it first appears

The native backend is *assembling first-party Charm packages*, not writing a
terminal stack from scratch (see the
[Charm ecosystem map](../usage/charm-ecosystem-map.md)):

* **PTY:** `x/xpty` (+ `x/conpty` for eventual Windows).
* **Emulation:** `x/vt` (screen+scrollback+`InputPipe`), `x/ansi` (sequences),
  `x/cellbuf` (diff-based repaints), and crucially **`x/wcwidth`** for wide/CJK/
  combining-char widths — *the specific correctness hazard* called out below.
* **Rendering:** `ultraviolet` (the `Drawable` the `vt` screen composites into) +
  Lip Gloss for the frame around it.
* **Input on attach:** `x/input` decodes keyboard/mouse events.
* **Testing the risk down:** `x/vttest` (VT conformance helpers) + **`x/vcr`**
  (record/replay of terminal interactions) let us capture real Claude Code / Crush
  sessions as fixtures and assert the emulator reproduces them — the golden-test
  corpus below, with tooling provided. `sequin` (human-readable ANSI decoder) is
  the debugging aid while hardening it.

So the "young emulator" risk is real but *instrumented*: we have conformance
tests, a record/replay fixture harness, width tables, and — as the ultimate
backstop — `backend = "tmux"`.

### Consequences

* Good, because terminals are native and embedded, there is one binary, remote is
  symmetric with local, and we have full control of the "hop" and scrollback UX.
* Good, because tmux remains available as a safety valve.
* Bad, because the emulator is the project's core technical risk; we own resize,
  reflow, and escape-sequence fidelity. Mitigation: the `Backend` interface + tmux
  fallback + a corpus of real agent-CLI sessions (Claude Code, Crush) as a golden
  test set for the emulator.

### Confirmation

* A harness with no `backend` key runs under the native backend: its PTY is
  allocated by `x/xpty` and its screen is an `x/vt` emulator inside the daemon.
* The config accepts `backend = "native"` (the default) and `backend = "tmux"` on
  a harness; the key selects the backend without changing any client-facing verb.
* With two clients attached at different sizes, the PTY size equals the smaller
  viewport.

## Pros and Cons of the Options

### Option 1 — Shell out to tmux

The daemon runs `tmux new-session -d` per harness; `attach` execs `tmux attach`.

* Good, because it gets battle-tested PTY/scrollback/reflow/copy-mode for free.
* Good, because it is the least new code; the risky terminal-emulation problem is
  already solved.
* Good, because users who love tmux keep their muscle memory and config.
* Bad, because our TUI can't *embed* a tmux pane — attach means yielding the whole
  screen to a tmux client. The dashboard and the terminal can't coexist in one
  styled view.
* Bad, because remote attach over Wish is awkward: we'd be nesting a tmux client
  inside an SSH-served Bubble Tea app, or bypassing our TUI entirely.
* Bad, because it adds an external dependency, external config surface, version
  drift, and socket edge cases (the predecessor already carries `-L harness`
  socket handling).
* Bad, because there are two sources of truth (tmux's session state vs our
  registry) to reconcile.

### Option 2 — Bake in the multiplexer (`x/xpty` + `x/vt`)

The daemon allocates a PTY per harness via `x/xpty`, feeds the master's output
into an `x/vt` **virtual terminal emulator** (which maintains screen + scrollback
and is `Draw`able), and forwards client keystrokes into the emulator's
`InputPipe`. "Attach" = subscribe to that emulator's screen; the TUI renders it
as a Lip Gloss pane.

* Good, because **terminals live *inside* our UI.** Sidebar + framed terminal +
  status bar in one coherent, styled surface. The "hop between harnesses" UX
  becomes native and instant. Multi-pane (several agents on screen) becomes
  *possible* later.
* Good, because it is a **single static binary.** No tmux runtime, no external
  config.
* Good, because **remote is symmetric with local** — the SSH-served Bubble Tea
  client renders the same `vt` screen the local client does (ADR-0004). One code
  path.
* Good, because there is one source of truth (the daemon's emulator == the state).
* Good, because we control scrollback, search, activity detection, resize policy,
  and recording.
* Bad, because **we re-earn terminal-emulator robustness.** `x/vt` is young and
  experimental; edge cases (wide/combining chars, obscure escape sequences, mouse
  reporting, reflow-on-resize) may bite. This is the real cost.
* Bad, because it is more novel engineering than any other part of the system.
* Bad, because resize semantics are on us: when two clients attach at different
  sizes, whose dimensions win? (The resize policy above answers it.)

### Option 3 — Hybrid: native by default, tmux as an optional backend

* Good, because it delivers everything Option 2 delivers by default.
* Good, because a harness that trips an emulator bug can fall back to tmux with
  one config key.
* Bad, because two backends must be kept behind one interface.

## More Information

* **Extends ADR-0002** — the daemon owns the emulator.
* **Related ADR-0001** — `x/vt` and `x/xpty` come from the chosen ecosystem.
* **Related ADR-0004** — remote renders the same screen.
* **Related ADR-0007** — scrollback.

# Bubbletea TUI — Design System

A terminal-native design system for building **Bubble Tea / Charm-style TUI experiences** on the web — recreations, marketing, prototypes, and docs that look like the real thing. It captures the Charm ecosystem aesthetic (rounded Lip Gloss borders, monospace type, ANSI neon on blue-black) and pushes it toward a **cutesy-cyberpunk / Tron** direction: glowing phosphor, grid floors, a kawaii boba-cup mascot.

> This is an *original* system inspired by the open-source Charm ecosystem. It is **not** the official Charm brand and does not reproduce Charm's proprietary logo or commercial fonts.

---

## What this is modeled on

The [Charmbracelet](https://charm.sh) open-source TUI stack:

| Project | Repo | What it is |
|---|---|---|
| **Bubble Tea** | `github.com/charmbracelet/bubbletea` | The TUI framework (The Elm Architecture in Go) |
| **Bubbles** | `github.com/charmbracelet/bubbles` | Component library — list, textinput, spinner, progress, table, viewport, help, paginator |
| **Lip Gloss** | `github.com/charmbracelet/lipgloss` | Styling: borders, colors, padding, layout |
| **Huh?** | `github.com/charmbracelet/huh` | Interactive forms |
| **Gum** | `github.com/charmbracelet/gum` | Glamorous shell-script prompts |
| **Glow** | `github.com/charmbracelet/glow` | Markdown reader (Glamour rendering) |
| **Soft Serve / Wish** | `github.com/charmbracelet/soft-serve` | Self-hostable git server + SSH apps |

Sources explored: the Bubble Tea README/tutorial and the ecosystem repos above. The design system re-expresses their *visual language* (character-cell layouts, rounded borders, ANSI palette, help footers) as reusable React + CSS.

**The "products" recreated here are TUI applications** — a project scaffolder (`charm-cli`) and a markdown reader (`glow`) — because that is what this ecosystem produces.

---

## CONTENT FUNDAMENTALS

The voice is the Charm voice: **playful, warm, lowercase, a little cheeky, never corporate.**

- **Casing** — lowercase by default (`bubbletea`, `charm — create`, `brew install charm`). Reserve Title Case for proper nouns (Bubble Tea, Lip Gloss). Uppercase is used *only* as a stylistic device: pixel eyebrows (`// TERMINAL UI KIT`), status-bar modes (`NORMAL`, `INSERT`, `READING`), and tiny badges (`NEW`, `GO`).
- **Person** — address the user as **you** ("What would you like to brew?", "Your app is ready"). The program refers to itself implicitly, never "we the company."
- **Tone** — friendly and concrete. Micro-copy leans into the tea/boba metaphor: *brew, steep, boba, blend*. Errors stay gentle ("Alas, there's been an error").
- **Emoji** — used **sparingly and intentionally**, matching Charm's own READMEs (🫧 💄 🍬 🏗). Never as bullet decoration or filler. Prefer Unicode/box-drawing glyphs (`✓ ✗ ◆ • → ↑↓`) over emoji inside the UI itself.
- **Command flavor** — copy frequently mimics a shell: a pink/mint prompt glyph (`❯` / `$`) followed by a monospace command, often with a blinking cursor.
- **Help lines** — every screen ends with a dim footer of `key action` pairs joined by bullets: `↑/↓ navigate • enter select • q quit`. This is a load-bearing convention, not decoration.

Examples of on-brand copy: `"What would you like to brew?"`, `"Steeping the boba…"`, `"cd boba-app && go run ."`, `"Leverage the power of Bubbles without writing any Go."`

---

## TWO DESIGN LANGUAGES

The system speaks two dialects. Decide which one you're in **before** designing:

**1. TUI (terminal)** — rigid, cell-grid framework:
- **Always framed.** Every screen lives inside `TerminalWindow` — the window chrome *is* the terminal. Status bar bottom, help footer (`key action • key action`) always present.
- Character-cell discipline: mono type only, box-drawing borders, glyph icons, block cursors.
- **Cell-honesty rules (inside the terminal body):**
  - **One glyph size.** Terminals render exactly one cell size — headings are bold + color, never bigger type. (Emoji like 🧋 are legal — they're double-width cells — but images are not.)
  - **No pills, no rounded chips.** Buttons and badges are flat highlight bars (square corners) — colored cells, like Gum/Lip Gloss actually render. Bordered boxes (inputs, tables, dialogs) may use `--radius-sm` at most — that's the curvature of the `╭ ╮` glyphs, nothing more.
  - **No shadows or bloom inside.** A terminal can't paint drop shadows or glow — emphasis comes from color, bold, and reverse-video (tint) highlights only.
  - **Gradients only as cell ramps.** Smooth gradients exist only where Bubbles itself fakes them (per-cell colored progress fills); never as decorative surface gradients.
  - The *emulator chrome* (the `TerminalWindow` frame, its dots, its drop shadow and outer glow) is the OS layer — it may stay rich. Everything inside the frame obeys the cell.
- **Both themes.** Night is the native habitat; day mode = a light terminal theme (Solarized-Light spirit) — same rules, paper canvas.
- Layout tops out around a 900–1000px window — it's a terminal, not a canvas.

**2. HTML/HTMX (browser)** — richer, borderless, web-scale:
- **No window chrome.** Content sits directly on the page canvas. A framed window on a web surface means exactly one thing: *a literal embedded terminal* (which stays dark even in day mode). Never wrap web content in fake terminal chrome.
- **Day-first**, with a night toggle. Still mono-first typography — that's the brand — but with web-scale sizes (`--text-5xl` heroes), `--radius-2xl` cards, and richer hover/motion.
- **Stretch out.** Assume big screens: `--container-web` 1160px, `--section-pad` 96px sections, `--gap-web` gutters, generous whitespace. Don't crowd.
- Server-rendered spirit: plain HTML + CSS + tokens works without React; sprinkle interactivity, don't SPA it.

See `ui_kits/charm-web/` for the browser language and `ui_kits/charm-cli/` + `ui_kits/glow/` for the TUI language. The **Brand → Two Design Languages** card shows them side-by-side.

---

## DAY & NIGHT THEMES

Night is the default (`:root`). Day mode is a full token scope: set **`data-theme="day"` on `<html>`** and every component and surface flips — lavender-paper backgrounds (`#EEEDFA`–`#FDFDFF`), the same brand hues deepened for contrast (purple `#6C46EA`, pink `#E43A8C`, cyan `#0793B4`, mint `#059669`), dark-indigo text, and neon glows softened into colored shadows. Theme-aware tokens to reach for: `--tint-primary/accent/info` (row highlights), `--focus-ring`/`--focus-ring-soft`. Persist a user's choice in `localStorage` (`btds-theme`) as `charm-web` does. Embedded terminals stay night in both themes.

---

## VISUAL FOUNDATIONS

**Overall vibe.** A dark terminal floating on a blue-black void, lit from within by ANSI neon and a faint Tron grid (night); a lavender-paper page with the same neon voice, deepened, in day mode. Everything is monospace, everything can glow.

- **Color.** Deep blue-black backgrounds (`--bg-void #08080F` → `--bg-surface-2 #1E1E38`) carry neon foreground: Charm purple `#7D56F4` and hot pink `#FF5FA2` anchor the brand; cyan `#4EE6FF` and mint `#00F0A8` supply the Tron/cyberpunk edge; gold and coral cover warning/danger. Text is cool phosphor white (`#F4F4FF`) fading to dim indigo-grey. See the **Colors** foundation cards.
- **Type.** Monospace-first. `JetBrains Mono` is the workhorse (body, code, UI). `Space Mono` is the chunky display voice for wordmarks and titles (tracking `-0.04em`, often gradient-filled). `Silkscreen` is a pixel accent for eyebrows/badges only. Scale runs 11→62px; line-height stays cell-aware (1.35 body). No non-mono fonts — hierarchy comes from weight, size, and color.
- **Backgrounds.** Never flat. The signature treatment is a **Tron floor grid** (`--grid-tron`, 26–30px cells) plus a soft radial brand glow bleeding from the top. Full-bleed imagery is rare; the void + grid is the canvas.
- **Borders & corners.** The Lip Gloss `RoundedBorder` (`╭ ╮ ╰ ╯`) is *the* signature. DOM chrome mirrors it: 1.5px borders in `--line #3A3A66`, corners `--radius-md 8px` (panels) to `--radius-lg 12px` (terminal windows). Focused elements shift the border to cyan.
- **Cards / panels.** A card is a `--bg-surface` fill, 1px `--line-dim` or 1.5px `--line` border, `--radius-md`, and usually a subtle inner top-gradient (`linear-gradient(180deg, rgba(125,86,244,.1), transparent)`). No heavy drop shadows on inner cards — elevation is expressed with **glow**, not blur-shadow.
- **Shadows = glow, at the right layer.** Neon bloom (`--glow-purple/pink/cyan/mint`) belongs to *emulator chrome and web surfaces only* — never inside a terminal body (cells can't glow). Terminal *windows* get a real drop shadow (`--shadow-window`) so they float on the void, plus a faint colored bloom.
- **Cursor.** The blinking block cursor (`█`, cyan, `steps(1)` hard on/off, period `1.06s`) is the heartbeat of the system — present in inputs, prompts, and log streams.
- **Motion.** Quick and springy (Harmonica-inspired): `--ease-spring` for button lifts, `--ease-out` for bars, `120–340ms`. Spinners cycle glyph frames (braille/dots/moon). Everything respects `prefers-reduced-motion` (cursor/spinners freeze, progress jumps to end). **Never `transition: all`** — always list explicit properties (transform, color, border-color, box-shadow, background-color); `all` catches initial style application and can freeze layout properties.
- **Hover / press.** Hover: lift + intensify glow (buttons translateY(-1px)), or a surface tint (`rgba(125,86,244,.16)`) on rows. Focus/selected: cyan ring + glow, plus an inset colored rail (`inset 3px 0 0 var(--charm-pink)`) on the active list/table row — mirroring the TUI `>` cursor.
- **Transparency & blur.** Used lightly — tinted row highlights use `rgba`/`color-mix` fills; scrims use `--scrim` (72% void). Backdrop-blur is not part of the terminal metaphor and is avoided.
- **Imagery vibe.** Cool, saturated, neon-on-dark. The only illustration is the kawaii boba-cup mascot (`assets/logo-boba.svg`) — pink→purple→cyan gradient, cyan straw, dark tapioca pearls, a little smiley face.

---

## ICONOGRAPHY

**Terminal UIs don't use an SVG icon set — they use glyphs.** This system follows that faithfully.

- **Primary "icons" are Unicode + box-drawing characters**, rendered in the mono font: navigation `↑ ↓ ← →`, status `✓ ✗ ◆ ● ○ ▲ •`, prompts `❯ › $ #`, spinners `⣾⣽⣻⢿⡿⣟⣯⣷` and `⠋⠙⠹⠸`, progress blocks `█ ▓ ▒ ░`, and the box-drawing set for borders `─ │ ╭ ╮ ╰ ╯ ├ ┤ ┬ ┴ ┼`. The **Spacing → Box-Drawing & Borders** card is the reference sheet.
- **This is intentional and authentic** — it matches how Bubble Tea, Bubbles, and Lip Gloss render. Do **not** substitute a web icon font for these; type the glyph.
- **Emoji** appear only where Charm itself uses them: section flourishes in prose/READMEs (🫧 💄 🍬) and the `moon` spinner. Never inside dense UI.
- **Nerd Font note.** Real Charm terminals often use a Nerd Font for extra glyphs (git branches, file-type icons). We rely on standard Unicode so it renders everywhere; if a consumer has a Nerd Font available, those code points can be dropped in.
- **The one bitmap asset** is the boba mascot (`assets/logo-boba.svg`) + wordmark (`assets/wordmark.svg`) — original vector brand art, safe to place on any dark surface.

No third-party icon CDN is linked, by design.

---

## Font substitution ⚠️

Charm's own surfaces use **commercial** typefaces (e.g. Berkeley Mono on charm.sh). Those are not redistributable, so this system ships the closest **free Google Fonts** substitutes: **JetBrains Mono** (body/code), **Space Mono** (display), **Silkscreen** (pixel accent), loaded via `tokens/fonts.css`.

**If you have licensed the real faces, drop the files into `tokens/` and swap the `@import` for local `@font-face` rules** — the token aliases (`--font-mono`, `--font-display`) will pick them up with no other changes. *(Because fonts load from the Google CDN rather than bundled `@font-face` binaries, the compiler reports 0 self-hosted fonts — expected.)*

---

## INDEX / MANIFEST

**Root**
- `styles.css` — the single entry point consumers link (`@import` manifest only).
- `readme.md` — this guide.
- `SKILL.md` — Agent-Skills wrapper (works in Claude Code).

**`tokens/`** — global CSS custom properties (187 tokens; night `:root` + `[data-theme="day"]` scope)
- `fonts.css` · `colors.css` · `typography.css` · `spacing.css`

**`assets/`** — brand art
- `logo-boba.svg` (kawaii boba mascot) · `wordmark.svg` (bubbletea lockup)

**`components/`** — 15 React primitives (`window.BubbleteaTUIDesignSystem_5c2f37`)
- `terminal/` — **TerminalWindow**, **StatusBar**, **Kbd**
- `forms/` — **Button**, **TextInput**, **Checkbox**, **Toggle**
- `display/` — **List**, **Table**, **Tabs**, **Badge**
- `feedback/` — **Spinner**, **Progress**, **KeyHint**, **Dialog**

**`guidelines/`** — 19 foundation specimen cards (Colors incl. Day Mode, Type, Spacing, Brand incl. Two Design Languages)

**`ui_kits/`** — full product recreations, one per design language
- `charm-cli/` — TUI language: a project-scaffolder (menu → form → install flow)
- `glow/` — TUI language: a two-pane markdown reader
- `charm-web/` — HTML/HTMX language: borderless, day-mode, web-scale landing page with night toggle

Every `.html` tagged `@dsCard` appears in the **Design System** tab; the two UI-kit screens are also tagged `@startingPoint`.

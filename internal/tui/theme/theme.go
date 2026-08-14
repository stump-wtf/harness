// Package theme is the TUI's Lip Gloss palette and the paired glyph+color
// state-presentation system.
//
// Governing: SPEC-0001 REQ "State Presentation" (paired glyph + adaptive color
// for the SPEC-0003 states; colorprofile degradation; color NEVER carries
// meaning alone — the glyph always accompanies it, for colorblind and mono
// legibility) and REQ "Zero And Error States" / day-night themes (ADR-0002,
// ADR-0006). Palette tokens come from the design exploration in docs/design/:
// Charm purple #7D56F4, hot pink #FF5FA2, cyan #4EE6FF, mint #00F0A8, amber and
// coral for degraded/failed, on a blue-black void (night) or lavender-paper
// (day).
package theme

import (
	"image/color"
	"os"
	"sync"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/colorprofile"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// readOnlyGlyph is the eye badge shown on a read-only attach (SPEC-0001 REQ
// "Attached Mode"). It is not a lifecycle state, so it lives here rather than in
// core.State's glyph table.
const readOnlyGlyph = "👁"

// Adaptive is a light/dark color pair. Lip Gloss v2 removed AdaptiveColor (and
// the renderer that resolved it lazily), so the pair is now resolved eagerly by
// the Theme against its own background polarity — see Theme.resolve.
type Adaptive struct {
	// Light is the value used on a light terminal background (day theme).
	Light color.Color
	// Dark is the value used on a dark terminal background (night theme).
	Dark color.Color
}

// adaptive builds an Adaptive from the two hex tokens in docs/design/.
func adaptive(light, dark string) Adaptive {
	return Adaptive{Light: lipgloss.Color(light), Dark: lipgloss.Color(dark)}
}

// Palette holds the design-system tokens as light/dark pairs. The Theme picks
// per its background polarity and degrades the result to 256/16/mono through
// its color profile. Colors are only ever used *with* their paired glyph
// (SPEC-0001), so a mono terminal that drops all color still reads.
type Palette struct {
	// Accent is the Charm-purple brand color (headers, selection).
	Accent Adaptive
	// Pink / Cyan / Mint are the neon secondary hues from the exploration.
	Pink Adaptive
	Cyan Adaptive
	Mint Adaptive
	// Amber / Coral carry degraded / failed emphasis.
	Amber Adaptive
	Coral Adaptive
	// Fg / Dim / Faint are the text ramp; Border is the box-drawing color.
	Fg     Adaptive
	Dim    Adaptive
	Faint  Adaptive
	Border Adaptive
}

// DefaultPalette is the design-exploration palette: neon-on-void at night,
// the same hues deepened on lavender-paper by day (docs/design/).
func DefaultPalette() Palette {
	return Palette{
		Accent: adaptive("#5A3FD6", "#7D56F4"),
		Pink:   adaptive("#D6247A", "#FF5FA2"),
		Cyan:   adaptive("#0E8FB0", "#4EE6FF"),
		Mint:   adaptive("#009E70", "#00F0A8"),
		Amber:  adaptive("#B26A00", "#FFB454"),
		Coral:  adaptive("#C22E2E", "#FF5F5F"),
		Fg:     adaptive("#1A1A2E", "#E6E6F0"),
		Dim:    adaptive("#6C6C8A", "#9A9AB8"),
		Faint:  adaptive("#9A9AB0", "#5A5A78"),
		Border: adaptive("#B8A8F0", "#3A2F66"),
	}
}

// Colors is a Palette resolved against one Theme: the same tokens flattened to
// concrete colors, degraded to that theme's profile and picked for its
// background. Surfaces that build Lip Gloss styles directly — the `harness ls`
// table, doctor, cliui — hold one of these instead of the raw Palette, because
// Lip Gloss v2 styles take a resolved image/color.Color and no longer resolve a
// light/dark pair themselves. A mono profile yields nil entries, which Lip Gloss
// renders with no SGR sequence at all.
type Colors struct {
	Accent color.Color
	Pink   color.Color
	Cyan   color.Color
	Mint   color.Color
	Amber  color.Color
	Coral  color.Color
	Fg     color.Color
	Dim    color.Color
	Faint  color.Color
	Border color.Color
}

// Theme bundles a palette with the two terminal properties that used to live on
// the Lip Gloss v1 renderer: the color profile and the background polarity. A
// caller can pin either — for degradation testing, or because the terminal is
// on the far end of an SSH session — and every style is built through them, so
// a mono profile strips every SGR sequence and leaves glyph + text intact
// (SPEC-0001 REQ "State Presentation": legible in a monochrome terminal).
//
// Under Bubble Tea v2 the program downsamples output itself, so a TUI theme is
// built at colorprofile.TrueColor and lets Bubble Tea degrade; the profile
// carried here is what makes standalone (non-Bubble Tea) rendering — cliui, the
// `harness ls` table — degrade correctly.
type Theme struct {
	Palette Palette
	profile colorprofile.Profile
	isDark  bool
}

// New builds a Theme pinned to a color profile and background polarity.
func New(p colorprofile.Profile, isDark bool, pal Palette) *Theme {
	return &Theme{Palette: pal, profile: p, isDark: isDark}
}

// detected caches the terminal probe. Detecting the background is a blocking
// terminal round-trip, so it happens at most once per process — Lip Gloss v1
// hid this behind the lazily-initialized default renderer.
var detected = sync.OnceValues(func() (colorprofile.Profile, bool) {
	return colorprofile.Detect(os.Stdout, os.Environ()),
		lipgloss.HasDarkBackground(os.Stdin, os.Stdout)
})

// Default is the conventional theme: the detected terminal profile and
// background, plus the design palette. Detection falls back to a dark
// background when stdout is not a terminal.
func Default() *Theme {
	p, isDark := detected()
	return New(p, isDark, DefaultPalette())
}

// WithDarkBackground returns a copy of the theme rebuilt for the given
// background polarity. Bubble Tea v2 reports the real terminal background
// asynchronously via tea.BackgroundColorMsg — including the *client's*
// background over SSH — so the TUI rebuilds its theme when that arrives.
func (t *Theme) WithDarkBackground(isDark bool) *Theme {
	return New(t.profile, isDark, t.Palette)
}

// IsDark reports the background polarity this theme renders for. Components
// that build their own styles (bubbles' help, textinput, huh) need it, since
// Bubbles v2 takes isDark explicitly where v1 used AdaptiveColor.
func (t *Theme) IsDark() bool { return t.isDark }

// Colors resolves the whole palette for this theme in one pass.
func (t *Theme) Colors() Colors {
	return Colors{
		Accent: t.resolve(t.Palette.Accent),
		Pink:   t.resolve(t.Palette.Pink),
		Cyan:   t.resolve(t.Palette.Cyan),
		Mint:   t.resolve(t.Palette.Mint),
		Amber:  t.resolve(t.Palette.Amber),
		Coral:  t.resolve(t.Palette.Coral),
		Fg:     t.resolve(t.Palette.Fg),
		Dim:    t.resolve(t.Palette.Dim),
		Faint:  t.resolve(t.Palette.Faint),
		Border: t.resolve(t.Palette.Border),
	}
}

// resolve picks the light or dark half of a token and degrades it to the
// theme's color profile. Under a mono profile Convert yields nil, which Lip
// Gloss renders with no SGR sequence at all.
func (t *Theme) resolve(a Adaptive) color.Color {
	c := a.Dark
	if !t.isDark {
		c = a.Light
	}
	return t.profile.Convert(c)
}

// style is an empty style to build from. Lip Gloss v2 styles are plain values
// with no renderer attached; degradation happens in resolve instead.
func (t *Theme) style() lipgloss.Style { return lipgloss.NewStyle() }

// stateColor maps a lifecycle state to its palette token per SPEC-0001 REQ
// "State Presentation": running green(mint), degraded amber, the transient trio
// cyan, stopped pink (warm/red-family so it draws the eye like the other
// active states — the ○ glyph still distinguishes it from failed's ✖),
// failed red(coral).
func (t *Theme) stateColor(s core.State) Adaptive {
	switch s {
	case core.StateRunning:
		return t.Palette.Mint
	case core.StateDegraded:
		return t.Palette.Amber
	case core.StateStarting, core.StateRestarting, core.StateStopping:
		return t.Palette.Cyan
	case core.StateStopped:
		return t.Palette.Pink
	case core.StateFailed:
		return t.Palette.Coral
	default:
		return t.Palette.Fg
	}
}

// StateStyle returns the colored style for a state's glyph/label.
func (t *Theme) StateStyle(s core.State) lipgloss.Style {
	return t.style().Foreground(t.resolve(t.stateColor(s)))
}

// Glyph returns the SPEC-0003 status glyph for a state (delegating to core so
// the TUI and CLI never diverge — the issue mandates reuse of core.State.Glyph).
// An unknown state falls back to a neutral bullet so a row is never blank.
func (t *Theme) Glyph(s core.State) string {
	if !s.Valid() {
		return "·"
	}
	return s.Glyph()
}

// RenderState renders "<glyph> <label>" in the state color. Because the glyph
// and the text label are always emitted together, a mono terminal (where the
// color is stripped) still fully conveys the state (SPEC-0001 REQ "State
// Presentation": "state remains fully legible from glyphs and text").
func (t *Theme) RenderState(s core.State) string {
	return t.StateStyle(s).Render(t.Glyph(s) + " " + string(s))
}

// RenderGlyph renders just the colored glyph (row-leading marker). Even alone
// the glyph shape distinguishes every state, so color is decorative not
// load-bearing (colorblind-safe).
func (t *Theme) RenderGlyph(s core.State) string {
	return t.StateStyle(s).Render(t.Glyph(s))
}

// ReadOnlyBadge renders the "👁 read-only" badge for a read-only attach
// (SPEC-0001 REQ "Attached Mode"). The eye glyph + words survive color loss.
func (t *Theme) ReadOnlyBadge() string {
	return t.style().Foreground(t.resolve(t.Palette.Cyan)).Render(readOnlyGlyph + " read-only")
}

// --- structural styles used across the cockpit -----------------------------

// Header is the top bar style (app · profile · daemon identity).
func (t *Theme) Header() lipgloss.Style {
	return t.style().Foreground(t.resolve(t.Palette.Accent)).Bold(true)
}

// Footer is the key-bar style.
func (t *Theme) Footer() lipgloss.Style {
	return t.style().Foreground(t.resolve(t.Palette.Dim))
}

// Selected is the dashboard selection style.
func (t *Theme) Selected() lipgloss.Style {
	return t.style().Foreground(t.resolve(t.Palette.Fg)).Background(t.resolve(t.Palette.Border)).Bold(true)
}

// Box is the box-drawing bordered container — the Lip Gloss signature
// (rounded `╭ ╮ ╰ ╯`) called out in the design.
func (t *Theme) Box() lipgloss.Style {
	return t.style().Border(lipgloss.RoundedBorder()).BorderForeground(t.resolve(t.Palette.Border))
}

// Ribbon is the attached-mode status ribbon.
func (t *Theme) Ribbon() lipgloss.Style {
	return t.style().Foreground(t.resolve(t.Palette.Fg)).Background(t.resolve(t.Palette.Accent)).Bold(true)
}

// LogoChip renders the small brand mark that leads the attached-mode status
// bar: "h◈" in the accent color on the void/paper background. Compact and
// color-paired with text so it still reads as "harness" in mono.
func (t *Theme) LogoChip() string {
	return t.style().Foreground(t.resolve(t.Palette.Accent)).Bold(true).Render("h◈")
}

// Banner is the non-fatal config-parse banner (last-good config, ADR-0006).
func (t *Theme) Banner() lipgloss.Style {
	return t.style().Foreground(t.resolve(t.Palette.Coral)).Bold(true)
}

// Faint is dimmed/secondary text (config summary keys, hints).
func (t *Theme) Faint() lipgloss.Style {
	return t.style().Foreground(t.resolve(t.Palette.Faint))
}

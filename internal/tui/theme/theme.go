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
	"github.com/charmbracelet/lipgloss"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// readOnlyGlyph is the eye badge shown on a read-only attach (SPEC-0001 REQ
// "Attached Mode"). It is not a lifecycle state, so it lives here rather than in
// core.State's glyph table.
const readOnlyGlyph = "👁"

// Palette holds the design-system tokens as Lip Gloss adaptive colors. Each
// AdaptiveColor carries a Light value (day theme) and Dark value (night theme);
// Lip Gloss picks per the terminal background, and colorprofile degrades the
// value to 256/16/mono automatically. Colors are only ever used *with* their
// paired glyph (SPEC-0001), so a mono terminal that drops all color still reads.
type Palette struct {
	// Accent is the Charm-purple brand color (headers, selection).
	Accent lipgloss.AdaptiveColor
	// Pink / Cyan / Mint are the neon secondary hues from the exploration.
	Pink lipgloss.AdaptiveColor
	Cyan lipgloss.AdaptiveColor
	Mint lipgloss.AdaptiveColor
	// Amber / Coral carry degraded / failed emphasis.
	Amber lipgloss.AdaptiveColor
	Coral lipgloss.AdaptiveColor
	// Fg / Dim / Faint are the text ramp; Border is the box-drawing color.
	Fg     lipgloss.AdaptiveColor
	Dim    lipgloss.AdaptiveColor
	Faint  lipgloss.AdaptiveColor
	Border lipgloss.AdaptiveColor
}

// DefaultPalette is the design-exploration palette: neon-on-void at night,
// the same hues deepened on lavender-paper by day (docs/design/).
func DefaultPalette() Palette {
	return Palette{
		Accent: lipgloss.AdaptiveColor{Light: "#5A3FD6", Dark: "#7D56F4"},
		Pink:   lipgloss.AdaptiveColor{Light: "#D6247A", Dark: "#FF5FA2"},
		Cyan:   lipgloss.AdaptiveColor{Light: "#0E8FB0", Dark: "#4EE6FF"},
		Mint:   lipgloss.AdaptiveColor{Light: "#009E70", Dark: "#00F0A8"},
		Amber:  lipgloss.AdaptiveColor{Light: "#B26A00", Dark: "#FFB454"},
		Coral:  lipgloss.AdaptiveColor{Light: "#C22E2E", Dark: "#FF5F5F"},
		Fg:     lipgloss.AdaptiveColor{Light: "#1A1A2E", Dark: "#E6E6F0"},
		Dim:    lipgloss.AdaptiveColor{Light: "#6C6C8A", Dark: "#9A9AB8"},
		Faint:  lipgloss.AdaptiveColor{Light: "#9A9AB0", Dark: "#5A5A78"},
		Border: lipgloss.AdaptiveColor{Light: "#B8A8F0", Dark: "#3A2F66"},
	}
}

// Theme bundles a palette with a Lip Gloss renderer. All styles are built from
// the renderer so a caller can inject one pinned to a specific color profile
// (24-bit / 256 / 16 / mono) for degradation testing — the mono renderer strips
// every SGR sequence, leaving glyph + text intact (SPEC-0001 REQ "State
// Presentation": legible in a monochrome terminal).
type Theme struct {
	Palette  Palette
	renderer *lipgloss.Renderer
}

// New builds a Theme from a renderer and palette. A nil renderer uses the Lip
// Gloss default (stdout, auto-detected profile and background).
func New(r *lipgloss.Renderer, p Palette) *Theme {
	if r == nil {
		r = lipgloss.DefaultRenderer()
	}
	return &Theme{Palette: p, renderer: r}
}

// Default is the conventional theme: default renderer + design palette.
func Default() *Theme { return New(nil, DefaultPalette()) }

// style is a renderer-bound empty style to build from.
func (t *Theme) style() lipgloss.Style { return t.renderer.NewStyle() }

// stateColor maps a lifecycle state to its palette color per SPEC-0001 REQ
// "State Presentation": running green(mint), degraded amber, the transient trio
// cyan, stopped pink (warm/red-family so it draws the eye like the other
// active states — the ○ glyph still distinguishes it from failed's ✖),
// failed red(coral).
func (t *Theme) stateColor(s core.State) lipgloss.AdaptiveColor {
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
	return t.style().Foreground(t.stateColor(s))
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
	return t.style().Foreground(t.Palette.Cyan).Render(readOnlyGlyph + " read-only")
}

// --- structural styles used across the cockpit -----------------------------

// Header is the top bar style (app · profile · daemon identity).
func (t *Theme) Header() lipgloss.Style {
	return t.style().Foreground(t.Palette.Accent).Bold(true)
}

// Footer is the key-bar style.
func (t *Theme) Footer() lipgloss.Style {
	return t.style().Foreground(t.Palette.Dim)
}

// Selected is the dashboard selection style.
func (t *Theme) Selected() lipgloss.Style {
	return t.style().Foreground(t.Palette.Fg).Background(t.Palette.Border).Bold(true)
}

// Box is the box-drawing bordered container — the Lip Gloss signature
// (rounded `╭ ╮ ╰ ╯`) called out in the design.
func (t *Theme) Box() lipgloss.Style {
	return t.style().Border(lipgloss.RoundedBorder()).BorderForeground(t.Palette.Border)
}

// Ribbon is the attached-mode status ribbon.
func (t *Theme) Ribbon() lipgloss.Style {
	return t.style().Foreground(t.Palette.Fg).Background(t.Palette.Accent).Bold(true)
}

// LogoChip renders the small brand mark that leads the attached-mode status
// bar: "h◈" in the accent color on the void/paper background. Compact and
// color-paired with text so it still reads as "harness" in mono.
func (t *Theme) LogoChip() string {
	return t.style().Foreground(t.Palette.Accent).Bold(true).Render("h◈")
}

// StatusBar is the 1-line bottom bar in attached mode: a subtle background
// span across the full terminal width. Kept distinct from Ribbon (the
// hop-flash style) so the bar reads as chrome, not a flashing signal.
func (t *Theme) StatusBar() lipgloss.Style {
	return t.style().
		Foreground(t.Palette.Fg).
		Background(t.Palette.Border)
}

// Banner is the non-fatal config-parse banner (last-good config, ADR-0006).
func (t *Theme) Banner() lipgloss.Style {
	return t.style().Foreground(t.Palette.Coral).Bold(true)
}

// Faint is dimmed/secondary text (config summary keys, hints).
func (t *Theme) Faint() lipgloss.Style {
	return t.style().Foreground(t.Palette.Faint)
}

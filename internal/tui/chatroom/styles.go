package chatroom

import (
	"os"

	"charm.land/lipgloss/v2"

	"gitea.stump.rocks/stump.wtf/harness/internal/tui/theme"
	"github.com/stump-wtf/agent-trace/tail"
)

// styles resolves the chatroom's lipgloss styles from the Harness theme.
//
// Governing: SPEC-0015 REQ "Harness Identity Display" (per-harness color),
// REQ "High-Contrast Mode", REQ "Color Not Sole Indicator" — harness identity
// always renders as an @username prefix too, so a mono terminal loses nothing.
type styles struct {
	highContrast bool
	colors       theme.Colors

	fg          lipgloss.Style
	faint       lipgloss.Style
	errBadge    lipgloss.Style
	okBadge     lipgloss.Style
	box         lipgloss.Style
	focusedBox  lipgloss.Style
	selectedBox lipgloss.Style
}

// harnessColor maps each harness to a theme palette token. SPEC-0015 names
// web-style hexes; these are the same hues expressed through the Harness
// palette so the view follows the active theme (dark/light, profile
// degradation) instead of pinning its own colors.
func harnessColor(c theme.Colors, h tail.Harness) lipgloss.Style {
	st := lipgloss.NewStyle().Bold(true)
	switch h {
	case tail.HarnessClaudeCode:
		return st.Foreground(c.Accent) // purple
	case tail.HarnessCodex:
		return st.Foreground(c.Cyan) // teal
	case tail.HarnessCrush:
		return st.Foreground(c.Amber) // orange
	case tail.HarnessOpenCode:
		return st.Foreground(c.Mint) // green
	case tail.HarnessPi:
		return st.Foreground(c.Pink) // pink
	}
	return st.Foreground(c.Fg)
}

func highContrastMode() bool {
	return os.Getenv("HARNESS_HIGH_CONTRAST") == "1"
}

func newStyles(th *theme.Theme) styles {
	c := th.Colors()
	border := lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(c.Border)
	return styles{
		highContrast: highContrastMode(),
		colors:       c,
		fg:           lipgloss.NewStyle().Foreground(c.Fg),
		faint:        lipgloss.NewStyle().Foreground(c.Faint),
		errBadge:     lipgloss.NewStyle().Bold(true).Foreground(c.Coral),
		okBadge:      lipgloss.NewStyle().Foreground(c.Mint),
		box:          border,
		focusedBox:   border.BorderForeground(c.Accent),
		selectedBox:  lipgloss.NewStyle().Background(c.Accent).Foreground(c.Fg),
	}
}

func (s styles) user(h tail.Harness) lipgloss.Style {
	return harnessColor(s.colors, h)
}

func (s styles) actionBadge(action string) lipgloss.Style {
	if s.highContrast {
		return lipgloss.NewStyle().Bold(true).Underline(true)
	}
	switch action {
	case "edit":
		return lipgloss.NewStyle().Bold(true).Foreground(s.colors.Accent)
	case "verify":
		return lipgloss.NewStyle().Bold(true).Foreground(s.colors.Cyan)
	case "exec":
		return lipgloss.NewStyle().Bold(true).Foreground(s.colors.Amber)
	}
	return lipgloss.NewStyle().Foreground(s.colors.Dim)
}

func (s styles) markBadge(markType string) lipgloss.Style {
	st := lipgloss.NewStyle().Bold(true)
	if s.highContrast {
		return st.Underline(true)
	}
	if markType == "user" {
		return st.Foreground(s.colors.Accent)
	}
	return st.Foreground(s.colors.Dim)
}

func (s styles) header(w int) lipgloss.Style {
	return lipgloss.NewStyle().Width(w).Bold(true).Foreground(s.colors.Fg)
}

func (s styles) statusBar(w int) lipgloss.Style {
	return lipgloss.NewStyle().Width(w).Foreground(s.colors.Faint)
}

func (s styles) selected(w int) lipgloss.Style {
	return s.selectedBox.Width(w)
}

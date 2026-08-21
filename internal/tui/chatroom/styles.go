// Styles for the chatroom view (ADR-0015, SPEC-0015).
//
// Harness themes map to the existing Palette tokens where possible, falling
// back to hex values from the spec. The styles layer resolves through the
// theme system so dark/light and mono profiles degrade correctly.

package chatroom

import (
	"image/color"

	"charm.land/lipgloss/v2"
	"gitea.stump.rocks/stump.wtf/harness/internal/tui/theme"
)

// Styles holds pre-computed lipgloss styles for the chatroom.
type Styles struct {
	// Chat panel
	ChatViewport lipgloss.Style

	// Activity feed panel
	ActivityViewport lipgloss.Style
	ActivityHeader   lipgloss.Style

	// Status bar
	StatusBar lipgloss.Style
	PauseInd  lipgloss.Style
	FilterInd lipgloss.Style
	FocusInd  lipgloss.Style

	// Per-harness username styles (keyed by harness string)
	Username map[string]lipgloss.Style
	// Per-harness message body styles (keyed by harness string)
	Message map[string]lipgloss.Style

	// Badges
	BadgeRead   lipgloss.Style
	BadgeSearch lipgloss.Style
	BadgeEdit   lipgloss.Style
	BadgeExec   lipgloss.Style
	BadgeVerify lipgloss.Style
	BadgeOther  lipgloss.Style
	BadgeUser   lipgloss.Style
	BadgeError  lipgloss.Style
	BadgeOK     lipgloss.Style

	// Timestamp and tool name
	Timestamp lipgloss.Style
	Tool      lipgloss.Style
	Target    lipgloss.Style

	// Divider between chat and activity panels
	Divider lipgloss.Style

	// Dimmed text for secondary info
	Dim lipgloss.Style
}

// NewStyles builds the chatroom style set from a Harness theme.
func NewStyles(t *theme.Theme) *Styles {
	colors := t.Colors()

	// Resolve hex colors through the theme, falling back to spec hex values.
	// The spec maps:
	//   claude-code → Purple (Accent / #BB86FC)
	//   codex       → Green (Mint / #03DAC6)
	//   crush       → Orange (Amber / #FFB74D)
	//   opencode    → Blue (Cyan / #64B5F6)
	//   pi          → Pink (Pink / #F06292)
	harnessColors := map[string]color.Color{
		"claude-code": colors.Accent,
		"codex":       colors.Mint,
		"crush":       colors.Amber,
		"opencode":    colors.Cyan,
		"pi":          colors.Pink,
	}

	usernameStyles := make(map[string]lipgloss.Style)
	messageStyles := make(map[string]lipgloss.Style)

	for h, c := range harnessColors {
		usernameStyles[h] = lipgloss.NewStyle().Foreground(c).Bold(true)
		messageStyles[h] = lipgloss.NewStyle().Foreground(c)
	}

	dim := lipgloss.NewStyle().Foreground(colors.Dim)
	fg := lipgloss.NewStyle().Foreground(colors.Fg)

	badge := func(c color.Color) lipgloss.Style {
		return lipgloss.NewStyle().Foreground(c).Bold(true)
	}

	s := &Styles{
		ChatViewport:     lipgloss.NewStyle(),
		ActivityViewport: lipgloss.NewStyle(),
		ActivityHeader:   dim.Bold(true),
		StatusBar:        lipgloss.NewStyle().Background(colors.Border).Foreground(colors.Fg),
		PauseInd:         lipgloss.NewStyle().Foreground(colors.Amber).Bold(true),
		FilterInd:        dim,
		FocusInd:         dim,
		Username:         usernameStyles,
		Message:          messageStyles,
		BadgeRead:        badge(colors.Cyan),
		BadgeSearch:      badge(colors.Amber),
		BadgeEdit:        badge(colors.Coral),
		BadgeExec:        badge(colors.Amber),
		BadgeVerify:      badge(colors.Mint),
		BadgeOther:       badge(colors.Dim),
		BadgeUser:        lipgloss.NewStyle().Foreground(colors.Accent).Bold(true),
		BadgeError:       lipgloss.NewStyle().Foreground(colors.Coral).Bold(true),
		BadgeOK:          lipgloss.NewStyle().Foreground(colors.Mint).Bold(true),
		Timestamp:        dim,
		Tool:             fg.Bold(true),
		Target:           dim.Italic(true),
		Divider:          lipgloss.NewStyle().Foreground(colors.Border),
		Dim:              dim,
	}

	return s
}

// BadgeStyle returns the style for a given action badge.
func (s *Styles) BadgeStyle(action string) lipgloss.Style {
	switch action {
	case "read":
		return s.BadgeRead
	case "search":
		return s.BadgeSearch
	case "edit":
		return s.BadgeEdit
	case "exec":
		return s.BadgeExec
	case "verify":
		return s.BadgeVerify
	default:
		return s.BadgeOther
	}
}

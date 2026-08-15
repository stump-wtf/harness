// Package theme — help styles.
//
// Governing: SPEC-0001 REQ "Zero And Error States" (shared voice + palette
// across cockpit and CLI); ADR-0002 (the CLI is a thin client whose visual
// language matches the TUI). These styles reproduce the fang grammar —
// program line, usage block, command/flag tables, error hints — using only
// palette tokens so help is visually kin to the dashboard.
package theme

import "charm.land/lipgloss/v2"

// HelpProgram renders the program name in Accent bold (the leading "harness").
func (t *Theme) HelpProgram() lipgloss.Style {
	return t.style().Foreground(t.resolve(t.Palette.Accent)).Bold(true)
}

// HelpDesc renders the one-line program description in Faint text.
func (t *Theme) HelpDesc() lipgloss.Style {
	return t.style().Foreground(t.resolve(t.Palette.Faint))
}

// HelpUsage renders the literal "usage:" label in Dim.
func (t *Theme) HelpUsage() lipgloss.Style {
	return t.style().Foreground(t.resolve(t.Palette.Dim))
}

// HelpVerb renders a command/verb name in Accent.
func (t *Theme) HelpVerb() lipgloss.Style {
	return t.style().Foreground(t.resolve(t.Palette.Accent))
}

// HelpFlag renders a flag name in Cyan.
func (t *Theme) HelpFlag() lipgloss.Style {
	return t.style().Foreground(t.resolve(t.Palette.Cyan))
}

// HelpArg renders a positional argument or placeholder in Dim.
func (t *Theme) HelpArg() lipgloss.Style {
	return t.style().Foreground(t.resolve(t.Palette.Dim))
}

// HelpDesc2 renders a command/flag description in Dim text (the table body).
func (t *Theme) HelpDesc2() lipgloss.Style {
	return t.style().Foreground(t.resolve(t.Palette.Dim))
}

// HelpDefault renders a default value or parenthetical note in Faint.
func (t *Theme) HelpDefault() lipgloss.Style {
	return t.style().Foreground(t.resolve(t.Palette.Faint))
}

// HelpSection renders a section header (e.g. "commands:", "flags:") in Dim
// bold, separating the tables from the usage block.
func (t *Theme) HelpSection() lipgloss.Style {
	return t.style().Foreground(t.resolve(t.Palette.Dim)).Bold(true)
}

// HelpError renders the problem statement in an error hint in Coral.
func (t *Theme) HelpError() lipgloss.Style {
	return t.style().Foreground(t.resolve(t.Palette.Coral))
}

// HelpHint renders the hint/suggestion line in Dim italic.
func (t *Theme) HelpHint() lipgloss.Style {
	return t.style().Foreground(t.resolve(t.Palette.Dim)).Italic(true)
}

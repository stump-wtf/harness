package theme

// Governing: SPEC-0001 ("calm ops cockpit — state legibility over decoration")
// and docs/design/.
//
// The state glyph/colour pairing was well covered. The chrome accessors —
// Header, Selected, Box, Ribbon, Banner, Faint, LogoChip — were not tested from
// inside this package at all. They are exercised from `tui` when a view
// renders, but per-package coverage does not see that, and more importantly no
// test pins the properties that make the cockpit legible:
//
//   - The selected row must be visually distinguishable from an unselected one,
//     or the cursor disappears and every keystroke targets an unknown harness.
//   - Styles must degrade to plain text on a mono profile rather than emitting
//     escapes a non-colour terminal would print literally.
//
// These are cheap invariants that catch a whole class of "the TUI looks broken
// on that terminal" reports.

import (
	"strings"
	"testing"
)

// TestDefaultThemeIsUsable pins the package-level Default() constructor, which
// is what a caller gets with no configuration.
func TestDefaultThemeIsUsable(t *testing.T) {
	th := Default()
	if th == nil {
		t.Fatal("Default() returned nil")
	}
	// Rendering through it must not panic and must preserve the text.
	if got := th.Faint().Render("hello"); !strings.Contains(got, "hello") {
		t.Errorf("Faint().Render(%q) lost the text: %q", "hello", got)
	}
}

// TestChromeStylesPreserveText pins that every chrome accessor renders its
// content rather than swallowing or truncating it. A style that drops the text
// produces an empty header or an invisible banner.
func TestChromeStylesPreserveText(t *testing.T) {
	th := trueColorTheme()
	const sample = "SAMPLE"

	styles := map[string]func() string{
		"Header":   func() string { return th.Header().Render(sample) },
		"Footer":   func() string { return th.Footer().Render(sample) },
		"Selected": func() string { return th.Selected().Render(sample) },
		"Box":      func() string { return th.Box().Render(sample) },
		"Ribbon":   func() string { return th.Ribbon().Render(sample) },
		"Banner":   func() string { return th.Banner().Render(sample) },
		"Faint":    func() string { return th.Faint().Render(sample) },
	}
	for name, render := range styles {
		t.Run(name, func(t *testing.T) {
			got := render()
			if !strings.Contains(got, sample) {
				t.Errorf("%s().Render(%q) did not contain the text: %q", name, sample, got)
			}
		})
	}
}

// TestSelectedIsDistinguishable pins the single most important visual
// invariant on the dashboard: the selected row must not render identically to
// an unstyled one. If it does, the operator cannot see where the cursor is —
// and every lifecycle key acts on a harness they cannot identify.
func TestSelectedIsDistinguishable(t *testing.T) {
	th := trueColorTheme()
	const row = "reduit-agent   running"

	selected := th.Selected().Render(row)
	plain := th.Faint().Render(row)

	if selected == plain {
		t.Error("Selected() renders identically to Faint(); the cursor would be invisible")
	}
	if selected == row {
		t.Error("Selected() applied no styling at all on a true-colour profile")
	}
}

// TestLogoChipRenders pins the header chip. It returns a string rather than a
// Style, so an empty return is a silently blank header corner.
func TestLogoChipRenders(t *testing.T) {
	th := trueColorTheme()
	if got := th.LogoChip(); got == "" {
		t.Error("LogoChip() is empty; the header would render a blank chip")
	}
}

// hasColorEscape reports whether s carries an SGR *colour* parameter — the
// 30–37/90–97 foreground, 40–47/100–107 background, and 38/48 extended forms.
// Attribute-only SGR (1 bold, 2 faint, 7 inverse, 22/27 resets) is deliberately
// not colour: those render correctly on a monochrome terminal and are how the
// cockpit stays legible without colour at all.
func hasColorEscape(s string) bool {
	for i := 0; i+1 < len(s); i++ {
		if s[i] != 0x1b || s[i+1] != '[' {
			continue
		}
		j := i + 2
		start := j
		for j < len(s) && s[j] != 'm' && s[j] != 'a' {
			j++
		}
		if j >= len(s) || s[j] != 'm' {
			continue
		}
		for _, param := range strings.Split(s[start:j], ";") {
			switch {
			case param == "38" || param == "48":
				return true
			case len(param) == 2 &&
				(param[0] == '3' || param[0] == '4') &&
				param[1] >= '0' && param[1] <= '7':
				return true
			case len(param) == 3 &&
				(strings.HasPrefix(param, "9") || strings.HasPrefix(param, "10")):
				return true
			}
		}
	}
	return false
}

// TestChromeDropsColorOnMono pins the accessibility floor: on a mono profile
// the chrome must carry no COLOUR escapes, because a terminal without colour
// support has nothing to render them as.
//
// It deliberately does not forbid escapes outright. lipgloss's Ascii profile
// downgrades colour while keeping attributes, so bold survives — and that is
// the point: bold is what distinguishes the header and the selected row once
// colour is gone. TestMonoLegibility pins the stricter no-escapes rule for the
// state glyphs, where the glyph itself already carries the meaning.
func TestChromeDropsColorOnMono(t *testing.T) {
	th := asciiTheme()
	const sample = "SAMPLE"

	styles := map[string]string{
		"Header":   th.Header().Render(sample),
		"Footer":   th.Footer().Render(sample),
		"Selected": th.Selected().Render(sample),
		"Ribbon":   th.Ribbon().Render(sample),
		"Banner":   th.Banner().Render(sample),
		"Faint":    th.Faint().Render(sample),
		"LogoChip": th.LogoChip(),
	}
	for name, got := range styles {
		t.Run(name, func(t *testing.T) {
			if hasColorEscape(got) {
				t.Errorf("%s emitted a colour escape on a mono profile: %q", name, got)
			}
			if !strings.Contains(got, sample) && name != "LogoChip" {
				t.Errorf("%s lost its text on mono: %q", name, got)
			}
		})
	}
}

// TestSelectedStaysDistinguishableOnMono is the payoff of allowing attributes
// through. With colour unavailable the selected row must still differ from an
// ordinary one by some non-colour means — otherwise a mono user loses the
// cursor and every lifecycle key acts on an unidentifiable harness.
func TestSelectedStaysDistinguishableOnMono(t *testing.T) {
	th := asciiTheme()
	const row = "reduit-agent"

	selected := th.Selected().Render(row)
	if !strings.Contains(selected, row) {
		t.Fatalf("Selected() dropped the row text on mono: %q", selected)
	}
	if selected == row {
		t.Error("Selected() applied nothing at all on a mono profile; the cursor would be invisible without colour")
	}
}

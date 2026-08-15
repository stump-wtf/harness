package theme

import (
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"
)

// TestHelpStylesTrueColor verifies the help style methods emit SGR color
// sequences under TrueColor, confirming the theme palette is applied.
func TestHelpStylesTrueColor(t *testing.T) {
	th := New(colorprofile.TrueColor, true, DefaultPalette())

	styles := map[string]func() string{
		"HelpProgram": func() string { return th.HelpProgram().Render("harness") },
		"HelpDesc":    func() string { return th.HelpDesc().Render("desc") },
		"HelpUsage":   func() string { return th.HelpUsage().Render("usage:") },
		"HelpVerb":    func() string { return th.HelpVerb().Render("list") },
		"HelpFlag":    func() string { return th.HelpFlag().Render("--json") },
		"HelpArg":     func() string { return th.HelpArg().Render("NAME") },
		"HelpDesc2":   func() string { return th.HelpDesc2().Render("description") },
		"HelpDefault": func() string { return th.HelpDefault().Render("(default)") },
		"HelpSection": func() string { return th.HelpSection().Render("commands:") },
		"HelpError":   func() string { return th.HelpError().Render("error") },
		"HelpHint":    func() string { return th.HelpHint().Render("hint") },
	}

	for name, fn := range styles {
		out := fn()
		if !strings.Contains(out, "\x1b") {
			t.Errorf("%s under TrueColor should contain ANSI sequences, got %q", name, out)
		}
	}
}

// TestHelpStylesAscii verifies the help style methods degrade gracefully under
// a monochrome profile — text attributes (bold/italic) may survive but color
// escape sequences do not, so content is fully legible.
func TestHelpStylesAscii(t *testing.T) {
	th := New(colorprofile.Ascii, true, DefaultPalette())

	mustContainText := map[string]string{
		"HelpProgram": "harness",
		"HelpDesc":    "desc",
		"HelpUsage":   "usage:",
		"HelpVerb":    "list",
		"HelpFlag":    "--json",
		"HelpArg":     "NAME",
		"HelpDesc2":   "description",
		"HelpDefault": "(default)",
		"HelpSection": "commands:",
		"HelpError":   "error",
		"HelpHint":    "hint",
	}

	for text, expected := range mustContainText {
		var out string
		switch text {
		case "HelpProgram":
			out = th.HelpProgram().Render(expected)
		case "HelpDesc":
			out = th.HelpDesc().Render(expected)
		case "HelpUsage":
			out = th.HelpUsage().Render(expected)
		case "HelpVerb":
			out = th.HelpVerb().Render(expected)
		case "HelpFlag":
			out = th.HelpFlag().Render(expected)
		case "HelpArg":
			out = th.HelpArg().Render(expected)
		case "HelpDesc2":
			out = th.HelpDesc2().Render(expected)
		case "HelpDefault":
			out = th.HelpDefault().Render(expected)
		case "HelpSection":
			out = th.HelpSection().Render(expected)
		case "HelpError":
			out = th.HelpError().Render(expected)
		case "HelpHint":
			out = th.HelpHint().Render(expected)
		}
		if !strings.Contains(out, expected) {
			t.Errorf("%s under Ascii lost text %q, got %q", text, expected, out)
		}
	}
}

// TestHelpStylesDayNight verifies the help styles render under both day and
// night backgrounds (SPEC-0001 day/night requirement).
func TestHelpStylesDayNight(t *testing.T) {
	for _, isDark := range []bool{true, false} {
		th := New(colorprofile.TrueColor, isDark, DefaultPalette())
		out := th.HelpProgram().Render("harness")
		if !strings.Contains(out, "harness") {
			t.Errorf("isDark=%v: HelpProgram lost text", isDark)
		}
		out = th.HelpFlag().Render("--json")
		if !strings.Contains(out, "--json") {
			t.Errorf("isDark=%v: HelpFlag lost text", isDark)
		}
	}
}

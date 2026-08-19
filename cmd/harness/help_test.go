package main

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/colorprofile"

	"gitea.stump.rocks/stump.wtf/harness/internal/tui/theme"
)

// ansiStrip removes all SGR escape sequences from a string so styled and
// plain output can be compared on content alone.
var ansiStrip = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func stripANSI(s string) string {
	return ansiStrip.ReplaceAllString(s, "")
}

// TestPlainUsageContent verifies that plainUsage() preserves the exact help
// text the CLI has always emitted — same verbs, flags, defaults, and hint
// strings (SPEC-0001: this is a styling-only change, not a content change).
func TestPlainUsageContent(t *testing.T) {
	out := plainUsage()

	mustContain := []string{
		"harness — systemctl for your agents",
		"usage:",
		"harness [--socket PATH] [--json] <command> [args]",
		"harness daemon <subcommand> [daemon-flags]",
		"commands:",
		"list",
		"describe NAME",
		"start NAME",
		"stop NAME",
		"restart NAME",
		"logs NAME [--lines N] [--follow]",
		"profiles",
		"use-profile NAME",
		"reload",
		"doctor",
		"attach NAME [--ro]",
		"project commands",
		"up",
		"down [PROJECT]",
		"ps",
		"daemon subcommands",
		"daemon start",
		"daemon stop",
		"daemon status",
		"flags:",
		"--socket PATH",
		"--json",
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("plainUsage() missing %q", s)
		}
	}

	if !strings.HasSuffix(out, "\n") {
		t.Error("plainUsage() should end with a newline")
	}
}

// TestPlainDaemonUsageContent verifies the daemon help text is unchanged.
func TestPlainDaemonUsageContent(t *testing.T) {
	out := plainDaemonUsage()

	mustContain := []string{
		"harness daemon — supervise long-running harnesses",
		"usage:",
		"harness daemon <subcommand> [flags]",
		"subcommands:",
		"start",
		"stop",
		"status",
		"start/run flags:",
		"--config PATH",
		"--socket PATH",
		"--scrollback N",
		"--log-level LEVEL",
		"--log-file PATH",
		"--detach",
		"--ssh",
		"--ssh-listen H:P",
		"--version",
		"examples:",
		"harness daemon start --detach",
		"harness daemon stop",
		"harness daemon status",
	}
	for _, s := range mustContain {
		if !strings.Contains(out, s) {
			t.Errorf("plainDaemonUsage() missing %q", s)
		}
	}

	if !strings.HasSuffix(out, "\n") {
		t.Error("plainDaemonUsage() should end with a newline")
	}
}

// TestStyledUsageHasColor verifies the styled output carries SGR sequences
// under a TrueColor profile (i.e., the theme palette is actually applied).
func TestStyledUsageHasColor(t *testing.T) {
	th := theme.New(colorprofile.TrueColor, true, theme.DefaultPalette())
	out := styledUsage(th)
	if !strings.Contains(out, "\x1b") {
		t.Fatal("styledUsage() under TrueColor should contain ANSI escape sequences")
	}
}

// TestStyledDaemonUsageHasColor mirrors the above for the daemon help.
func TestStyledDaemonUsageHasColor(t *testing.T) {
	th := theme.New(colorprofile.TrueColor, true, theme.DefaultPalette())
	out := styledDaemonUsage(th)
	if !strings.Contains(out, "\x1b") {
		t.Fatal("styledDaemonUsage() under TrueColor should contain ANSI escape sequences")
	}
}

// TestStyledUsageMonoLegible verifies that under a monochrome profile the
// styled output is still fully legible — stripping any remaining text
// attributes (bold/italic) yields the same content as the plain output.
// Bold and italic SGR codes may survive under Ascii since they are text
// attributes not colors, but the content must be identical after stripping.
func TestStyledUsageMonoLegible(t *testing.T) {
	th := theme.New(colorprofile.Ascii, true, theme.DefaultPalette())
	styled := stripANSI(styledUsage(th))
	plain := plainUsage()
	styledLines := strings.Split(strings.TrimRight(styled, "\n"), "\n")
	plainLines := strings.Split(strings.TrimRight(plain, "\n"), "\n")
	if len(styledLines) != len(plainLines) {
		t.Fatalf("mono styled has %d lines, plain has %d", len(styledLines), len(plainLines))
	}
	for i, sl := range styledLines {
		// TrimRight only: leading indentation is part of the layout, and
		// trimming it on both sides is what let the styled renderer drop the
		// two-space row indent unnoticed.
		if strings.TrimRight(sl, " ") != strings.TrimRight(plainLines[i], " ") {
			t.Errorf("line %d: mono %q != plain %q", i, sl, plainLines[i])
		}
	}
}

// TestStyledDaemonUsageMonoLegible mirrors the above for the daemon help.
func TestStyledDaemonUsageMonoLegible(t *testing.T) {
	th := theme.New(colorprofile.Ascii, true, theme.DefaultPalette())
	styled := stripANSI(styledDaemonUsage(th))
	plain := plainDaemonUsage()
	styledLines := strings.Split(strings.TrimRight(styled, "\n"), "\n")
	plainLines := strings.Split(strings.TrimRight(plain, "\n"), "\n")
	if len(styledLines) != len(plainLines) {
		t.Fatalf("mono styled has %d lines, plain has %d", len(styledLines), len(plainLines))
	}
	for i, sl := range styledLines {
		// TrimRight only: leading indentation is part of the layout, and
		// trimming it on both sides is what let the styled renderer drop the
		// two-space row indent unnoticed.
		if strings.TrimRight(sl, " ") != strings.TrimRight(plainLines[i], " ") {
			t.Errorf("line %d: mono %q != plain %q", i, sl, plainLines[i])
		}
	}
}

// TestStyledUsageContentMatchesPlain verifies that stripping ANSI from the
// TrueColor styled output yields the same text content as the plain output.
// The styling is presentation-only — no words may be added or removed.
func TestStyledUsageContentMatchesPlain(t *testing.T) {
	th := theme.New(colorprofile.TrueColor, true, theme.DefaultPalette())
	styled := stripANSI(styledUsage(th))
	plain := plainUsage()

	// The styled and plain versions should contain the same key content
	// lines. We compare line-by-line because the padding/alignment may
	// differ slightly between the styled (lipgloss-rendered) and plain
	// (fmt %-21s) paths.
	styledLines := strings.Split(strings.TrimRight(styled, "\n"), "\n")
	plainLines := strings.Split(strings.TrimRight(plain, "\n"), "\n")

	if len(styledLines) != len(plainLines) {
		t.Fatalf("styled has %d lines, plain has %d (should match after ANSI strip)", len(styledLines), len(plainLines))
	}

	for i, sl := range styledLines {
		pl := plainLines[i]
		// TrimRight only — the left indent must match too (see above).
		if strings.TrimRight(sl, " ") != strings.TrimRight(pl, " ") {
			t.Errorf("line %d: styled %q != plain %q", i, sl, pl)
		}
	}
}

// TestStyledDaemonUsageContentMatchesPlain mirrors the above for daemon help.
func TestStyledDaemonUsageContentMatchesPlain(t *testing.T) {
	th := theme.New(colorprofile.TrueColor, true, theme.DefaultPalette())
	styled := stripANSI(styledDaemonUsage(th))
	plain := plainDaemonUsage()

	styledLines := strings.Split(strings.TrimRight(styled, "\n"), "\n")
	plainLines := strings.Split(strings.TrimRight(plain, "\n"), "\n")

	if len(styledLines) != len(plainLines) {
		t.Fatalf("styled has %d lines, plain has %d (should match after ANSI strip)", len(styledLines), len(plainLines))
	}

	for i, sl := range styledLines {
		pl := plainLines[i]
		if strings.TrimRight(sl, " ") != strings.TrimRight(pl, " ") {
			t.Errorf("line %d: styled %q != plain %q", i, sl, pl)
		}
	}
}

// TestStyledUsageDayNight verifies the help renders legibly under both day
// and night backgrounds — the SPEC-0001 day/night requirement extends to
// the CLI help surface.
func TestStyledUsageDayNight(t *testing.T) {
	for _, isDark := range []bool{true, false} {
		th := theme.New(colorprofile.TrueColor, isDark, theme.DefaultPalette())
		out := styledUsage(th)
		if !strings.Contains(out, "harness") {
			t.Errorf("isDark=%v: styledUsage() missing program name", isDark)
		}
	}
}

// TestHelpRowsAreIndented pins the two-space row indent on BOTH renderers. The
// styled renderer used to omit it entirely — every command and flag rendered
// flush-left in a terminal while the piped output stayed indented — and the
// comparison tests above could not see it because they trimmed both sides.
func TestHelpRowsAreIndented(t *testing.T) {
	th := theme.New(colorprofile.TrueColor, true, theme.DefaultPalette())
	for name, out := range map[string]string{
		"plain":  plainUsage(),
		"styled": stripANSI(styledUsage(th)),
	} {
		seen := 0
		for _, line := range strings.Split(out, "\n") {
			trimmed := strings.TrimLeft(line, " ")
			if !strings.HasPrefix(trimmed, "list ") && !strings.HasPrefix(trimmed, "--json ") {
				continue
			}
			seen++
			if !strings.HasPrefix(line, "  ") {
				t.Errorf("%s: table row %q is not indented", name, line)
			}
		}
		if seen != 2 {
			t.Errorf("%s: matched %d sample rows, want 2 — the test is not looking at what it thinks", name, seen)
		}
	}
}

// TestHelpWrapsAtHelpWidth pins that no help line overflows an 80-column
// terminal. The hand-written help wrapped its long descriptions by hand; the
// generated layout has to do it itself, or `harness --help` in a default
// terminal folds mid-word into column 0.
func TestHelpWrapsAtHelpWidth(t *testing.T) {
	for name, out := range map[string]string{
		"root":   plainUsage(),
		"daemon": plainDaemonUsage(),
	} {
		for _, line := range strings.Split(out, "\n") {
			if len([]rune(line)) > helpWidth {
				t.Errorf("%s help line is %d columns, want <= %d: %q", name, len([]rune(line)), helpWidth, line)
			}
		}
	}
}

// TestHasJSONArg pins the pre-parse --json scan that keeps `harness --json
// --help` plain. flag.Parse invokes Usage before the parsed value exists, so
// without this the --json branch in usage() was unreachable.
func TestHasJSONArg(t *testing.T) {
	for _, tt := range []struct {
		args []string
		want bool
	}{
		{nil, false},
		{[]string{"list"}, false},
		{[]string{"--json", "--help"}, true},
		{[]string{"-json"}, true},
		{[]string{"--json=true"}, true},
		{[]string{"--", "--json"}, false},
		{[]string{"--socket", "/tmp/s.sock", "--json"}, true},
	} {
		if got := hasJSONArg(tt.args); got != tt.want {
			t.Errorf("hasJSONArg(%v) = %v, want %v", tt.args, got, tt.want)
		}
	}
}

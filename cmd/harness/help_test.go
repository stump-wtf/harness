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
		"daemon-info",
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
	styledLines := strings.Split(strings.TrimSpace(styled), "\n")
	plainLines := strings.Split(strings.TrimSpace(plain), "\n")
	if len(styledLines) != len(plainLines) {
		t.Fatalf("mono styled has %d lines, plain has %d", len(styledLines), len(plainLines))
	}
	for i, sl := range styledLines {
		if strings.TrimSpace(sl) != strings.TrimSpace(plainLines[i]) {
			t.Errorf("line %d: mono %q != plain %q", i, strings.TrimSpace(sl), strings.TrimSpace(plainLines[i]))
		}
	}
}

// TestStyledDaemonUsageMonoLegible mirrors the above for the daemon help.
func TestStyledDaemonUsageMonoLegible(t *testing.T) {
	th := theme.New(colorprofile.Ascii, true, theme.DefaultPalette())
	styled := stripANSI(styledDaemonUsage(th))
	plain := plainDaemonUsage()
	styledLines := strings.Split(strings.TrimSpace(styled), "\n")
	plainLines := strings.Split(strings.TrimSpace(plain), "\n")
	if len(styledLines) != len(plainLines) {
		t.Fatalf("mono styled has %d lines, plain has %d", len(styledLines), len(plainLines))
	}
	for i, sl := range styledLines {
		if strings.TrimSpace(sl) != strings.TrimSpace(plainLines[i]) {
			t.Errorf("line %d: mono %q != plain %q", i, strings.TrimSpace(sl), strings.TrimSpace(plainLines[i]))
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
	styledLines := strings.Split(strings.TrimSpace(styled), "\n")
	plainLines := strings.Split(strings.TrimSpace(plain), "\n")

	if len(styledLines) != len(plainLines) {
		t.Fatalf("styled has %d lines, plain has %d (should match after ANSI strip)", len(styledLines), len(plainLines))
	}

	for i, sl := range styledLines {
		pl := plainLines[i]
		// Trim trailing whitespace for comparison — lipgloss may not
		// pad the same way fmt %-21s does.
		if strings.TrimSpace(sl) != strings.TrimSpace(pl) {
			t.Errorf("line %d: styled %q != plain %q", i, strings.TrimSpace(sl), strings.TrimSpace(pl))
		}
	}
}

// TestStyledDaemonUsageContentMatchesPlain mirrors the above for daemon help.
func TestStyledDaemonUsageContentMatchesPlain(t *testing.T) {
	th := theme.New(colorprofile.TrueColor, true, theme.DefaultPalette())
	styled := stripANSI(styledDaemonUsage(th))
	plain := plainDaemonUsage()

	styledLines := strings.Split(strings.TrimSpace(styled), "\n")
	plainLines := strings.Split(strings.TrimSpace(plain), "\n")

	if len(styledLines) != len(plainLines) {
		t.Fatalf("styled has %d lines, plain has %d (should match after ANSI strip)", len(styledLines), len(plainLines))
	}

	for i, sl := range styledLines {
		pl := plainLines[i]
		if strings.TrimSpace(sl) != strings.TrimSpace(pl) {
			t.Errorf("line %d: styled %q != plain %q", i, strings.TrimSpace(sl), strings.TrimSpace(pl))
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

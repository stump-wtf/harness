package theme

import (
	"io"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// asciiTheme is a theme whose renderer degrades to a monochrome (Ascii)
// profile — the SPEC-0001 "monochrome terminal or degraded SSH client" case.
func asciiTheme() *Theme {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.Ascii)
	return New(r, DefaultPalette())
}

// trueColorTheme renders at full 24-bit — SGR sequences present.
func trueColorTheme() *Theme {
	r := lipgloss.NewRenderer(io.Discard)
	r.SetColorProfile(termenv.TrueColor)
	return New(r, DefaultPalette())
}

// TestStateGlyphPairedWithColor verifies SPEC-0001 REQ "State Presentation":
// every lifecycle state renders its glyph paired with its label, and the glyph
// comes from core.State.Glyph() so the TUI never diverges from the CLI/spec.
func TestStateGlyphPairedWithColor(t *testing.T) {
	th := trueColorTheme()
	for _, s := range core.States {
		out := th.RenderState(s)
		glyph := s.Glyph()
		if !strings.Contains(out, glyph) {
			t.Errorf("state %q: rendered %q missing glyph %q", s, out, glyph)
		}
		if !strings.Contains(out, string(s)) {
			t.Errorf("state %q: rendered %q missing label", s, out)
		}
	}
}

// TestMonoLegibility verifies the colorblind/mono guarantee: in a monochrome
// profile every state is still legible from glyph + text alone, with no leftover
// escape sequences to garble a dumb terminal (SPEC-0001: "state remains fully
// legible from glyphs and text").
func TestMonoLegibility(t *testing.T) {
	th := asciiTheme()
	for _, s := range core.States {
		out := th.RenderState(s)
		if strings.Contains(out, "\x1b") {
			t.Errorf("state %q: mono render %q still contains an escape sequence", s, out)
		}
		if !strings.Contains(out, s.Glyph()) || !strings.Contains(out, string(s)) {
			t.Errorf("state %q: mono render %q lost glyph or label", s, out)
		}
	}
}

// TestDistinctGlyphs verifies color never carries meaning alone: the glyph
// shapes distinguish the visually-critical states (running/degraded/failed/
// stopped) from each other even with all color stripped.
func TestDistinctGlyphs(t *testing.T) {
	distinct := []core.State{core.StateRunning, core.StateDegraded, core.StateFailed, core.StateStopped}
	seen := map[string]core.State{}
	for _, s := range distinct {
		g := s.Glyph()
		if prev, ok := seen[g]; ok {
			t.Errorf("states %q and %q share glyph %q — not colorblind-safe", prev, s, g)
		}
		seen[g] = s
	}
}

// TestColorProfileDegradation verifies the same style yields color at 24-bit and
// no color under Ascii — i.e. colorprofile degradation is wired through the
// renderer (SPEC-0001).
func TestColorProfileDegradation(t *testing.T) {
	color := trueColorTheme().RenderGlyph(core.StateRunning)
	mono := asciiTheme().RenderGlyph(core.StateRunning)
	if !strings.Contains(color, "\x1b") {
		t.Fatalf("24-bit render %q carried no color", color)
	}
	if strings.Contains(mono, "\x1b") {
		t.Fatalf("mono render %q should carry no color", mono)
	}
	if !strings.Contains(color, "●") || !strings.Contains(mono, "●") {
		t.Fatalf("glyph lost across profiles: color=%q mono=%q", color, mono)
	}
}

// TestAllProfilesLegible verifies issue 16's acceptance: both themes stay
// legible across 24-bit, 256, 16-color, AND mono terminals — the glyph + label
// survive every colorprofile degradation, so meaning never rides on color alone.
func TestAllProfilesLegible(t *testing.T) {
	profiles := map[string]termenv.Profile{
		"truecolor": termenv.TrueColor,
		"ansi256":   termenv.ANSI256,
		"ansi16":    termenv.ANSI,
		"mono":      termenv.Ascii,
	}
	for pname, p := range profiles {
		r := lipgloss.NewRenderer(io.Discard)
		r.SetColorProfile(p)
		th := New(r, DefaultPalette())
		for _, s := range core.States {
			out := th.RenderState(s)
			if !strings.Contains(out, s.Glyph()) || !strings.Contains(out, string(s)) {
				t.Errorf("profile %s, state %q: rendered %q lost glyph or label", pname, s, out)
			}
		}
	}
}

// TestReadOnlyBadge verifies the read-only attach badge carries the eye glyph
// and the words (SPEC-0001 REQ "Attached Mode": a `👁 read-only` badge).
func TestReadOnlyBadge(t *testing.T) {
	out := asciiTheme().ReadOnlyBadge()
	if !strings.Contains(out, "👁") || !strings.Contains(out, "read-only") {
		t.Fatalf("read-only badge %q missing glyph or label", out)
	}
}

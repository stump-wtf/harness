package main

// Governing: SPEC-0001 REQ "State Presentation" (paired glyph + adaptive
// color across every CLI surface — color is decorative, the glyph carries
// meaning, so a mono terminal still reads); ADR-0001 (Charmbracelet stack:
// lipgloss + theme own the visual language). This file is the shared table
// renderer used by `list`, `describe`, `profiles`, `daemon-info`, and
// `doctor` so every CLI table looks like one product: bold header, rounded
// separator rules, colored cells when on a TTY, plain text otherwise.

import (
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/charmbracelet/lipgloss"

	"gitea.stump.rocks/stump.wtf/harness/internal/cliui"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/tui/theme"
)

// tableWidth is the column budget every CLI table shares. It matches the
// styled error/warn/info/success box width so a `list` next to a `doctor`
// report reads as a consistent surface.
const tableWidth = 80

// useColor reports whether the current stderr is a TTY and we're not in
// --json mode. Every table helper consults this so the decision is made
// once per process.
func useColor() bool { return !cliui.JSON() && cliui.IsTTY(os.Stderr) }

// palette is the shared design palette, looked up once.
func palette() theme.Palette { return theme.Default().Palette }

// Table is a tabular renderer with bold headers, separator rules, and
// optional ANSI styling. The zero value is not usable; use NewTable.
type Table struct {
	w       io.Writer
	tw      *tabwriter.Writer
	colored bool
	pal     theme.Palette
}

// NewTable starts a table written to w. Writes the header row + separator
// immediately. Call Row any number of times, then Flush.
func NewTable(w io.Writer, headers ...string) *Table {
	colored := useColor()
	pal := palette()
	t := &Table{
		w:       w,
		tw:      tabwriter.NewWriter(w, 0, 2, 2, ' ', 0),
		colored: colored,
		pal:     pal,
	}
	styled := make([]string, len(headers))
	for i, h := range headers {
		styled[i] = t.bold(h)
	}
	fmt.Fprintf(t.tw, "  %s\n", strings.Join(styled, "\t"))
	t.Separator()
	return t
}

// Separator writes a horizontal rule across the table width.
func (t *Table) Separator() {
	fmt.Fprintln(t.tw, strings.Repeat("─", tableWidth))
}

// Row writes one data row. Each cell is written verbatim; callers pass
// already-styled strings via the cell helpers below when color is wanted.
func (t *Table) Row(cells ...string) {
	fmt.Fprintf(t.tw, "  %s\n", strings.Join(cells, "\t"))
}

// Flush finalizes the table. Required before the writer is considered done.
func (t *Table) Flush() error { return t.tw.Flush() }

// bold renders s bold in the foreground color when coloring is on.
func (t *Table) bold(s string) string {
	if !t.colored {
		return s
	}
	return lipgloss.NewStyle().Foreground(t.pal.Fg).Bold(true).Render(s)
}

// --- cell helpers (not methods; usable inline in Row calls) ----------------

// stateCell renders "● running" in the state's palette color (paired glyph +
// label per SPEC-0001). The glyph always accompanies the color so a mono
// terminal that drops the color still fully conveys the state.
func stateCell(state string) string {
	s := core.State(state)
	glyph := stateGlyphFor(s)
	label := string(s)
	if !useColor() {
		return fmt.Sprintf("%s %s", glyph, label)
	}
	pal := palette()
	return lipgloss.NewStyle().Foreground(stateColor(s, pal)).Bold(true).
		Render(fmt.Sprintf("%s %s", glyph, label))
}

// stateGlyphOnly renders just the colored glyph for leading-column use.
func stateGlyphOnly(state string) string {
	s := core.State(state)
	glyph := stateGlyphFor(s)
	if !useColor() {
		return glyph
	}
	pal := palette()
	return lipgloss.NewStyle().Foreground(stateColor(s, pal)).Render(glyph)
}

// stateGlyphFor returns the SPEC-0003 glyph, falling back to "·" for an
// unknown state so a row is never blank.
func stateGlyphFor(s core.State) string {
	if !s.Valid() {
		return "·"
	}
	return s.Glyph()
}

// enabledCell renders "yes" in mint when enabled, "no" in dim when not.
func enabledCell(on bool) string {
	if on {
		return mintBold("yes")
	}
	return dimPlain("no")
}

// flappingCell renders the flapping flag with a warning glyph when true.
func flappingCell(on bool) string {
	if on {
		return amberBold("⚠ flapping")
	}
	return dimPlain("no")
}

// pidCell renders "-" in dim when pid <= 0, else the number in faint.
func pidCell(p int) string {
	if p <= 0 {
		return dimPlain("-")
	}
	return faintPlain(fmt.Sprintf("%d", p))
}

// yesno returns "yes" or "no" — the plain-text form used by callers that
// don't want styling (e.g. inside other styled cells).
func yesno(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// --- low-level styled primitives -------------------------------------------

func mintBold(s string) string {
	if !useColor() {
		return s
	}
	return lipgloss.NewStyle().Foreground(palette().Mint).Bold(true).Render(s)
}

func dimPlain(s string) string {
	if !useColor() {
		return s
	}
	return lipgloss.NewStyle().Foreground(palette().Dim).Render(s)
}

func faintPlain(s string) string {
	if !useColor() {
		return s
	}
	return lipgloss.NewStyle().Foreground(palette().Faint).Render(s)
}

func amberBold(s string) string {
	if !useColor() {
		return s
	}
	return lipgloss.NewStyle().Foreground(palette().Amber).Bold(true).Render(s)
}

func accentBold(s string) string {
	if !useColor() {
		return s
	}
	return lipgloss.NewStyle().Foreground(palette().Accent).Bold(true).Render(s)
}

func dimItalic(s string) string {
	if !useColor() {
		return s
	}
	return lipgloss.NewStyle().Foreground(palette().Dim).Italic(true).Render(s)
}

// stateColor maps a core.State to its palette color (running→mint,
// degraded→amber, transient→cyan, stopped→dim, failed→coral). Mirrors
// theme.stateColor so the CLI and TUI never diverge.
func stateColor(s core.State, pal theme.Palette) lipgloss.AdaptiveColor {
	switch s {
	case core.StateRunning:
		return pal.Mint
	case core.StateDegraded:
		return pal.Amber
	case core.StateStarting, core.StateRestarting, core.StateStopping:
		return pal.Cyan
	case core.StateStopped:
		return pal.Dim
	case core.StateFailed:
		return pal.Coral
	default:
		return pal.Fg
	}
}

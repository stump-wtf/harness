package main

// Governing: SPEC-0001 REQ "State Presentation" (paired glyph + adaptive
// color across every CLI surface — color is decorative, the glyph carries
// meaning, so a mono terminal still reads); ADR-0001 (Charmbracelet stack:
// lipgloss + theme own the visual language). This file is the shared table
// renderer used by `list`, `describe`, `profiles`, `daemon-info`, and
// `doctor` so every CLI table looks like one product: bold header, rounded
// separator rules, colored cells when on a TTY, plain text otherwise.
//
// The table uses lipgloss.Width-based cells rather than text/tabwriter:
// tabwriter counts bytes, so ANSI-styled cells (colored state glyphs, mint
// "yes", etc.) misalign. lipgloss applies padding to the *visible* width
// after rendering color, so columns line up regardless of styling.

import (
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"

	"gitea.stump.rocks/stump.wtf/harness/internal/cliui"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/tui/theme"
)

// tableWidth is the column budget every CLI table shares. It matches the
// styled error/warn/info/success box width so a `list` next to a `doctor`
// report reads as a consistent surface.
const tableWidth = 80

// useColor reports whether stderr is a TTY and we're not in --json mode.
func useColor() bool { return !cliui.JSON() && cliui.IsTTY(os.Stderr) }

// palette is the shared design palette, looked up once.
func palette() theme.Palette { return theme.Default().Palette }

// Table is a tabular renderer with bold headers, separator rules, and
// ANSI-aware column alignment. The zero value is not usable; use NewTable.
type Table struct {
	w       io.Writer
	colored bool
	pal     theme.Palette
	widths  []int // visible width per column
	rows    [][]string
}

// NewTable starts a table written to w. headers also fixes the column count
// and the visible width budget for each column. Call Row any number of times,
// then Flush.
func NewTable(w io.Writer, headers ...string) *Table {
	colored := useColor()
	pal := palette()
	t := &Table{
		w:       w,
		colored: colored,
		pal:     pal,
		widths:  defaultColumnWidths(headers),
	}
	// Bold + pad each header cell to its column width.
	styled := make([]string, len(headers))
	for i, h := range headers {
		styled[i] = t.padCell(t.bold(h), t.widths[i])
	}
	t.rows = append(t.rows, styled)
	return t
}

// defaultColumnWidths picks per-column widths that fit tableWidth. Short
// known columns (NAME/STATE/ENABLED/RESTARTS/PID/FIELD/CHECK/STATUS/
// AUTOSTART) get fixed budgets; everything else (typically DESCRIPTION,
// DETAIL, VALUE) absorbs the remainder so long text has room instead of
// wrapping every few characters.
func defaultColumnWidths(headers []string) []int {
	n := len(headers)
	if n == 0 {
		return nil
	}
	// Fixed budgets for known short columns. Values are visible-cell counts.
	fixed := map[string]int{
		"NAME":      14,
		"STATE":     12,
		"ENABLED":   9,
		"RESTARTS":  9,
		"PID":       9,
		"FIELD":     12,
		"CHECK":     12,
		"STATUS":    10,
		"AUTOSTART": 10,
	}
	widths := make([]int, n)
	used := 0
	for i, h := range headers {
		if w, ok := fixed[strings.ToUpper(strings.TrimSpace(h))]; ok {
			widths[i] = w
			used += w
		}
	}
	// Distribute the remainder evenly across the non-fixed (long-text)
	// columns. If every column was fixed, they're already set.
	rest := tableWidth - used
	flexCols := 0
	for i := range widths {
		if widths[i] == 0 {
			flexCols++
		}
	}
	if flexCols > 0 {
		per := rest / flexCols
		rem := rest - per*flexCols
		for i := range widths {
			if widths[i] == 0 {
				widths[i] = per
			}
		}
		widths[n-1] += rem // remainder to last column
	}
	// Fallback: if all columns were "flex" (unknown headers), use even split.
	if used == 0 {
		base := tableWidth / n
		for i := range widths {
			widths[i] = base
		}
		widths[n-1] = tableWidth - base*(n-1)
	}
	return widths
}

// Separator queues a horizontal rule across the table width.
func (t *Table) Separator() {
	t.rows = append(t.rows, []string{strings.Repeat("─", tableWidth)})
}

// Row queues one data row. Each cell is padded to its column's visible width
// via lipgloss.Width, so ANSI-styled cells align correctly.
func (t *Table) Row(cells ...string) {
	padded := make([]string, len(t.widths))
	for i := range t.widths {
		c := ""
		if i < len(cells) {
			c = cells[i]
		}
		padded[i] = t.padCell(c, t.widths[i])
	}
	t.rows = append(t.rows, padded)
}

// RowFull queues one row whose content spans the full table width (useful
// for summary/tally rows). label is left-aligned in the first column; value
// spans the remainder. Pass empty label for a full-width single cell.
func (t *Table) RowFull(label, value string) {
	if label == "" {
		t.rows = append(t.rows, []string{value})
		return
	}
	// Reserve label's column, then give the value the rest.
	labelW := t.widths[0]
	valW := tableWidth - labelW
	t.rows = append(t.rows, []string{
		t.padCell(label, labelW),
		t.padCell(value, valW),
	})
}

// Flush renders all queued rows to the writer. Each row is either a separator
// (single cell) or padded cells joined with "  ".
func (t *Table) Flush() error {
	var b strings.Builder
	for _, row := range t.rows {
		if len(row) == 1 && strings.HasPrefix(row[0], "─") {
			b.WriteString(row[0])
			b.WriteByte('\n')
			continue
		}
		b.WriteString(strings.Join(row, "  "))
		b.WriteByte('\n')
	}
	_, err := io.WriteString(t.w, b.String())
	return err
}

// padCell pads s with trailing spaces to exactly width visible cells.
// lipgloss.Width measures the *visible* width (ignoring ANSI escapes), so
// this works on both styled and plain strings.
func (t *Table) padCell(s string, width int) string {
	return lipgloss.NewStyle().Width(width).Render(s)
}

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
// don't want styling.
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

// --- terminal helpers for the CLI attach verb ------------------------------

// termWidth returns the visible column count of stdout, or 80 if unknown.
func termWidth() int {
	if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil && w > 0 {
		return w
	}
	return 80
}

// termHeight returns the visible row count of stdout, or 24 if unknown.
func termHeight() int {
	if _, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil && h > 0 {
		return h
	}
	return 24
}

// makeRaw puts f (typically os.Stdin) into raw mode so keystrokes pass
// through to the harness untouched. Returns the previous state so it can be
// restored. If raw mode isn't supported (no TTY), returns an error and the
// caller falls back to cooked mode.
func makeRaw(f *os.File) (*term.State, error) {
	return term.MakeRaw(int(f.Fd()))
}

// restoreTerm restores f to the previously-captured state.
func restoreTerm(f *os.File, state *term.State) {
	_ = term.Restore(int(f.Fd()), state)
}

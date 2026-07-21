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
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"

	"gitea.stump.rocks/stump.wtf/harness/internal/cliui"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/tui/theme"
)

// tableWidth is the column budget every CLI table shares. It matches the
// styled error/warn/info/success box width so a `list` next to a `doctor`
// report reads as a consistent surface.
//
// Note: this is the sum of *cell content* widths. Flush joins cells with
// colSep ("  ", 2 cells) between each pair, so the actual rendered row width
// is tableWidth + colSep*(n-1). Separator rules use renderedWidth(headers)
// for that reason (PR #23 nit).
const tableWidth = 80

// colSep is the visible-space separator inserted between cells on a row.
const colSep = "  "

// renderedWidth returns the actual visible width of a rendered row:
// the sum of column widths plus the separators between them. Separator rules
// are drawn to this width so they span the whole row, not just the cell
// content budget (PR #23 nit: separator under-spanned data rows).
func renderedWidth(nCols int) int {
	if nCols <= 1 {
		return tableWidth
	}
	return tableWidth + (len(colSep) * (nCols - 1))
}

// useColorFor reports whether w is a terminal we should style for and we're
// not in --json mode. Tables write to either stdout (list/describe/profiles/
// daemon-info) or stderr (doctor); styling must key off the *actual*
// destination so `harness list | cat` doesn't leak ANSI into the pipe (stdout
// is now a pipe, not a TTY, even though stderr still is) and `harness list
// 2>/dev/null` keeps color on a real terminal. See M2 in PR #23 review.
func useColorFor(w io.Writer) bool {
	return !cliui.JSON() && cliui.WriterIsTTY(w)
}

// palette is the shared design palette, looked up once.
func palette() theme.Palette { return theme.Default().Palette }

// Table is a tabular renderer with bold headers, separator rules, and
// ANSI-aware column alignment. The zero value is not usable; use NewTable.
type Table struct {
	w          io.Writer
	colored    bool
	pal        theme.Palette
	widths     []int      // visible width per column
	truncators []bool     // truncate-with-ellipsis (true) vs wrap (false) per column
	rows       []tableRow // queued rows
}

// tableRow is one queued output row. fullWidth rows span the entire
// tableWidth (single cell); normal rows have one cell per column.
type tableRow struct {
	fullWidth bool
	separator bool
	cells     []string
}

// NewTable starts a table written to w. headers also fixes the column count
// and the visible width budget for each column. Call Row any number of times,
// then Flush.
func NewTable(w io.Writer, headers ...string) *Table {
	colored := useColorFor(w)
	pal := palette()
	widths, trunc := defaultColumnWidths(headers)
	t := &Table{
		w:          w,
		colored:    colored,
		pal:        pal,
		widths:     widths,
		truncators: trunc,
	}
	// Bold + pad each header cell to its column width.
	styled := make([]string, len(headers))
	for i, h := range headers {
		styled[i] = t.wrapCell(t.bold(h), t.widths[i], true) // headers never wrap
	}
	t.rows = append(t.rows, tableRow{cells: styled})
	// Header rule directly under the header row, so the table reads as a
	// header + body block (rounded separator rules per the file docstring).
	t.Separator()
	return t
}

// defaultColumnWidths picks per-column widths that fit tableWidth, and reports
// which columns should truncate (vs wrap) on overflow. Short known columns
// (NAME/STATE/ENABLED/RESTARTS/PID/FIELD/CHECK/STATUS/AUTOSTART) get fixed
// budgets and truncate-with-ellipsis — they hold structured identifiers and a
// wrapped NAME like "crush-signal-\nchannel" would corrupt the row layout
// (lipgloss.Width-based wrapping gets joined line-wise with the next column).
// Everything else (typically DESCRIPTION, DETAIL, VALUE) absorbs the remainder
// and wraps on overflow, since long prose is the common case there.
func defaultColumnWidths(headers []string) (widths []int, truncate []bool) {
	n := len(headers)
	if n == 0 {
		return nil, nil
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
	widths = make([]int, n)
	truncate = make([]bool, n)
	used := 0
	for i, h := range headers {
		if w, ok := fixed[strings.ToUpper(strings.TrimSpace(h))]; ok {
			widths[i] = w
			truncate[i] = true
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
	return widths, truncate
}

// Separator queues a horizontal rule across the table width.
// Separator queues a horizontal rule sized to the table's rendered row
// width (cell budget + inter-cell separators), so it spans the whole row
// rather than under-spanning (PR #23 nit).
func (t *Table) Separator() {
	w := renderedWidth(len(t.widths))
	t.rows = append(t.rows, tableRow{separator: true, cells: []string{strings.Repeat("─", w)}})
}

// Row queues one data row. Cells that overflow their column width either
// truncate with an ellipsis (fixed/structured columns: NAME, STATE, …) or wrap
// onto continuation lines indented to the column's left edge (long-text
// columns: DESCRIPTION, DETAIL, VALUE).
func (t *Table) Row(cells ...string) {
	padded := make([]string, len(t.widths))
	for i := range t.widths {
		c := ""
		if i < len(cells) {
			c = cells[i]
		}
		trunc := i < len(t.truncators) && t.truncators[i]
		padded[i] = t.wrapCell(c, t.widths[i], trunc)
	}
	t.rows = append(t.rows, tableRow{cells: padded})
}

// RowFull queues one row whose content spans the full table width (useful
// for summary/tally rows). label is left-aligned in the first column; value
// spans the remainder. Pass empty label for a full-width single cell. Both
// cells wrap on overflow (RowFull is used for prose/tally, not structured
// identifiers).
func (t *Table) RowFull(label, value string) {
	if label == "" {
		t.rows = append(t.rows, tableRow{fullWidth: true, cells: []string{t.wrapCell(value, tableWidth, false)}})
		return
	}
	labelW := 0
	if len(t.widths) > 0 {
		labelW = t.widths[0]
	}
	valW := tableWidth - labelW
	t.rows = append(t.rows, tableRow{fullWidth: true, cells: []string{
		t.wrapCell(label, labelW, false),
		t.wrapCell(value, valW, false),
	}})
}

// wrapCell renders c into width visible columns. When truncate is true, an
// over-long cell is cut to width-1 cells and suffixed with "…"; when false,
// the cell wraps onto continuation lines joined by "\n" (the caller, Flush,
// re-indents continuation lines under the column's left edge).
func (t *Table) wrapCell(c string, width int, truncate bool) string {
	if lipgloss.Width(c) <= width {
		// Pad short content to width so it aligns in the row.
		return lipgloss.NewStyle().Width(width).Render(c)
	}
	if truncate {
		return lipgloss.NewStyle().Width(width).Render(truncateCell(c, width))
	}
	return strings.Join(wrapWords(c, width), "\n")
}

// truncateCell returns the longest prefix of c whose visible width is <=
// width-1, followed by "…". ANSI escapes in c are preserved (they don't
// count toward visible width). Widths <= 1 just return the ellipsis.
func truncateCell(c string, width int) string {
	if width <= 1 {
		if width == 1 {
			return "…"
		}
		return c
	}
	target := width - 1
	var b strings.Builder
	for _, r := range c {
		next := b.String() + string(r)
		if lipgloss.Width(next) > target {
			break
		}
		b.WriteRune(r)
	}
	return b.String() + "…"
}

// wrapWords splits s into lines no wider than width visible cells, breaking
// on spaces. ANSI escapes are preserved (they don't count toward visible
// width). A single word longer than width is hard-broken at the boundary.
func wrapWords(s string, width int) []string {
	if width < 1 {
		return []string{s}
	}
	var (
		out  []string
		line strings.Builder
	)
	fields := strings.Fields(s)
	for _, word := range fields {
		// Hard-break an over-long word.
		for lipgloss.Width(word) > width {
			if line.Len() > 0 {
				out = append(out, line.String())
				line.Reset()
			}
			// Trim word to width visible cells. We don't have a clean
			// ANSI-aware truncator here, so byte-trim and accept that a
			// multi-byte glyph at the cut is unlikely (Latin-1 paths).
			cut := truncateVisible(word, width)
			out = append(out, cut)
			word = word[len(cut):]
		}
		if line.Len() == 0 {
			line.WriteString(word)
			continue
		}
		// " " + word would overflow; start a new line.
		if lipgloss.Width(line.String())+1+lipgloss.Width(word) > width {
			out = append(out, line.String())
			line.Reset()
			line.WriteString(word)
		} else {
			line.WriteByte(' ')
			line.WriteString(word)
		}
	}
	if line.Len() > 0 {
		out = append(out, line.String())
	}
	if len(out) == 0 {
		return []string{""}
	}
	return out
}

// truncateVisible returns the longest prefix of s whose visible width is
// <= width. Used by wrapWords for hard-breaking over-long words.
func truncateVisible(s string, width int) string {
	var b strings.Builder
	for _, r := range s {
		b.WriteRune(r)
		if lipgloss.Width(b.String()) > width {
			// Back up one rune.
			cur := b.String()
			_, sz := utf8.DecodeLastRuneInString(cur)
			return cur[:len(cur)-sz]
		}
	}
	return b.String()
}

// Flush renders all queued rows to the writer. Separator rows are a single
// horizontal rule. fullWidth rows are rendered as label+value (or just value).
// Normal rows have each cell padded to its column width via lipgloss.Width,
// and when a cell wraps to multiple lines, the row is expanded so
// continuation text aligns under its own column's left edge.
func (t *Table) Flush() error {
	var b strings.Builder
	for _, row := range t.rows {
		if row.separator {
			b.WriteString(row.cells[0])
			b.WriteByte('\n')
			continue
		}
		if row.fullWidth {
			// Full-width row: cells are [value] or [label, value], already
			// wrapped to their widths. Join label+value with colSep.
			for ln, line := range strings.Split(strings.Join(row.cells, colSep), "\n") {
				if ln > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(line)
			}
			b.WriteByte('\n')
			continue
		}
		// Normal multi-column row. Split each cell into wrapped lines and
		// emit maxLines output lines so continuation text aligns.
		cellLines := make([][]string, len(row.cells))
		maxLines := 1
		for i, cell := range row.cells {
			lines := strings.Split(cell, "\n")
			cellLines[i] = lines
			if len(lines) > maxLines {
				maxLines = len(lines)
			}
		}
		for ln := 0; ln < maxLines; ln++ {
			parts := make([]string, len(row.cells))
			for i := range row.cells {
				w := tableWidth
				if i < len(t.widths) {
					w = t.widths[i]
				}
				if ln < len(cellLines[i]) {
					parts[i] = lipgloss.NewStyle().Width(w).Render(cellLines[i][ln])
				} else {
					parts[i] = strings.Repeat(" ", w)
				}
			}
			b.WriteString(strings.Join(parts, colSep))
			b.WriteByte('\n')
		}
	}
	_, err := io.WriteString(t.w, b.String())
	return err
}

// bold renders s bold in the foreground color when coloring is on.
func (t *Table) bold(s string) string {
	if !t.colored {
		return s
	}
	return lipgloss.NewStyle().Foreground(t.pal.Fg).Bold(true).Render(s)
}

// --- cell helpers (not methods; usable inline in Row calls) ----------------

// --- cell helpers (methods on Table so they consult t.colored, which is ---
// --- keyed off the table's actual writer — see useColorFor, PR #23 M2). ---

// stateCell renders "● running" in the state's palette color (paired glyph +
// label per SPEC-0001). The glyph always accompanies the color so a mono
// terminal that drops the color still fully conveys the state.
func (t *Table) stateCell(state string) string {
	s := core.State(state)
	glyph := stateGlyphFor(s)
	label := string(s)
	if !t.colored {
		return fmt.Sprintf("%s %s", glyph, label)
	}
	return lipgloss.NewStyle().Foreground(stateColor(s, t.pal)).Bold(true).
		Render(fmt.Sprintf("%s %s", glyph, label))
}

// stateGlyphOnly renders just the colored glyph for leading-column use.
func (t *Table) stateGlyphOnly(state string) string {
	s := core.State(state)
	glyph := stateGlyphFor(s)
	if !t.colored {
		return glyph
	}
	return lipgloss.NewStyle().Foreground(stateColor(s, t.pal)).Render(glyph)
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
func (t *Table) enabledCell(on bool) string {
	if on {
		return t.mintBold("yes")
	}
	return t.dimPlain("no")
}

// flappingCell renders the flapping flag with a warning glyph when true.
func (t *Table) flappingCell(on bool) string {
	if on {
		return t.amberBold("⚠ flapping")
	}
	return t.dimPlain("no")
}

// pidCell renders "-" in dim when pid <= 0, else the number in faint.
func (t *Table) pidCell(p int) string {
	if p <= 0 {
		return t.dimPlain("-")
	}
	return t.faintPlain(fmt.Sprintf("%d", p))
}

// yesno returns "yes" or "no" — the plain-text form used by callers that
// don't want styling.
func yesno(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// --- low-level styled primitives (methods on Table; consult t.colored) -----

func (t *Table) mintBold(s string) string {
	if !t.colored {
		return s
	}
	return lipgloss.NewStyle().Foreground(t.pal.Mint).Bold(true).Render(s)
}

func (t *Table) dimPlain(s string) string {
	if !t.colored {
		return s
	}
	return lipgloss.NewStyle().Foreground(t.pal.Dim).Render(s)
}

func (t *Table) faintPlain(s string) string {
	if !t.colored {
		return s
	}
	return lipgloss.NewStyle().Foreground(t.pal.Faint).Render(s)
}

func (t *Table) amberBold(s string) string {
	if !t.colored {
		return s
	}
	return lipgloss.NewStyle().Foreground(t.pal.Amber).Bold(true).Render(s)
}

func (t *Table) accentBold(s string) string {
	if !t.colored {
		return s
	}
	return lipgloss.NewStyle().Foreground(t.pal.Accent).Bold(true).Render(s)
}

func (t *Table) dimItalic(s string) string {
	if !t.colored {
		return s
	}
	return lipgloss.NewStyle().Foreground(t.pal.Dim).Italic(true).Render(s)
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

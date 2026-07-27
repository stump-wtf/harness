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

// Table sizing. The table targets tableWidthRatio of the terminal window
// (clamped to [minTableWidth, maxTableWidth]) so it fills more of a wide
// terminal instead of hugging the left 80 columns. When the writer isn't a
// TTY (piped, test buffer) we fall back to defaultTableWidth — the
// historical constant that also matches the styled error/warn/info/success
// box width so a `list` next to a `doctor` report reads as one surface.
const (
	defaultTableWidth = 80
	tableWidthRatio   = 0.8
	minTableWidth     = 60
	maxTableWidth     = 160
	// maxNameWidth caps how wide the NAME column may grow to fit the longest
	// harness name. NAME is never truncated (it is the harness's identity),
	// but an unbounded name would starve every other column; beyond this cap
	// the name wraps onto continuation lines instead of being cut.
	maxNameWidth = 40
)

// colSep is the visible-space separator inserted between cells on a row.
const colSep = "  "

// resolveTableWidth returns the column budget for a table writing to w.
// When w is a terminal, the budget is 80% of its column count, clamped to
// [minTableWidth, maxTableWidth]; otherwise (pipe, file, test buffer) it's
// defaultTableWidth. This is the same shape of policy cliui.blockWidth
// applies to the styled error box, kept separate because tables can grow
// wider than the error box.
func resolveTableWidth(w io.Writer) int {
	if f, ok := w.(*os.File); ok {
		if tw, _, err := term.GetSize(int(f.Fd())); err == nil && tw > 0 {
			target := int(float64(tw) * tableWidthRatio)
			if target < minTableWidth {
				return minTableWidth
			}
			if target > maxTableWidth {
				return maxTableWidth
			}
			return target
		}
	}
	return defaultTableWidth
}

// renderedWidth returns the actual visible width of a rendered row for a
// table of nCols columns and cell-content budget budget: the cell budget
// plus the separators between cells. Separator rules are drawn to this
// width so they span the whole row (PR #23 nit: separator under-spanned
// data rows).
func renderedWidth(budget, nCols int) int {
	if nCols <= 1 {
		return budget
	}
	return budget + (len(colSep) * (nCols - 1))
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
	w         io.Writer
	colored   bool
	pal       theme.Palette
	width     int        // cell-content budget for this table (resolved from writer's TTY size)
	headers   []string   // raw header labels; column count + fit/truncate policy key off these
	rows      []tableRow // queued rows (raw, unwrapped cells; laid out in Flush)
	widths    []int      // resolved per-column widths; computed lazily in Flush
	truncated []bool     // resolved per-column truncate policy; computed with widths
}

// tableRow is one queued output row. fullWidth rows span the entire
// table cell-content budget (t.width, single cell); normal rows have one
// cell per column. Cells are stored raw (unwrapped, unpadded); Flush
// resolves column widths — measuring the NAME column's content so a name is
// never cut — and only then wraps/pads.
type tableRow struct {
	fullWidth bool
	separator bool
	header    bool // bold every cell (the NewTable header row)
	cells     []string
}

// NewTable starts a table written to w. headers fixes the column count and
// the per-column fit/truncate policy. The cell-content budget is resolved
// from w's terminal size (~80% of the window, clamped) or falls back to
// defaultTableWidth when w isn't a TTY. Call Row any number of times, then
// Flush — column widths are resolved at Flush so the NAME column can size
// itself to the longest name in the data (never truncated; the DESCRIPTION
// column absorbs whatever budget remains and wraps).
func NewTable(w io.Writer, headers ...string) *Table {
	t := &Table{
		w:       w,
		colored: useColorFor(w),
		pal:     palette(),
		width:   resolveTableWidth(w),
		headers: append([]string(nil), headers...),
	}
	// Header row (raw labels) + the rule directly under it, so the table
	// reads as a header + body block (rounded separator rules per the file
	// docstring). Bold is applied at Flush once widths are known.
	t.rows = append(t.rows, tableRow{cells: append([]string(nil), headers...), header: true})
	t.Separator()
	return t
}

// defaultColumnWidths picks per-column widths that fit budget (the table's
// cell-content budget), and reports which columns should truncate (vs wrap)
// on overflow.
//
// NAME is special: it is the harness's identity, so it is NEVER truncated.
// Its width is measured from the longest name in the data (nameWidth,
// pre-computed by Flush across all rows, capped at maxNameWidth so one
// pathological name can't starve the table). If a name still exceeds the
// cap it wraps onto continuation lines rather than being cut — Flush joins
// wrapped cells line-wise per column, so a multi-line NAME no longer
// corrupts the row the way it would have under the old naive join (PR #23
// M1's original concern). The DESCRIPTION column absorbs whatever budget
// remains and wraps (long prose is the common case there), so when space is
// tight the description is sacrificed, never the name.
//
// The other short known columns (STATE/ENABLED/RESTARTS/PID/FIELD/CHECK/
// STATUS/AUTOSTART) keep fixed budgets and truncate-with-ellipsis — they
// hold structured values where a cut is safe.
func defaultColumnWidths(headers []string, budget, nameWidth int) (widths []int, truncate []bool) {
	n := len(headers)
	if n == 0 {
		return nil, nil
	}
	// Fixed budgets for known short columns. Values are visible-cell counts.
	// NAME is deliberately absent: its width comes from the measured
	// nameWidth, and it wraps (truncate stays false) so it is never cut.
	fixed := map[string]int{
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
		key := strings.ToUpper(strings.TrimSpace(h))
		if key == "NAME" {
			// Fit the measured longest name, but never below the header
			// label and never above the cap.
			w := nameWidth
			if w < len("NAME") {
				w = len("NAME")
			}
			if w > maxNameWidth {
				w = maxNameWidth
			}
			widths[i] = w
			used += w
			continue
		}
		if w, ok := fixed[key]; ok {
			widths[i] = w
			truncate[i] = true
			used += w
		}
	}
	// Distribute the remainder evenly across the non-fixed (long-text)
	// columns. If every column was fixed, they're already set.
	rest := budget - used
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
		base := budget / n
		for i := range widths {
			widths[i] = base
		}
		widths[n-1] = budget - base*(n-1)
	}
	return widths, truncate
}

// Separator queues a horizontal rule sized to the table's rendered row
// width (cell budget + inter-cell separators), so it spans the whole row
// rather than under-spanning (PR #23 nit).
func (t *Table) Separator() {
	w := renderedWidth(t.width, len(t.headers))
	t.rows = append(t.rows, tableRow{separator: true, cells: []string{strings.Repeat("─", w)}})
}

// Row queues one data row. Cells are stored raw; Flush resolves the column
// widths (measuring NAME content) and then lays each cell out — truncating
// with an ellipsis in a fixed/structured column (STATE, ENABLED, …) or
// wrapping onto continuation lines in a fit/wrap column (NAME, DESCRIPTION,
// DETAIL, VALUE), with continuation text aligned under the column's left
// edge.
func (t *Table) Row(cells ...string) {
	raw := make([]string, len(t.headers))
	for i := range t.headers {
		if i < len(cells) {
			raw[i] = cells[i]
		}
	}
	t.rows = append(t.rows, tableRow{cells: raw})
}

// RowFull queues one row whose content spans the full table width (useful
// for summary/tally rows). label is left-aligned in the first column; value
// spans the remainder. Pass empty label for a full-width single cell. Both
// cells wrap on overflow (RowFull is used for prose/tally, not structured
// identifiers).
func (t *Table) RowFull(label, value string) {
	t.rows = append(t.rows, tableRow{fullWidth: true, cells: []string{label, value}})
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

// Flush renders all queued rows to the writer. It first resolves the column
// widths: the NAME column is measured from the longest name across every
// queued row (capped at maxNameWidth) so a name is never cut, and the
// remaining budget is split across the fixed and flex columns
// (defaultColumnWidths). Each cell is then laid out — truncated in a
// fixed/structured column, wrapped in a fit/wrap column — and padded to its
// column width via lipgloss.Width; when a cell wraps to multiple lines the
// row expands so continuation text aligns under its own column's left edge.
func (t *Table) Flush() error {
	t.resolveWidths()
	var b strings.Builder
	for _, row := range t.rows {
		if row.separator {
			b.WriteString(row.cells[0])
			b.WriteByte('\n')
			continue
		}
		if row.fullWidth {
			// Full-width row: cells are [label, value] (label may be "").
			// Wrap to the full budget, then join label+value with colSep.
			label, value := row.cells[0], row.cells[1]
			var out string
			if label == "" {
				out = t.wrapCell(value, t.width, false)
			} else {
				labelW := 0
				if len(t.widths) > 0 {
					labelW = t.widths[0]
				}
				out = strings.Join([]string{
					t.wrapCell(label, labelW, false),
					t.wrapCell(value, t.width-labelW, false),
				}, colSep)
			}
			for ln, line := range strings.Split(out, "\n") {
				if ln > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(line)
			}
			b.WriteByte('\n')
			continue
		}
		// Normal multi-column row. Lay out each cell (truncate or wrap +
		// bold for the header), split into lines, and emit maxLines output
		// lines so continuation text aligns under its own column.
		cellLines := make([][]string, len(row.cells))
		maxLines := 1
		for i, cell := range row.cells {
			w := t.colWidth(i)
			trunc := i < len(t.truncated) && t.truncated[i]
			if row.header {
				cell = t.bold(cell)
				trunc = true // headers never wrap
			}
			lines := strings.Split(t.wrapCell(cell, w, trunc), "\n")
			cellLines[i] = lines
			if len(lines) > maxLines {
				maxLines = len(lines)
			}
		}
		for ln := 0; ln < maxLines; ln++ {
			parts := make([]string, len(row.cells))
			for i := range row.cells {
				w := t.colWidth(i)
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

// colWidth returns column i's resolved width (falling back to the table
// budget when out of range).
func (t *Table) colWidth(i int) int {
	if i < len(t.widths) {
		return t.widths[i]
	}
	return t.width
}

// resolveWidths measures the NAME column's content across every queued row
// and computes the final per-column widths and truncate policy. It is
// idempotent (a second call is a no-op), so a Table can be flushed once.
func (t *Table) resolveWidths() {
	if t.widths != nil {
		return
	}
	// The NAME column fits the longest name in the data. Bold styling on the
	// header adds escape sequences, so measure the raw label, not the styled
	// form; lipgloss.Width is ANSI-aware, but the raw header is unstyled here
	// anyway.
	nameW := 0
	nameIdx := -1
	for i, h := range t.headers {
		if strings.ToUpper(strings.TrimSpace(h)) == "NAME" {
			nameIdx = i
			break
		}
	}
	if nameIdx >= 0 {
		for _, row := range t.rows {
			if row.separator || row.fullWidth {
				continue
			}
			if nameIdx < len(row.cells) {
				if w := lipgloss.Width(row.cells[nameIdx]); w > nameW {
					nameW = w
				}
			}
		}
	}
	t.widths, t.truncated = defaultColumnWidths(t.headers, t.width, nameW)
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
// degraded→amber, transient→cyan, stopped→pink, failed→coral). Stopped is
// pink (a warm/red-family hue) rather than dim so a stopped harness draws
// the eye to its state at a glance, like running/degraded/failed do; the
// glyph (○ vs ✖) still distinguishes it from failed. Mirrors
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
		return pal.Pink
	case core.StateFailed:
		return pal.Coral
	default:
		return pal.Fg
	}
}

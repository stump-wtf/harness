package tui

// Governing: SPEC-0001 REQ "Attached Mode" — render the harness's real terminal
// from the daemon's x/vt screen: colors, cursor, and TUI apps inside it all
// work (ADR-0003, embedded terminal pane). The daemon streams a screen snapshot
// then live ATTACH_DATA bytes (SPEC-0002 REQ "Attach Session"); we feed those
// into a CLIENT-side x/vt emulator and render its cell grid into lines Bubble
// Tea prints. Because both ends run the same emulator, a full-screen TUI app
// (colors + cursor) reproduces faithfully.

import (
	"strings"

	"github.com/charmbracelet/x/vt"
)

// vtView is a client-side embedded terminal: an x/vt emulator fed the daemon's
// ATTACH_DATA byte stream, rendered on demand into styled lines.
type vtView struct {
	term *vt.Terminal
	cols int
	rows int
}

// newVTView creates an embedded terminal of the given size.
func newVTView(cols, rows int) *vtView {
	if cols < 1 {
		cols = 1
	}
	if rows < 1 {
		rows = 1
	}
	return &vtView{term: vt.NewTerminal(cols, rows), cols: cols, rows: rows}
}

// resize resizes the emulator (the client viewport changed; smallest-attached-
// wins is enforced server-side, but the local emulator must match what the
// daemon renders into).
func (v *vtView) resize(cols, rows int) {
	if cols < 1 || rows < 1 || (cols == v.cols && rows == v.rows) {
		return
	}
	v.cols, v.rows = cols, rows
	v.term.Resize(cols, rows)
}

// write feeds raw terminal bytes (the ATTACH_DATA payload) into the emulator.
func (v *vtView) write(p []byte) {
	if len(p) > 0 {
		_, _ = v.term.Write(p)
	}
}

// render serializes the current screen into styled lines joined by newlines,
// suitable for embedding in a Bubble Tea view. Each cell emits an SGR sequence
// only when the style changes (compact), and every line resets attributes at
// its end so a truncated line can't bleed color into the chrome around it.
func (v *vtView) render() string {
	w, h := v.term.Width(), v.term.Height()
	var lines []string
	for y := 0; y < h; y++ {
		var b strings.Builder
		prevSeq := ""
		skip := 0
		for x := 0; x < w; x++ {
			if skip > 0 {
				skip--
				continue
			}
			cell := v.term.Cell(x, y)
			if cell == nil {
				b.WriteByte(' ')
				continue
			}
			if seq := cell.Style.Sequence(); seq != prevSeq {
				b.WriteString("\x1b[0m")
				if seq != "" {
					b.WriteString(seq)
				}
				prevSeq = seq
			}
			s := cell.String()
			if s == "" {
				b.WriteByte(' ')
			} else {
				b.WriteString(s)
				if cell.Width > 1 {
					skip = cell.Width - 1
				}
			}
		}
		b.WriteString("\x1b[0m")
		lines = append(lines, b.String())
	}
	return strings.Join(lines, "\n")
}

// cursor returns the emulator's current cursor position (col, row), 0-indexed.
func (v *vtView) cursor() (int, int) {
	pos := v.term.CursorPosition()
	return pos.X, pos.Y
}

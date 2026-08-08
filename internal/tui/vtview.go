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
//
// The emulator's cursor is painted into the grid as a reverse-video cell
// (#48): Bubble Tea owns the hardware cursor — it hides it for the program's
// lifetime and re-parks it after every flush — so the only way to show the
// guest cursor is to draw it as cell content. The guest's DECTCEM state is
// respected: a program that hides its cursor (\x1b[?25l) gets no painted
// cursor either.
func (v *vtView) render() string { return v.renderScreen(true) }

// renderNoCursor is render without the painted cursor cell — for frozen
// frames (the scrollback snapshot, #50), where a reverse-video cursor cell
// would masquerade as live state in a view that has no live cursor to show.
func (v *vtView) renderNoCursor() string { return v.renderScreen(false) }

// renderScreen is the shared implementation behind render / renderNoCursor.
func (v *vtView) renderScreen(paintCursor bool) string {
	w, h := v.term.Width(), v.term.Height()
	cur := v.term.Screen().Cursor()
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
			atCursor := paintCursor && !cur.Hidden && x == cur.X && y == cur.Y
			cell := v.term.Cell(x, y)
			if cell == nil {
				if atCursor {
					b.WriteString("\x1b[0m\x1b[7m \x1b[0m")
					// The reset above invalidated the tracked run; force the
					// next styled cell to restate its sequence.
					prevSeq = "\x00"
				} else {
					b.WriteByte(' ')
				}
				continue
			}
			if atCursor {
				// Cursor cell: the cell's own style plus reverse video,
				// closed immediately so the inversion can't bleed.
				b.WriteString("\x1b[0m")
				if seq := cell.Style.Sequence(); seq != "" {
					b.WriteString(seq)
				}
				b.WriteString("\x1b[7m")
			} else if seq := cell.Style.Sequence(); seq != prevSeq {
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
			if atCursor {
				b.WriteString("\x1b[0m")
				prevSeq = "\x00"
			}
		}
		b.WriteString("\x1b[0m")
		lines = append(lines, b.String())
	}
	return strings.Join(lines, "\n")
}

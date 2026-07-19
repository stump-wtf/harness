package attach

// Governing: SPEC-0002 REQ "Attach Session" ("a screen snapshot (repaint of the
// current x/vt screen)" sent before any live bytes) and REQ "Backpressure
// Isolation" (a coalesced client "receives a snapshot repaint instead of the
// backlog"); ADR-0003 (the daemon owns the x/vt emulator and renders its
// screen). renderScreen turns the emulator's current cell grid into an ANSI
// byte stream a client terminal repaints verbatim.

import (
	"bytes"
	"fmt"

	"github.com/charmbracelet/x/vt"
)

// renderScreen serializes the emulator's current screen into an ANSI repaint:
// clear + home, each row's styled cells, then the cursor parked at its live
// position. The result is self-contained — a client that writes these bytes to
// its terminal ends up with a pixel-faithful copy of the harness screen, which
// is exactly the "correct before any live bytes arrive" guarantee (SPEC-0002).
func renderScreen(t *vt.Terminal) []byte {
	w, h := t.Width(), t.Height()
	var b bytes.Buffer
	// Reset attributes, clear the whole screen, home the cursor.
	b.WriteString("\x1b[0m\x1b[2J\x1b[H")

	prevSeq := ""
	for y := 0; y < h; y++ {
		// Absolute-position the start of each row so a short/again-sized client
		// still lands rows correctly.
		fmt.Fprintf(&b, "\x1b[%d;1H", y+1)
		skip := 0
		for x := 0; x < w; x++ {
			if skip > 0 {
				skip--
				continue
			}
			cell := t.Cell(x, y)
			if cell == nil {
				b.WriteByte(' ')
				continue
			}
			// Emit an SGR sequence only when the style changes from the
			// previous cell, keeping the repaint compact.
			if seq := cell.Style.Sequence(); seq != prevSeq {
				b.WriteString(seq)
				prevSeq = seq
			}
			s := cell.String()
			if s == "" {
				// An empty/never-written cell renders as a space so column
				// alignment is preserved.
				b.WriteByte(' ')
			} else {
				b.WriteString(s)
				if cell.Width > 1 {
					// A wide grapheme already advanced the cursor across its
					// trailing continuation column(s); skip them.
					skip = cell.Width - 1
				}
			}
		}
	}
	// Restore default attributes and park the cursor where the emulator has it.
	pos := t.CursorPosition()
	b.WriteString("\x1b[0m")
	fmt.Fprintf(&b, "\x1b[%d;%dH", pos.Y+1, pos.X+1)
	return b.Bytes()
}

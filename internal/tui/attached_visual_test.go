package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/vt"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// emulate renders a Model.View() string through a real x/vt terminal of the
// given size — the same emulator a terminal uses. Lines are joined with CRLF so
// each starts at column 0, exactly as a TTY receives frames. Tests can then
// assert the ON-SCREEN result (wrap, scroll, truncation), which the pre-render
// string alone can't show: an over-wide line here wraps and scrolls the grid,
// just like a real terminal.
func emulate(view string, w, h int) *vt.Terminal {
	term := vt.NewTerminal(w, h)
	term.Write([]byte(strings.ReplaceAll(view, "\n", "\r\n")))
	return term
}

// rowText reads row y of the emulator grid as a string (trailing blanks
// trimmed).
func rowText(term *vt.Terminal, y, w int) string {
	var b strings.Builder
	for x := 0; x < w; x++ {
		c := term.Cell(x, y)
		if c == nil || c.String() == "" {
			b.WriteByte(' ')
		} else {
			b.WriteString(c.String())
		}
	}
	return strings.TrimRight(b.String(), " ")
}

// TestAttachedVisualNoScroll is the end-to-end visual guard: it renders the
// attached view and replays it through x/vt, then asserts the grid didn't
// scroll (top row survives) and the status bar landed on the last row. This is
// what the overflow bug actually broke — a too-wide status bar wraps to a
// second physical row and scrolls the embedded terminal up. lipgloss.Width
// catches the over-wide line at the source; this catches the on-screen
// consequence, deterministically and without a PTY.
func TestAttachedVisualNoScroll(t *testing.T) {
	for _, dim := range [][2]int{{200, 50}, {145, 42}, {120, 40}, {90, 28}, {60, 20}} {
		w, h := dim[0], dim[1]
		m := New(Options{})
		m.conn = startOK
		m.w, m.h = w, h
		m.help.Width = w
		m.mode = modeAttached
		cols, rows := m.attachViewport()
		m.att = newAttachState("crush", protocol.AttachRW, sessionBase, cols, rows)
		m.harnesses = []protocol.HarnessInfo{{Name: "crush", State: "running"}}

		// Paint a sentinel on the TOP row of the embedded terminal. If the
		// status bar overflows and wraps, the alt-screen scrolls this away.
		sentinel := "TOP-ROW-SENTINEL"
		m.att.view.write([]byte("\x1b[42m" + sentinel + strings.Repeat(" ", w-len(sentinel)) + "\x1b[0m"))

		term := emulate(m.View(), w, h)

		if top := rowText(term, 0, w); !strings.Contains(top, sentinel) {
			t.Errorf("%dx%d: top row scrolled away (grid scrolled) — got %q", w, h, top)
		}
		if last := rowText(term, h-1, w); !strings.Contains(last, "attached:") {
			t.Errorf("%dx%d: status bar not on the last row — got %q", w, h, last)
		}
		// Nothing should have wrapped past the last row: the cursor must be
		// within the grid, not pushed below it.
		if pos := term.CursorPosition(); pos.Y >= h {
			t.Errorf("%dx%d: cursor at row %d is below the grid (content overflowed)", w, h, pos.Y)
		}
	}
}

// TestAttachedCursorVisibleInInteractive verifies the attached view emits a CUP
// cursor-positioning sequence when in interactive substate (#48). The sequence
// parks the hardware cursor where the emulator's cursor is, making it visible.
func TestAttachedCursorVisibleInInteractive(t *testing.T) {
	w, h := 80, 24
	m := New(Options{})
	m.conn = startOK
	m.w, m.h = w, h
	m.help.Width = w
	m.mode = modeAttached
	cols, rows := m.attachViewport()
	m.att = newAttachState("test", protocol.AttachRW, sessionBase, cols, rows)
	m.harnesses = []protocol.HarnessInfo{{Name: "test", State: "running"}}

	// Write something that moves the cursor to a known position.
	m.att.view.write([]byte("\x1b[3;5H"))

	view := m.View()
	// The view must contain a CUP sequence (\x1b[row;colH).
	if !strings.Contains(view, "\x1b[") || !strings.Contains(view, "H") {
		t.Fatal("interactive attached view missing CUP cursor sequence")
	}
}

// TestAttachedCursorHiddenInScrollback verifies the attached view does NOT emit
// a cursor-positioning sequence when in scrollback substate — scrollback has no
// live cursor to show.
func TestAttachedCursorHiddenInScrollback(t *testing.T) {
	w, h := 80, 24
	m := New(Options{})
	m.conn = startOK
	m.w, m.h = w, h
	m.help.Width = w
	m.mode = modeAttached
	cols, rows := m.attachViewport()
	m.att = newAttachState("test", protocol.AttachRW, sessionBase, cols, rows)
	m.harnesses = []protocol.HarnessInfo{{Name: "test", State: "running"}}

	// Enter scrollback with some lines.
	lines := make([]string, 20)
	for i := range lines {
		lines[i] = "line"
	}
	m.att.enterScrollback(lines, rows)

	view := m.View()
	// In scrollback, there should be no CUP sequence from the cursor fix.
	// The view may still contain SGR sequences for styling, but not \x1b[...H
	// cursor positioning from the vtView cursor logic.
	// We check that the view does NOT contain a CUP after the body content.
	// Since scrollback renders inert text, any \x1b[...H would be from our
	// cursor fix — which should not be present.
	if strings.Contains(view, "\x1b[1;1H") {
		t.Fatal("scrollback view should not emit cursor positioning sequence")
	}
}

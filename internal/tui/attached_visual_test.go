package tui

import (
	"regexp"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/vt"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// emulate renders a Model.View() string through a real x/vt terminal of the
// given size — the same emulator a terminal uses. Lines are joined with CRLF so
// each starts at column 0, exactly as a TTY receives frames. Tests can then
// assert the ON-SCREEN result (wrap, scroll, truncation), which the pre-render
// string alone can't show: an over-wide line here wraps and scrolls the grid,
// just like a real terminal.
func emulate(view string, w, h int) vt.Terminal {
	term := vt.NewEmulator(w, h)
	term.Write([]byte(strings.ReplaceAll(view, "\n", "\r\n")))
	return term
}

// rowText reads row y of the emulator grid as a string (trailing blanks
// trimmed).
func rowText(term vt.Terminal, y, w int) string {
	var b strings.Builder
	for x := 0; x < w; x++ {
		c := term.CellAt(x, y)
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

// cupRE matches CUP (CSI row;col H) cursor-addressing sequences. A rendered
// frame must never contain one: it would move the user's real cursor instead
// of displaying (the invariant ansitext.go documents, and the reason the
// cursor is painted as a reverse-video cell rather than parked via escapes).
var cupRE = regexp.MustCompile("\x1b\\[[0-9;]*H")

// TestAttachedCursorPaintedInInteractive verifies the attached view paints the
// emulator's cursor cell (#48): reverse video at the guest cursor position,
// with no cursor-addressing escapes anywhere in the frame.
func TestAttachedCursorPaintedInInteractive(t *testing.T) {
	w, h := 80, 24
	m := attachedModel(w, h, false)
	// Park the guest cursor at a known position: CUP row 3, col 5 → cell (4, 2).
	m.att.view.write([]byte("\x1b[3;5H"))

	view := m.View()
	lines := strings.Split(view, "\n")

	// The cursor cell must be painted on body row 2 and nowhere else.
	const marker = "\x1b[7m"
	for i, ln := range lines[:h-1] {
		if has := strings.Contains(ln, marker); has != (i == 2) {
			t.Errorf("body row %d: cursor cell present=%v, want %v", i, has, i == 2)
		}
	}
	row := lines[2]
	idx := strings.Index(row, marker)
	if idx < 0 {
		t.Fatalf("no reverse-video cursor cell on body row 2: %q", row)
	}
	if got := lipgloss.Width(row[:idx]); got != 4 {
		t.Errorf("cursor cell painted at column %d, want 4", got)
	}
	if cupRE.MatchString(view) {
		t.Errorf("interactive frame contains cursor-addressing escapes: %q",
			cupRE.FindString(view))
	}
}

// TestAttachedCursorNotPaintedInScrollback verifies scrollback frames stay
// inert: a frozen view has no live cursor to show, so neither a reverse-video
// cursor cell nor any cursor-addressing escape may appear. The screen is
// non-empty with a visible guest cursor, so the appended frame — the path
// every real scrollback entry takes — is what's being checked, not an empty
// fixture.
func TestAttachedCursorNotPaintedInScrollback(t *testing.T) {
	m := scrollbackModelWithScreen(80, 24, rawPTYLog,
		"output line\r\noutput line\r\nshell$ ")
	view := m.View()
	if strings.Contains(view, "\x1b[7m") {
		t.Error("scrollback frame paints a cursor cell")
	}
	if cupRE.MatchString(view) {
		t.Errorf("scrollback frame contains cursor-addressing escapes: %q",
			cupRE.FindString(view))
	}
	// The stored buffer must be clean too, not just the visible window.
	joined := strings.Join(m.att.scroll.lines, "\n")
	if strings.Contains(joined, "\x1b[7m") {
		t.Error("scrollback buffer holds a painted cursor cell")
	}
	if cupRE.MatchString(joined) {
		t.Errorf("scrollback buffer contains cursor-addressing escapes: %q",
			cupRE.FindString(joined))
	}
}

package tui

// Coverage for the dashboard preview rendering an LF-only log tail as a
// staircase.
//
// The polled tail is the durable log, and ADR-0007 was amended so that log
// holds sanitized line-oriented TEXT rather than raw PTY bytes — LF-terminated,
// because the CRs a terminal would have seen come from the tty's ONLCR and
// never reach the file. Replaying it through an emulator verbatim made LF mean
// "down one row" only: every line started where the previous one ended and the
// tail walked off the right edge.
//
// It showed up on headless harnesses because #290 made the tail their pane:
// crush with --channels and claude -p never paint a screen, so all the pane
// ever holds is the daemon's own "state changed" lines — as an unreadable
// diagonal.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// lifecycleTail is what a headless harness's log actually contains: the
// supervisor's own event lines, written by charmbracelet/log as `line + "\n"`.
var lifecycleTail = []string{
	"2026/08/30 12:14:34 INFO state changed from=running to=stopping",
	"2026/08/30 12:14:34 INFO state changed from=stopping to=stopped",
	"2026/08/30 12:16:04 INFO state changed from=starting to=running",
	"2026/08/30 15:45:20 INFO state changed from=starting to=running",
}

// peekRows returns the preview's rendered rows with styling and the pane's
// own border stripped, so a row is exactly the text the pane shows on it.
func peekRows(m *Model, w, h int) []string {
	out := strings.Split(m.viewPeek(w, h), "\n")
	for i, ln := range out {
		row := strings.Trim(ansi.Strip(ln), "│╭╮╰╯")
		out[i] = strings.TrimRight(row, " ")
	}
	return out
}

// headlessPeekModel is a dashboard whose selection is a harness with no live
// session — the polled tail is the pane.
func headlessPeekModel(tail string) *Model {
	m := baseModel(120, 45)
	m.profiles = nil
	m.harnesses = []protocol.HarnessInfo{{Name: "crush-signal", State: "running"}}
	m.sel = 0
	m.peek = logsMsg{name: "crush-signal", text: tail}
	return m
}

// TestPeekTailLinesStartAtColumnZero is the regression: every line of an
// LF-only tail must begin its own row, not continue the previous one.
func TestPeekTailLinesStartAtColumnZero(t *testing.T) {
	m := headlessPeekModel(strings.Join(lifecycleTail, "\n") + "\n")

	rows := peekRows(m, 110, 30)
	for _, want := range lifecycleTail {
		found := false
		for _, row := range rows {
			// Whole line, flush left: a staircased row carries the line after
			// a run of spaces, and a wrapped one carries only a fragment.
			if row == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("tail line not rendered on a row of its own: %q\n--- pane ---\n%s",
				want, strings.Join(rows, "\n"))
		}
	}
}

// TestPeekTailKeepsCRLFHistory guards the older logs: guest output recorded
// before the ADR-0007 amendment already carries CRLF, and putting a second CR
// in front of those LFs must not change what they render as.
func TestPeekTailKeepsCRLFHistory(t *testing.T) {
	crlf := headlessPeekModel("first line\r\nsecond line\r\n")
	lf := headlessPeekModel("first line\nsecond line\n")

	got := strings.Join(peekRows(crlf, 110, 30), "\n")
	want := strings.Join(peekRows(lf, 110, 30), "\n")
	if got != want {
		t.Errorf("CRLF tail renders differently from the LF-only one:\n--- crlf ---\n%s\n--- lf ---\n%s", got, want)
	}
}

// TestONLCR pins the translation itself, including the pair it must leave alone.
func TestONLCR(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{"bare lf", "a\nb", "a\r\nb"},
		{"already crlf", "a\r\nb", "a\r\nb"},
		{"mixed", "a\r\nb\nc", "a\r\nb\r\nc"},
		{"lone cr untouched", "a\rb", "a\rb"},
		{"no newline", "abc", "abc"},
		{"empty", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(onlcr([]byte(tc.in))); got != tc.want {
				t.Errorf("onlcr(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

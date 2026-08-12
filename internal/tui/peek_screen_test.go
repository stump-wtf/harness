package tui

import (
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// TestPeekRendersScreenNotTranscript verifies that a full-screen TUI's
// repaint stream shows only the current frame, not every frame stacked
// as lines (issue #147).
func TestPeekRendersScreenNotTranscript(t *testing.T) {
	m := baseModel(120, 45)
	m.profiles = nil
	m.harnesses = []protocol.HarnessInfo{
		{Name: "altscreen", State: "running"},
	}

	// Simulate a full-screen guest: alt-screen enter, then 5 repaints.
	var buf strings.Builder
	buf.WriteString("\x1b[?1049h") // enter alt screen
	for i := 1; i <= 5; i++ {
		buf.WriteString("\x1b[H\x1b[2J") // home + clear
		buf.WriteString("FULLSCREEN TUI frame " + intStrSimple(i) + "\n")
		buf.WriteString("Positioned text\n")
	}
	m.peek = logsMsg{name: "altscreen", text: buf.String()}
	m.sel = 0

	view := m.viewPeek(60, 30)

	// Must contain the latest frame.
	if !strings.Contains(view, "frame 5") {
		t.Error("peek should contain 'frame 5' (latest frame)")
	}
	// Must NOT contain earlier frames (they were overwritten by cursor addressing).
	for _, n := range []string{"frame 1", "frame 2", "frame 3", "frame 4"} {
		if strings.Contains(view, n) {
			t.Errorf("peek should NOT contain %q (overwritten by later repaint)", n)
		}
	}
}

// TestPeekLineOrientedGuest verifies that a plain line-oriented stream still
// renders as a scrolling tail (regression guard for #147).
func TestPeekLineOrientedGuest(t *testing.T) {
	m := baseModel(120, 45)
	m.profiles = nil
	m.harnesses = []protocol.HarnessInfo{
		{Name: "logger", State: "running"},
	}

	var lines []string
	for i := 0; i < 50; i++ {
		lines = append(lines, "log line "+intStrSimple(i))
	}
	m.peek = logsMsg{name: "logger", text: strings.Join(lines, "\n")}
	m.sel = 0

	view := m.viewPeek(60, 30)

	// Should contain recent lines.
	if !strings.Contains(view, "log line 49") {
		t.Error("peek should contain the latest log line")
	}
}

func intStrSimple(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return string(rune('0'+n/10)) + string(rune('0'+n%10))
}

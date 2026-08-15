package tui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// TestPeekReplaysAtGuestViewport is the dashboard's half of the "not 100%x100%"
// bug. Hopping into a harness resizes its PTY to the whole window and leaves it
// there on detach (smallest-attached-wins retains the last size, ADR-0003), so
// the peek tail that comes back is drawn for a window-wide guest. Replayed
// through an emulator sized to the *pane*, every one of those lines wraps and
// the screen reconstructs smooshed — the dashboard looking correct before the
// hop and mangled after it. Replayed at the guest's own viewport it
// reconstructs faithfully and the pane crops what doesn't fit.
func TestPeekReplaysAtGuestViewport(t *testing.T) {
	m := baseModel(160, 45)
	m.profiles = nil
	m.harnesses = []protocol.HarnessInfo{{Name: "wide", State: "running"}}
	m.sel = 0

	// A guest drawn at 120 columns: one row that runs past the pane's width,
	// then a second row beneath it.
	const guestCols, guestRows = 120, 30
	row1 := strings.Repeat("L", 100) + "RIGHT-EDGE"
	m.peek = logsMsg{
		name: "wide",
		text: "\x1b[H\x1b[2J" + row1 + "\r\nSECOND-ROW",
		cols: guestCols,
		rows: guestRows,
	}

	view := m.viewPeek(60, 30)

	if !strings.Contains(view, "SECOND-ROW") {
		t.Error("peek should contain the guest's second row")
	}
	// Column 101 of a 120-column guest is off the right edge of a 60-column
	// pane: cropped away, not wrapped down onto the next row.
	if strings.Contains(view, "RIGHT-EDGE") {
		t.Error("content past the pane's right edge leaked in — the tail wrapped instead of cropping")
	}
	// The wrapped-replay failure also pushes the guest's rows down; the second
	// row must still land directly under the first.
	lines := strings.Split(view, "\n")
	first, second := -1, -1
	for i, ln := range lines {
		if first < 0 && strings.Contains(ln, "LLLL") {
			first = i
		}
		if strings.Contains(ln, "SECOND-ROW") {
			second = i
		}
	}
	if first < 0 || second != first+1 {
		t.Errorf("guest rows landed at %d and %d, want adjacent (wrapping inserted a row)", first, second)
	}
	// Cropping is stated rather than silent, so a cut-off preview reads as a
	// narrow pane instead of a broken guest.
	if !strings.Contains(view, "120×30 cropped") {
		t.Error("head should name the guest viewport when the pane crops it")
	}
}

// TestPeekFallsBackToPaneViewport covers a daemon that reports no viewport (an
// older build, or a harness with no Mux): the peek keeps its historical
// pane-sized replay rather than rendering nothing.
func TestPeekFallsBackToPaneViewport(t *testing.T) {
	m := baseModel(160, 45)
	m.profiles = nil
	m.harnesses = []protocol.HarnessInfo{{Name: "wide", State: "running"}}
	m.sel = 0
	m.peek = logsMsg{name: "wide", text: "plain tail line\n"}

	view := m.viewPeek(60, 30)
	if !strings.Contains(view, "plain tail line") {
		t.Error("peek with no reported viewport should still render the tail")
	}
	if strings.Contains(view, "cropped") {
		t.Error("a pane-sized replay is not cropped; the head should not say so")
	}
}

// TestPeekGuestViewportStaysInsidePane guards the pane budget: a guest bigger
// than the pane on both axes must not widen or lengthen it. This is the same
// invariant #179/#180 pinned for the dashboard frame, re-checked with an
// oversized emulator behind the peek.
func TestPeekGuestViewportStaysInsidePane(t *testing.T) {
	m := baseModel(160, 45)
	m.profiles = nil
	m.harnesses = []protocol.HarnessInfo{{Name: "wide", State: "running"}}
	m.sel = 0

	var b strings.Builder
	b.WriteString("\x1b[H\x1b[2J")
	for i := 0; i < 40; i++ {
		b.WriteString(strings.Repeat("W", 200) + "\r\n")
	}
	m.peek = logsMsg{name: "wide", text: b.String(), cols: 200, rows: 40}

	const paneW, paneH = 60, 30
	lines := strings.Split(m.viewPeek(paneW, paneH), "\n")
	if len(lines) != paneH+paneBorderRows {
		t.Fatalf("peek rendered %d lines, want %d", len(lines), paneH+paneBorderRows)
	}
	for i, ln := range lines {
		if w := lipgloss.Width(ln); w != paneW {
			t.Errorf("line %d width = %d, want %d", i, w, paneW)
		}
	}
}

package tui

// Governing: SPEC-0001 (layout constants + small shared helpers for the
// cockpit). Kept in one place so the sizing math is consistent across the
// dashboard and attached views.

import "strings"

const (
	// peekLines is how many trailing log lines the peek pane tails.
	peekLines = 200
	// sessionBase is the first attach session id; the hop increments it so a
	// stale ATTACH_DATA frame from a just-closed session is ignored by id.
	sessionBase uint32 = 1
	// ribbonRows / headerRows / footerRows are the fixed chrome heights used to
	// size the embedded terminal viewport.
	headerRows = 3
	footerRows = 2
	ribbonRows = 1
)

// attachViewport returns the cols/rows available to the embedded terminal after
// subtracting the ribbon chrome.
func (m *Model) attachViewport() (int, int) {
	cols := m.w
	if cols < 1 {
		cols = 80
	}
	rows := m.h - ribbonRows
	if rows < 1 {
		rows = 24
	}
	return cols, rows
}

// bodyHeight is the dashboard body height between header and footer.
func (m *Model) bodyHeight() int {
	h := m.h - headerRows - footerRows
	if m.banner != "" {
		h--
	}
	if m.status != "" {
		h--
	}
	if h < 1 {
		h = 1
	}
	return h
}

// joinNames renders a name list for status lines.
func joinNames(names []string) string { return strings.Join(names, ", ") }

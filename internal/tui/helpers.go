package tui

// Governing: SPEC-0001 (layout constants + small shared helpers for the
// cockpit). Kept in one place so the sizing math is consistent across the
// dashboard and attached views.

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

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

// spinnerActive reports whether any visible harness (or the currently-
// attached one) is in a transient state (starting / restarting / stopping).
// The spinner ticks while true so those rows animate; false lets it rest so
// we're not burning a ~120ms tick on a still screen.
func (m *Model) spinnerActive() bool {
	isTransient := func(s string) bool {
		switch core.State(s) {
		case core.StateStarting, core.StateRestarting, core.StateStopping:
			return true
		}
		return false
	}
	if m.att != nil {
		if h := m.harnessByName(m.att.name); h != nil && isTransient(h.State) {
			return true
		}
	}
	for _, h := range m.visible() {
		if isTransient(h.State) {
			return true
		}
	}
	return false
}

// maybeStartSpinner returns the spinner tick command when the spinner should
// be running (a transient harness just appeared) and nil otherwise. Called
// after every state change (refresh / event / lifecycle op) so the spinner
// spins up the moment a harness enters starting/restarting/stopping and
// winds down once it settles.
func (m *Model) maybeStartSpinner() tea.Cmd {
	if m.spinnerActive() {
		return m.spinner.Tick
	}
	return nil
}

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

package tui

// Governing: SPEC-0001 (layout constants + small shared helpers for the
// cockpit). Kept in one place so the sizing math is consistent across the
// dashboard and attached views.

import (
	"strings"

	tea "charm.land/bubbletea/v2"

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
// subtracting the ribbon chrome. When the window size is unknown it falls back
// to 80×24 — a display default for the LOCAL view only. attachReportSize is
// what must not fall back: reporting 80×24 to the daemon when the size is
// unknown is how one blind client clamps every other client's guest PTY
// (#183).
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

// attachReportSize returns the viewport to report to the daemon on attach:
// the real window size, or 0×0 when it is genuinely unknown (#183). The daemon
// already treats a non-positive session size as "does not participate in
// smallest-attached-wins", so a blind client follows the harness's current
// geometry instead of defining it for everyone. When a real size arrives later
// (WindowSizeMsg / probeSizeMsg), AttachResize brings the session into the
// policy — see the WindowSizeMsg handler.
func (m *Model) attachReportSize() (int, int) {
	if m.w < 1 || m.h-ribbonRows < 1 {
		return 0, 0
	}
	return m.w, m.h - ribbonRows
}

// scrollbackHeight is the number of scrollback content rows that fit in the
// attached viewport. It's one less than the terminal body (view.rows) because
// viewScrollback renders its own 1-line status footer, and viewAttached appends
// the global status bar below that — so the content must leave a row for each to
// keep the total at exactly m.h lines (no overflow-and-scroll). Clamped to ≥1.
func (m *Model) scrollbackHeight() int {
	if m.att == nil {
		return 1
	}
	h := m.att.view.rows - 1
	if h < 1 {
		h = 1
	}
	return h
}

// bodyHeight is the dashboard body height between header and footer.
func (m *Model) bodyHeight() int {
	h := m.h - headerRows - footerRows
	if m.banner != "" {
		h--
	}
	if m.skewNotice != "" {
		h--
	}
	if m.status != "" {
		h--
	}
	// The search overlay renders an extra input line below the panes in place
	// of the status line (viewDashboard), so reserve a row for it or the
	// dashboard runs one line past the viewport and scrolls.
	if m.overlay == overlaySearch && m.status == "" {
		h--
	}
	if h < 1 {
		h = 1
	}
	return h
}

// joinNames renders a name list for status lines.
func joinNames(names []string) string { return strings.Join(names, ", ") }

package tui

// Governing: SPEC-0001 (the cockpit must not scroll — every screen renders in
// exactly m.h rows). ADR-0003 (the attached viewport's size is what the daemon
// resizes the real PTY to).
//
// The sizing math in helpers.go is the load-bearing arithmetic behind every
// "the frame tore apart" bug this package has had (issues #144, #148). It was
// covered only *transitively*: TestVisualJourneysFit renders composed screens
// and asserts they fit, which catches an over-deduction (visible as a short
// pane) but is blind to two things:
//
//   - An UNDER-deduction only shows up when some state combination actually
//     overflows. `journeys()` never builds banner+status+search together, so
//     that combination could scroll by exactly one row and every existing test
//     would still pass.
//   - Which deduction is wrong. A composed-view failure says "this screen is
//     one row too tall"; it does not say "bodyHeight forgot the search row".
//
// These tests assert each deduction independently, so a failure names the term.

import (
	"testing"
)

// TestBodyHeightDeductions pins every conditional subtraction in bodyHeight
// individually and in combination. The expected values are derived from the
// constants rather than hard-coded, so a deliberate change to the chrome height
// updates the expectation in one place instead of silently invalidating the
// test.
func TestBodyHeightDeductions(t *testing.T) {
	const h = 40
	base := h - headerRows - footerRows

	tests := []struct {
		name    string
		mutate  func(*Model)
		want    int
		because string
	}{
		{
			name:    "bare dashboard",
			mutate:  func(m *Model) {},
			want:    base,
			because: "only the fixed header/footer chrome comes off",
		},
		{
			name:    "banner takes a row",
			mutate:  func(m *Model) { m.banner = "reload failed" },
			want:    base - 1,
			because: "the banner renders as its own line above the panes",
		},
		{
			name:    "status takes a row",
			mutate:  func(m *Model) { m.status = "started reduit-agent" },
			want:    base - 1,
			because: "the status line renders below the panes",
		},
		{
			name:    "banner and status are independent",
			mutate:  func(m *Model) { m.banner = "b"; m.status = "s" },
			want:    base - 2,
			because: "both render, so both must be reserved",
		},
		{
			name:    "search overlay takes a row when there is no status",
			mutate:  func(m *Model) { m.overlay = overlaySearch },
			want:    base - 1,
			because: "the search input renders in the row the status line would have used",
		},
		{
			name:   "search overlay does NOT double-deduct when status is set",
			mutate: func(m *Model) { m.overlay = overlaySearch; m.status = "s" },
			want:   base - 1,
			because: "search replaces the status line rather than adding to it; " +
				"deducting twice would leave a blank row and shrink the panes",
		},
		{
			name:    "banner plus status plus search — the combination no journey builds",
			mutate:  func(m *Model) { m.banner = "b"; m.status = "s"; m.overlay = overlaySearch },
			want:    base - 2,
			because: "banner + (status-or-search, not both)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := baseModel(120, h)
			tt.mutate(m)
			if got := m.bodyHeight(); got != tt.want {
				t.Errorf("bodyHeight() = %d, want %d — %s", got, tt.want, tt.because)
			}
		})
	}
}

// TestBodyHeightClampsToOne pins the floor. A terminal shorter than the chrome
// yields a negative body; returning that would make callers slice with a
// negative length and panic, so it clamps to 1. Sizes below are deliberately
// absurd — this is the "someone dragged the window to nothing" path.
func TestBodyHeightClampsToOne(t *testing.T) {
	for _, h := range []int{0, 1, 2, 3, 4, 5} {
		m := baseModel(80, h)
		m.banner = "b"
		m.status = "s"
		if got := m.bodyHeight(); got < 1 {
			t.Errorf("bodyHeight() at h=%d = %d, want >= 1", h, got)
		}
	}
}

// TestAttachViewportSubtractsRibbon pins the attached sizing: the embedded
// terminal gets the full width and everything but the ribbon row. This is the
// size the client asks the daemon to resize the real PTY to (ADR-0003), so an
// error here does not just look wrong locally — it reshapes the guest process's
// window.
func TestAttachViewportSubtractsRibbon(t *testing.T) {
	m := baseModel(120, 40)
	cols, rows := m.attachViewport()
	if cols != 120 {
		t.Errorf("cols = %d, want the full width 120", cols)
	}
	if rows != 40-ribbonRows {
		t.Errorf("rows = %d, want %d (h - ribbonRows)", rows, 40-ribbonRows)
	}
}

// TestAttachViewportFallsBackWhenUnsized covers the branch taken before the
// first WindowSizeMsg arrives, when m.w/m.h are still zero. Handing the daemon
// a 0x0 PTY is the failure this guard exists to prevent: a zero-size PTY makes
// a full-screen guest render into nothing and often wedges it.
func TestAttachViewportFallsBackWhenUnsized(t *testing.T) {
	m := baseModel(0, 0)
	cols, rows := m.attachViewport()
	if cols != 80 || rows != 24 {
		t.Errorf("unsized viewport = %dx%d, want the 80x24 fallback", cols, rows)
	}

	// Negative dimensions are the same class of hazard and take the same guard.
	m = baseModel(-5, -5)
	cols, rows = m.attachViewport()
	if cols < 1 || rows < 1 {
		t.Errorf("negative-size viewport = %dx%d, want positive dimensions", cols, rows)
	}
}

// TestScrollbackHeightLeavesRoomForBothFooters pins the -1 that the comment in
// helpers.go explains at length: viewScrollback draws its own status footer and
// viewAttached appends the global status bar below it. Getting this wrong by
// one is exactly the "attached view scrolls" failure that
// TestScrollbackDoesNotOverflow catches only after composition.
func TestScrollbackHeightLeavesRoomForBothFooters(t *testing.T) {
	m, _ := attachedFake(100, 30)
	if m.att == nil {
		t.Fatal("fixture did not attach")
	}
	want := m.att.view.rows - 1
	if got := m.scrollbackHeight(); got != want {
		t.Errorf("scrollbackHeight() = %d, want %d (view.rows - 1 for the scrollback footer)", got, want)
	}
}

// TestScrollbackHeightWithoutAttachIsSafe covers the nil-attach guard. The
// dashboard can ask for this height while nothing is attached (a resize racing
// a detach); returning 0 or panicking would take the whole TUI down.
func TestScrollbackHeightWithoutAttachIsSafe(t *testing.T) {
	m := baseModel(80, 24)
	m.att = nil
	if got := m.scrollbackHeight(); got < 1 {
		t.Errorf("scrollbackHeight() with no attach = %d, want >= 1", got)
	}
}

// TestPaneInnerNeverNegative pins the pane-width helper across the range that
// matters, including terminals narrower than the border chrome. A negative
// inner width propagates into lipgloss Width() and truncation math.
func TestPaneInnerNeverNegative(t *testing.T) {
	for _, w := range []int{0, 1, 2, 3, 4, 10, 40, 80, 200} {
		if got := paneInner(w); got < 0 {
			t.Errorf("paneInner(%d) = %d, want >= 0", w, got)
		}
	}
}

package tui

// Governing: stump.wtf/harness#179 + #180 — two symptoms of one broken
// invariant. The dashboard frame must render EXACTLY m.h lines at every window
// height: #179 overflowed (a hard 13-line pane floor under a 12-row window,
// scrolling the alt screen — the #144 failure mode), and #180 underfilled
// (a constant −2 gap whenever the harness list was too short to pad the panes
// out to their border budget, leaving two dead rows at the bottom of the
// window). Both come from the same header/footer reservation arithmetic:
// bodyHeight() over-reserves by 2 to pay for the pane borders, but the panes
// only ever reached content+2 when content filled the budget — lipgloss pads
// up to a Height but never truncates down through one, so the peek pane's
// fixed-length summary forced the floor and sparse list content surrendered
// the border budget instead of claiming it.

import (
	"strings"
	"testing"
)

// TestDashboardFrameIsExactlyWindowHeight is the pair's combined regression:
// every height from below the split-cockpit floor to a tall window, with
// sparse and abundant harness lists, must land on exactly m.h lines.
func TestDashboardFrameIsExactlyWindowHeight(t *testing.T) {
	for _, harnesses := range []int{3, 40, 200} {
		for _, h := range []int{4, 5, 6, 8, 10, 12, 15, 20, 24, 47, 50} {
			m := manyHarnessModel(160, h, harnesses)
			if got := len(strings.Split(m.content(), "\n")); got != h {
				t.Errorf("h=%d with %d harnesses rendered %d lines, want exactly %d", h, harnesses, got, h)
			}
		}
	}
}

// TestDashboardFrameWithBannerAndStatus pins the same invariant with the two
// optional chrome rows present — bodyHeight() pays for each, so neither may
// disturb the exact-height landing.
func TestDashboardFrameWithBannerAndStatus(t *testing.T) {
	for _, h := range []int{8, 12, 20, 24, 50} {
		m := manyHarnessModel(160, h, 3)
		m.banner = "config reloaded"
		m.status = "restarted probe"
		if got := len(strings.Split(m.content(), "\n")); got != h {
			t.Errorf("h=%d with banner+status rendered %d lines, want exactly %d", h, got, h)
		}
	}
}

// TestDashboardShortWindowDegradesBelowFloor: below the split cockpit's
// minimum (header 2 + bordered pane 3 + footer 1 = 6), the dashboard drops the
// panes rather than emitting rows the window cannot hold — and still lands on
// exactly m.h.
func TestDashboardShortWindowDegradesBelowFloor(t *testing.T) {
	for _, h := range []int{1, 2, 3, 4, 5} {
		m := manyHarnessModel(60, h, 3)
		view := m.content()
		if got := len(strings.Split(view, "\n")); got != h {
			t.Errorf("h=%d rendered %d lines, want exactly %d (below-floor degradation)", h, got, h)
		}
	}
}

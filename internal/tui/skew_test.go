package tui

// Governing: stump.wtf/harness#181 — the TUI must say something when the
// running daemon predates the binary that launched it. The notice is advisory
// only (never a gate), dismissable with esc, and recomputed on every refresh
// so a restarted daemon clears it by itself.

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"gitea.stump.rocks/stump.wtf/harness/internal/buildinfo"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// withBuildVersion swaps buildinfo.Version for the test and restores it.
func withBuildVersion(t *testing.T, v string) {
	t.Helper()
	orig := buildinfo.Version
	buildinfo.Version = v
	t.Cleanup(func() { buildinfo.Version = orig })
}

// TestSkewBannerShowsAndDismisses is the #181 regression: a daemon older than
// the client produces a banner naming both builds and the fix, esc dismisses
// it for the session, and it stays dismissed across refreshes.
func TestSkewBannerShowsAndDismisses(t *testing.T) {
	withBuildVersion(t, "v0.1.0-147-g775e92f")
	m := manyHarnessModel(120, 30, 3)
	m.daemon = protocol.DaemonInfo{Version: "v0.1.0-90-gb9addf9"}
	if !m.skewDismissed && m.skewNotice != "" {
		t.Fatal("setup: notice already set before any refresh")
	}

	mm, _ := m.onRefresh(refreshMsg{daemon: m.daemon})
	m = mm.(*Model)
	if !strings.Contains(m.skewNotice, "57 commits behind") {
		t.Fatalf("skewNotice = %q, want the 57-commit delta", m.skewNotice)
	}
	if got := m.content(); !strings.Contains(got, "restart the daemon") {
		t.Error("dashboard does not surface the skew banner")
	}

	// Esc dismisses it for the session — including across later refreshes.
	m2, _ := m.dispatchDashboardKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	m = m2.(*Model)
	if m.skewNotice != "" || !m.skewDismissed {
		t.Fatalf("esc did not dismiss the banner: notice=%q dismissed=%v", m.skewNotice, m.skewDismissed)
	}
	if got := m.content(); strings.Contains(got, "restart the daemon") {
		t.Error("banner still rendered after esc")
	}
	m3, _ := m.onRefresh(refreshMsg{daemon: m.daemon})
	m = m3.(*Model)
	if m.skewNotice != "" {
		t.Error("dismissed banner came back on the next refresh")
	}
}

// TestSkewBannerSilentWhenAligned pins the quiet cases: same build, and the
// dev-client-next-to-tagged-daemon workflow.
func TestSkewBannerSilentWhenAligned(t *testing.T) {
	withBuildVersion(t, "v0.1.0-147-g775e92f")
	m := manyHarnessModel(120, 30, 3)
	m.daemon = protocol.DaemonInfo{Version: "v0.1.0-147-g775e92f"}
	mm, _ := m.onRefresh(refreshMsg{daemon: m.daemon})
	if m := mm.(*Model); m.skewNotice != "" {
		t.Errorf("same build produced a banner: %q", m.skewNotice)
	}

	withBuildVersion(t, "dev")
	m2 := manyHarnessModel(120, 30, 3)
	m2.daemon = protocol.DaemonInfo{Version: "v0.1.0-147-g775e92f"}
	mm2, _ := m2.onRefresh(refreshMsg{daemon: m2.daemon})
	if m2 := mm2.(*Model); m2.skewNotice != "" {
		t.Errorf("dev client produced a banner: %q", m2.skewNotice)
	}
}

// TestSkewBannerKeepsFrameHeight: the banner takes a real row out of
// bodyHeight, so the frame still lands on exactly m.h with it showing.
func TestSkewBannerKeepsFrameHeight(t *testing.T) {
	withBuildVersion(t, "v0.1.0-147-g775e92f")
	for _, h := range []int{16, 24, 40} {
		m := manyHarnessModel(120, h, 3)
		m.daemon = protocol.DaemonInfo{Version: "v0.1.0-90-gb9addf9"}
		mm, _ := m.onRefresh(refreshMsg{daemon: m.daemon})
		m = mm.(*Model)
		if got := len(strings.Split(m.content(), "\n")); got != h {
			t.Errorf("h=%d with skew banner rendered %d lines, want exactly %d", h, got, h)
		}
	}
}

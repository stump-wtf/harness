package tui

// Governing: stump.wtf/harness#183 (third defect) — a client that cannot detect
// its terminal size used to report the 80×24 fallback to the daemon, and
// smallest-attached-wins then let that blind client define geometry for every
// other attached client. The client half of the fix: attach with 0×0 (unknown)
// when the size is genuinely unknown, and join the resize policy once a real
// size arrives. The daemon half — a non-positive session size never
// participates in the minimum — is pinned in internal/attach.

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestAttachReportsUnknownWhenSizeUnknown: attaching with no detected window
// size must open the session at 0×0 — NOT the 80×24 display fallback — so the
// daemon excludes this session from smallest-attached-wins instead of clamping
// every other client at 80 columns. The local view still renders at the 80×24
// display default until a real size arrives.
func TestAttachReportsUnknownWhenSizeUnknown(t *testing.T) {
	m, fa := attachedFake(0, 0)
	m.att = nil // not yet attached; attachTo opens the session

	if cmd := m.attachTo(m.harnesses[0], 0); cmd != nil {
		drain(cmd)
	}
	if len(fa.openSizes) != 1 {
		t.Fatalf("AttachOpen called %d times, want 1", len(fa.openSizes))
	}
	got := fa.openSizes[0]
	if got.cols != 0 || got.rows != 0 {
		t.Errorf("opened at %dx%d, want 0x0 (unknown) — the fallback would clamp everyone else", got.cols, got.rows)
	}
	// The local emulator still has something sane to render into.
	if m.att.view.cols != 80 || m.att.view.rows != 24 {
		t.Errorf("local view = %dx%d, want the 80x24 display default", m.att.view.cols, m.att.view.rows)
	}

	// Recovery: a real size arriving later joins the policy via AttachResize,
	// so the once-blind session starts participating at its true viewport.
	m.Update(tea.WindowSizeMsg{Width: 200, Height: 50})
	if len(fa.resizes) != 1 {
		t.Fatalf("AttachResize called %d times after the size arrived, want 1", len(fa.resizes))
	}
	r := fa.resizes[0]
	if r.cols != 200 || r.rows != 50-ribbonRows {
		t.Errorf("resize sent %dx%d, want %dx%d", r.cols, r.rows, 200, 50-ribbonRows)
	}
}

// TestAttachReportsRealSizeWhenKnown pins the companion: a client that does
// know its size still reports it — unknown-when-blind must not degrade the
// normal path.
func TestAttachReportsRealSizeWhenKnown(t *testing.T) {
	for _, dim := range [][2]int{{200, 50}, {80, 24}} {
		w, h := dim[0], dim[1]
		m, fa := attachedFake(w, h)
		m.att = nil
		if cmd := m.attachTo(m.harnesses[0], 0); cmd != nil {
			drain(cmd)
		}
		got := fa.openSizes[0]
		if got.cols != w || got.rows != h-ribbonRows {
			t.Errorf("%dx%d: reported %dx%d, want %dx%d", w, h, got.cols, got.rows, w, h-ribbonRows)
		}
	}
}

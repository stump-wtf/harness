package tui

// Governing: stump.wtf/harness#239 — the confirm guard must be a centered
// modal composited ON TOP of the surface it interrupts, not a replacement for
// it. content() used to return the confirm box directly, so the dashboard
// vanished and a lone box sat at the top-left of an empty screen; the fix
// composites base + dialog via Lip Gloss v2 Canvas/Layer, so the harness the
// operator is about to stop stays visible behind the dialog.

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
)

// confirmModel drives a dashboard into the stop-confirm state the way the x
// key does (keyhandlers.go), at a given window size.
func confirmModel(w, h int) *Model {
	m := baseModel(w, h)
	m.sel = 0 // crush-signal, running
	model, _ := m.onKey(runeKey("x"))
	return model.(*Model)
}

// TestConfirmOverlaysDashboard: rendering with the confirm guard open must
// still show the dashboard it interrupts — header, the selected harness's
// name, and the footer key bar — alongside the confirm dialog's prompt.
func TestConfirmOverlaysDashboard(t *testing.T) {
	m := confirmModel(120, 40)
	if m.overlay != overlayConfirm {
		t.Fatalf("overlay = %v, want overlayConfirm", m.overlay)
	}
	view := m.content()
	for _, want := range []string{"harness", "crush-signal", "confirm"} {
		if !strings.Contains(strings.ToLower(view), strings.ToLower(want)) {
			t.Errorf("confirm view lost %q — the base surface must stay visible behind the modal", want)
		}
	}
}

// TestConfirmFrameIsExactlyWindowHeight: the composited frame must keep the
// exact-height invariant (#179/#180) — exactly m.h lines, none wider than m.w
// — at every size, same as the dashboard underneath.
func TestConfirmFrameIsExactlyWindowHeight(t *testing.T) {
	for _, s := range [][2]int{{200, 50}, {120, 40}, {80, 24}, {80, 12}, {60, 8}, {60, 6}} {
		w, h := s[0], s[1]
		view := confirmModel(w, h).content()
		lines := strings.Split(view, "\n")
		if len(lines) != h {
			t.Errorf("%dx%d: confirm overlay rendered %d lines, want exactly %d", w, h, len(lines), h)
		}
		for i, ln := range lines {
			if lw := lipgloss.Width(ln); lw > w {
				t.Errorf("%dx%d: row %d width %d exceeds terminal width %d", w, h, i, lw, w)
			}
		}
	}
}

// TestConfirmDialogIsCentered: the modal's box must sit inside the middle of
// the window — its title row strictly between the top and bottom edges, the
// title horizontally inset from column 0 — not pinned to the top-left the way
// the pre-fix whole-screen replacement was.
func TestConfirmDialogIsCentered(t *testing.T) {
	w, h := 120, 40
	lines := strings.Split(confirmModel(w, h).content(), "\n")

	titleRow, titleCol := -1, -1
	for i, ln := range lines {
		if col := strings.Index(ln, "Confirm"); col > 0 {
			titleRow, titleCol = i, col
			break
		}
	}
	if titleRow < 0 {
		t.Fatal("confirm dialog title not found in the composited view")
	}
	if titleRow == 0 || titleRow >= h-1 {
		t.Errorf("dialog title on row %d of %d — should be vertically centered", titleRow, h)
	}
	if titleCol < 2 {
		t.Errorf("dialog title at column %d — should be horizontally inset, not flush left", titleCol)
	}
}

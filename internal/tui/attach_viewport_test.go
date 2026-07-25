package tui

// Governing: SPEC-0001 REQ "Attached Mode" (the embedded terminal is the whole
// window bar the status line) and ADR-0003 (the daemon sizes the harness's real
// PTY from the client's reported viewport). These cover the client half of "a
// harness fills the window and follows it when the window changes": what the
// client *asks the daemon for*. The daemon half — that ask reaching a real
// child process's PTY — is covered end-to-end in internal/attach.

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// attachedFake builds an attached model wired to a recording fakeAttach.
func attachedFake(w, h int) (*Model, *fakeAttach) {
	fa := &fakeAttach{}
	fc := &fakeController{harnesses: sampleHarnesses(), profiles: sampleProfiles()}
	m := New(Options{})
	m.ctrl, m.attach = fc, fa
	m.conn = startOK
	m.harnesses = fc.harnesses
	m.profiles = fc.profiles
	m.w, m.h = w, h
	m.help.Width = w
	m.mode = modeAttached
	cols, rows := m.attachViewport()
	m.att = newAttachState(m.harnesses[0].Name, protocol.AttachRW, sessionBase, cols, rows)
	return m, fa
}

// TestAttachOpensAtFullWindow: the session is opened at the whole window minus
// the one-line status bar. Opening at anything smaller (e.g. the 80×24 fallback,
// which is what happens when the attach is issued before the window size is
// known) is what leaves the harness rendering into a box in the corner.
func TestAttachOpensAtFullWindow(t *testing.T) {
	for _, dim := range [][2]int{{200, 50}, {176, 25}, {120, 40}, {80, 24}} {
		w, h := dim[0], dim[1]
		m, fa := attachedFake(w, h)
		m.att = nil // not yet attached; attachTo opens the session

		if cmd := m.attachTo(m.harnesses[0], 0); cmd != nil {
			drain(cmd)
		}
		if len(fa.openSizes) != 1 {
			t.Fatalf("%dx%d: AttachOpen called %d times, want 1", w, h, len(fa.openSizes))
		}
		got := fa.openSizes[0]
		if got.cols != w || got.rows != h-ribbonRows {
			t.Errorf("%dx%d: opened at %dx%d, want %dx%d (full window less the status bar)",
				w, h, got.cols, got.rows, w, h-ribbonRows)
		}
	}
}

// TestWindowResizePropagatesToDaemon: every window change while attached must
// reach the daemon, which resizes the harness's PTY to match. Without this the
// app inside keeps its old layout while the window around it changes.
func TestWindowResizePropagatesToDaemon(t *testing.T) {
	m, fa := attachedFake(120, 40)

	steps := [][2]int{{200, 50}, {90, 20}, {176, 25}}
	for _, s := range steps {
		w, h := s[0], s[1]
		m.Update(tea.WindowSizeMsg{Width: w, Height: h})
	}
	if len(fa.resizes) != len(steps) {
		t.Fatalf("AttachResize called %d times, want %d (one per window change)", len(fa.resizes), len(steps))
	}
	for i, s := range steps {
		w, h := s[0], s[1]
		got := fa.resizes[i]
		if got.cols != w || got.rows != h-ribbonRows {
			t.Errorf("resize %d: sent %dx%d, want %dx%d", i, got.cols, got.rows, w, h-ribbonRows)
		}
		if got.sid != m.att.sessionID {
			t.Errorf("resize %d: session id %d, want %d", i, got.sid, m.att.sessionID)
		}
	}
	// The client's own emulator must track the same size, or the daemon paints
	// into a grid the client renders at a different shape.
	if m.att.view.cols != 176 || m.att.view.rows != 25-ribbonRows {
		t.Errorf("client emulator = %dx%d, want %dx%d",
			m.att.view.cols, m.att.view.rows, 176, 25-ribbonRows)
	}
}

// TestHopReopensAtFullWindow: hopping to another harness opens the new session
// at the current window size too — a hop must not reintroduce the 80×24 box.
func TestHopReopensAtFullWindow(t *testing.T) {
	m, fa := attachedFake(176, 25)
	if cmd := m.hopTo(1); cmd != nil {
		drain(cmd)
	}
	if len(fa.openSizes) == 0 {
		t.Fatal("hop opened no session")
	}
	got := fa.openSizes[len(fa.openSizes)-1]
	if got.cols != 176 || got.rows != 25-ribbonRows {
		t.Errorf("hop opened at %dx%d, want %dx%d", got.cols, got.rows, 176, 25-ribbonRows)
	}
}

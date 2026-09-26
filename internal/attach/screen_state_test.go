package attach

// Governing: issue #735; ADR-0040 (screen observability). ScreenState is what
// makes a stuck-at-prompt harness observable without an interactive TTY: the
// plain-text screen the emulator projects, plus the honest "no output change"
// age — PTY bytes received, not screen bytes compared.

import (
	"strings"
	"testing"
	"time"
)

// TestScreenStateReportsScreenAndIdle pins the two halves of the verdict a
// sweep renders: WHAT is on the glass (a full-screen prompt's text, which the
// durable log deliberately never receives, ADR-0007) and HOW LONG the guest
// has been silent — measured from the last PTY byte, so an in-place repaint
// that rewrote identical cells still counts as activity.
func TestScreenStateReportsScreenAndIdle(t *testing.T) {
	m := newMux("h", 100, nil, nil, nil)

	// An unfed mux is "no screen yet": the Manager tees a mux for every
	// configured harness at registration, so Fed — not existence — is what
	// says whether there is anything to observe.
	if st := m.ScreenState(time.Now()); st.Fed || len(st.Rows) != 0 {
		t.Fatalf("a never-fed mux must report unfed and blank, got fed=%v rows=%q", st.Fed, st.Rows)
	}

	m.Write([]byte("regular output\r\n"))
	m.Write([]byte("\x1b[2J\x1b[Hstuck: Do you want to proceed? (y/n)"))
	// A later repaint that writes identical cells: activity, not idleness.
	repaintAt := time.Now().Add(-2 * time.Second)
	m.mu.Lock()
	m.term.Write([]byte("\x1b[H"))
	m.term.Write([]byte("stuck: Do you want to proceed? (y/n)"))
	m.lastWrite = repaintAt
	m.mu.Unlock()

	st := m.ScreenState(repaintAt.Add(90 * time.Second))
	joined := strings.Join(st.Rows, "\n")
	if !strings.Contains(joined, "Do you want to proceed? (y/n)") {
		t.Fatalf("screen text lost the prompt: %q", joined)
	}
	if strings.ContainsAny(joined, "\x1b") {
		t.Fatalf("plain-text screen leaked escape bytes: %q", joined)
	}
	// Trailing blank rows are trimmed from a capture view; interior geometry
	// is not. The prompt sits on row 0 after the clear-and-home, and the rows
	// below it are padding.
	if st.Rows[0] != "stuck: Do you want to proceed? (y/n)" {
		t.Fatalf("first row = %q, want the prompt row", st.Rows[0])
	}
	if st.Idle != 90*time.Second {
		t.Fatalf("idle = %v, want 90s from the last byte (repaint counts as activity)", st.Idle)
	}
}

// TestScreenStateIdleIsLastByteNotLastChange guards the definition of "no
// output change": a busy TUI that repaints every second shows Idle ~0 even
// though the screen text never differs, so waiting detection cannot fire on a
// healthy harness.
func TestScreenStateIdleIsLastByteNotLastChange(t *testing.T) {
	m := newMux("h", 100, nil, nil, nil)
	m.Write([]byte("frame 1"))
	now := time.Now()
	m.mu.Lock()
	m.lastWrite = now.Add(-500 * time.Millisecond)
	m.mu.Unlock()
	if got := m.ScreenState(now).Idle; got < 400*time.Millisecond {
		t.Fatalf("idle = %v, want ~500ms after a recent repaint", got)
	}
}

// TestRegistryScreenForNeverCreates pins the SnapshotFor discipline: reading a
// screen for a harness name the registry has never seen must not materialize a
// Mux (and its goroutine) for it.
func TestRegistryScreenForNeverCreates(t *testing.T) {
	r := NewRegistry(100)
	if st, ok := r.ScreenFor("ghost", time.Now()); ok || st.Fed {
		t.Fatal("ScreenFor reported a screen for a harness with no mux")
	}
	r.Mux("real") // creates
	if st, ok := r.ScreenFor("real", time.Now()); !ok || st.Fed {
		t.Fatalf("ScreenFor missed an existing mux (ok=%v fed=%v)", ok, st.Fed)
	}
	if _, ok := r.ScreenFor("ghost", time.Now()); ok {
		t.Fatal("ScreenFor materialized a mux for the ghost")
	}
	if _, exists := r.muxes["ghost"]; exists {
		t.Fatal("a ghost harness got a mux out of a screen read")
	}
}

// TestAnsiScreenRepaints pins that the ANSI view is a self-contained repaint
// (clear + home) a client terminal renders faithfully — the same contract as
// an attach snapshot's first frame.
func TestAnsiScreenRepaints(t *testing.T) {
	m := newMux("h", 100, nil, nil, nil)
	m.Write([]byte("hello\r\n"))
	out := string(m.AnsiScreen())
	if !strings.HasPrefix(out, "\x1b[0m\x1b[2J\x1b[H") {
		t.Fatalf("ansi screen must open with reset+clear+home, got %q", out[:20])
	}
	if !strings.Contains(out, "hello") {
		t.Fatalf("ansi screen lost content: %q", out)
	}
}


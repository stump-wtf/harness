package attach

// Governing: ADR-0055 (mouse ownership follows the guest's mouse-reporting
// modes). A client attaches to a screen snapshot, and cells, cursor and SGR
// survive that repaint while a DECSET does not, so a guest that enabled the
// mouse once at startup, long before anyone attached, would be invisible to the
// client, which would then keep the wheel for its own scrollback while the guest
// sat waiting for events (issue #954, upstream stump-wtf/harness#29). The mux
// shadows those modes and states them in every snapshot.

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/protocol"
)

// snapshotOf attaches one read-write client and returns the snapshot frame it
// is handed first. Polling for the first frame is enough: the pump writes in
// queue order, and the snapshot is what Attach queued ahead of everything else.
// The assertions are about what that frame states, because the ring's tail replay
// legitimately carries the guest's original DECSET bytes, so "some frame
// mentions it" would pin nothing.
func snapshotOf(t *testing.T, m *Mux) []byte {
	t.Helper()
	c := &collector{}
	s := m.Attach(1, protocol.AttachRW, 80, 24, c.write)
	t.Cleanup(func() { m.Detach(s) })
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if frames := c.all(); len(frames) > 0 {
			return frames[0]
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("no snapshot frame reached the client")
	return nil
}

// TestSnapshotCarriesGuestMouseModes pins the half of the fix the client cannot
// do alone: a guest that enabled the mouse before the attach still owns the
// wheel afterwards.
func TestSnapshotCarriesGuestMouseModes(t *testing.T) {
	m := newMux("h", 100, nil, nil, nil)
	if _, err := m.Write([]byte("claude\x1b[?1000h\x1b[?1002h\x1b[?1006h")); err != nil {
		t.Fatalf("write: %v", err)
	}

	snap := snapshotOf(t, m)
	for _, want := range []string{"\x1b[?1000h", "\x1b[?1002h", "\x1b[?1006h"} {
		if !bytes.Contains(snap, []byte(want)) {
			t.Errorf("snapshot does not enable %q; a late client would keep the wheel", want)
		}
	}
}

// TestSnapshotResetsStaleMouseModes is the other half of the same guarantee: a
// snapshot must be authoritative, so a mode the guest has since turned off is
// stated OFF rather than merely absent, otherwise a client that learned "on"
// from an earlier frame stays deaf to the guest forever.
func TestSnapshotResetsStaleMouseModes(t *testing.T) {
	m := newMux("h", 100, nil, nil, nil)
	if _, err := m.Write([]byte("\x1b[?1000h\x1b[?1003h\x1b[?1000l\x1b[?1003l")); err != nil {
		t.Fatalf("write: %v", err)
	}

	snap := snapshotOf(t, m)
	for _, want := range []string{"\x1b[?1000l", "\x1b[?1003l"} {
		if !bytes.Contains(snap, []byte(want)) {
			t.Errorf("snapshot does not reset %q", want)
		}
	}
	if bytes.Contains(snap, []byte("\x1b[?1000h")) {
		t.Error("snapshot re-enables a mode the guest turned off")
	}
}

// TestSnapshotLeavesUntrackedModesAlone keeps the snapshot from becoming a mode
// dump: bracketed paste already has its own daemon-side handling (Input
// unwraps a paste for a guest that never asked for it), so repeating ?2004 in
// the snapshot would only give a client state it has no use for.
func TestSnapshotLeavesUntrackedModesAlone(t *testing.T) {
	m := newMux("h", 100, nil, nil, nil)
	if _, err := m.Write([]byte("\x1b[?2004h")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if snap := snapshotOf(t, m); strings.Contains(string(snap), "?2004") {
		t.Error("snapshot carries ?2004; the daemon answers that one itself")
	}
}

// TestCoalescedRepaintCarriesGuestMouseModes pins that the coalesced repaint a
// slow client is handed instead of its backlog carries the modes too: that
// client has by definition missed bytes, which is exactly when a stale shadow
// bites.
func TestCoalescedRepaintCarriesGuestMouseModes(t *testing.T) {
	m := newMux("h", 100, nil, nil, nil)
	if _, err := m.Write([]byte("\x1b[?1006h")); err != nil {
		t.Fatalf("write: %v", err)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	repaint := m.renderSnapshotLocked()
	if !bytes.Contains(repaint, []byte("\x1b[?1006h")) {
		t.Error("coalesced repaint does not state the guest's mouse encoding")
	}
}

// TestAnsiScreenStaysModeFree: those bytes are written to a real terminal by
// capture (cmd/harness verbs.go), where a mouse DECSET would change what the
// operator's own terminal reports.
func TestAnsiScreenStaysModeFree(t *testing.T) {
	m := newMux("h", 100, nil, nil, nil)
	if _, err := m.Write([]byte("\x1b[?1000h\x1b[?1006h")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if screen := string(m.AnsiScreen()); strings.Contains(screen, "?1000") || strings.Contains(screen, "?1006") {
		t.Error("AnsiScreen carries mouse modes; capture writes it straight to a terminal")
	}
}

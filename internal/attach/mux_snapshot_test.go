package attach

// Governing: stump.wtf/harness#183 — a stale attach client clamps every other
// client's guest PTY via smallest-attached-wins, and there was no way to see
// it. Snapshot is the visibility half: the authoritative viewport plus every
// live session, with the minimum-setter flagged so the clamping session is
// identifiable from `describe` instead of lsof on the daemon and stty on the
// guest's tty.

import (
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// discard is a session write func that swallows the stream.
func discard([]byte) error { return nil }

// TestMuxSnapshotFlagsMinimumSetter pins the flag that answers "which session
// is clamping the guest?": the session(s) whose viewport defines the current
// minimum on at least one axis, recomputed exactly as applyResizeLocked does.
func TestMuxSnapshotFlagsMinimumSetter(t *testing.T) {
	m := newMux("h", 100, nil, nil)
	big := m.Attach(1, protocol.AttachRW, 200, 50, discard)
	small := m.Attach(2, protocol.AttachRW, 80, 24, discard)
	ro := m.Attach(3, protocol.AttachRO, 120, 40, discard)

	snap := m.Snapshot()
	if snap.Cols != 80 || snap.Rows != 24 {
		t.Fatalf("viewport = %dx%d, want the 80x24 minimum", snap.Cols, snap.Rows)
	}
	if len(snap.Sessions) != 3 {
		t.Fatalf("sessions = %d, want 3", len(snap.Sessions))
	}
	for i, s := range snap.Sessions {
		if want := uint32(i + 1); s.ID != want {
			t.Errorf("sessions[%d].ID = %d, want %d (sorted by id)", i, s.ID, want)
		}
		if s.CreatedAt.IsZero() {
			t.Errorf("session %d has no createdAt — age is half the diagnosis", s.ID)
		}
	}
	if !snap.Sessions[1].SetsMin {
		t.Error("the 80x24 session must be flagged as setting the minimum")
	}
	if snap.Sessions[0].SetsMin || snap.Sessions[2].SetsMin {
		t.Error("a session larger than the minimum on both axes must not be flagged")
	}
	if snap.Sessions[2].Mode != protocol.AttachRO {
		t.Errorf("mode = %s, want ro (mode is part of the triage picture)", snap.Sessions[2].Mode)
	}

	// The stale client grows up: the minimum moves to the read-only session.
	small.Resize(300, 100)
	snap = m.Snapshot()
	if snap.Cols != 120 || snap.Rows != 40 {
		t.Fatalf("after resize viewport = %dx%d, want 120x40", snap.Cols, snap.Rows)
	}
	if !snap.Sessions[2].SetsMin || snap.Sessions[1].SetsMin {
		t.Error("the minimum-setter flag did not follow the resize")
	}

	// The min-setting session detaches: it disappears and the flag moves on.
	ro.Detach()
	snap = m.Snapshot()
	if len(snap.Sessions) != 2 {
		t.Fatalf("sessions after detach = %d, want 2", len(snap.Sessions))
	}
	if !snap.Sessions[0].SetsMin {
		t.Error("after the clamp detaches, the next-smallest session (200x50) is the minimum-setter")
	}

	big.Detach()
	small.Detach()
}

// TestMuxSnapshotNoSessionsIsHonest: with every session gone the mux retains
// its last size by design, and the snapshot reports that size with an empty
// session list — the describe output then says "clamped, but by nobody live",
// which is exactly the retained-intent case #183 documents.
func TestMuxSnapshotNoSessionsIsHonest(t *testing.T) {
	m := newMux("h", 100, nil, nil)
	s := m.Attach(7, protocol.AttachRW, 100, 30, discard)
	s.Detach()

	snap := m.Snapshot()
	if snap.Cols != 100 || snap.Rows != 30 {
		t.Fatalf("retained viewport = %dx%d, want 100x30", snap.Cols, snap.Rows)
	}
	if len(snap.Sessions) != 0 {
		t.Fatalf("sessions = %d, want 0", len(snap.Sessions))
	}
}

// TestRegistrySnapshotForDoesNotCreate: describing a harness nobody attached to
// must not materialize a Mux for it (Registry.Mux would).
func TestRegistrySnapshotForDoesNotCreate(t *testing.T) {
	r := NewRegistry(100)
	if _, ok := r.SnapshotFor("never-attached"); ok {
		t.Fatal("SnapshotFor reported a Mux for a harness that was never attached")
	}
	m := r.Mux("live")
	m.Attach(1, protocol.AttachRW, 80, 24, discard)
	snap, ok := r.SnapshotFor("live")
	if !ok {
		t.Fatal("SnapshotFor missed an existing Mux")
	}
	if len(snap.Sessions) != 1 || !snap.Sessions[0].SetsMin {
		t.Fatalf("snapshot = %+v, want the single session flagged as minimum-setter", snap)
	}
	if _, ok := r.SnapshotFor("never-attached"); ok {
		t.Fatal("looking up one harness created a Mux for another")
	}
	if snap.Sessions[0].CreatedAt.After(time.Now()) {
		t.Error("createdAt is in the future")
	}
}

// TestUnknownSizeSessionDoesNotClamp pins the daemon half of #183's third
// defect: a session that attached with 0×0 (client could not detect its size)
// never participates in smallest-attached-wins, so it cannot clamp the guest
// for the clients that do know their viewport.
func TestUnknownSizeSessionDoesNotClamp(t *testing.T) {
	m := newMux("h", 100, nil, nil)
	blind := m.Attach(1, protocol.AttachRW, 0, 0, discard)
	sighted := m.Attach(2, protocol.AttachRW, 200, 49, discard)

	if c, r := m.Size(); c != 200 || r != 49 {
		t.Fatalf("viewport = %dx%d, want 200x49 — the blind session must not clamp it", c, r)
	}
	snap := m.Snapshot()
	if snap.Sessions[0].SetsMin {
		t.Error("the 0x0 session must not be flagged as minimum-setter")
	}
	if !snap.Sessions[1].SetsMin {
		t.Error("the only sized session is the minimum-setter")
	}

	// The blind client learns its size: it joins the policy without evicting
	// anyone, and the minimum only changes if the new size is actually smaller.
	blind.Resize(120, 30)
	if c, r := m.Size(); c != 120 || r != 30 {
		t.Fatalf("viewport after the blind client learned 120x30 = %dx%d, want 120x30", c, r)
	}

	blind.Detach()
	sighted.Detach()
}

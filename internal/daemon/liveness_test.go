package daemon

// Governing: SPEC-0002 REQ "Backpressure Isolation" ("PING/PONG heartbeats
// SHALL detect dead clients so their sessions get reaped") and REQ "Attach
// Session"; ADR-0003 (smallest-attached-wins). These are the scenarios for
// stump.wtf/harness#183: an attach client that is alive but no longer reading
// its socket never fails the daemon's writes, so without a liveness reaper it
// holds its session forever and clamps the guest PTY for every other client.
//
// The heartbeat intervals are shrunk to milliseconds per daemon (Options.
// PingInterval / LivenessTimeout) rather than slept through at the production
// 15s/60s, following the deadline-poll style the rest of the daemon and attach
// tests use.

import (
	"sync"
	"syscall"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/attach"
	"gitea.stump.rocks/stump.wtf/harness/internal/client"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// reapFast tunes a test daemon so the reaper runs in milliseconds: a wedged
// client is reaped roughly one ping interval after the timeout elapses.
func reapFast(o *Options) {
	o.PingInterval = 25 * time.Millisecond
	o.LivenessTimeout = 75 * time.Millisecond
}

// reapPatient keeps the fast ping cadence but gives a client a wide silence
// budget — 20 unanswered PINGs — so the "healthy client is never evicted" test
// cannot fail merely because a goroutine was descheduled under -race.
func reapPatient(o *Options) {
	o.PingInterval = 25 * time.Millisecond
	o.LivenessTimeout = 500 * time.Millisecond
}

// recordingController wraps the real Controller and records every PTY resize
// the mux drives, so a test can assert that eviction actually raised the guest
// — not merely that the bookkeeping changed.
type recordingController struct {
	inner attach.Controller

	mu      sync.Mutex
	resizes [][2]int
}

func (r *recordingController) Resize(name string, cols, rows int) bool {
	r.mu.Lock()
	r.resizes = append(r.resizes, [2]int{cols, rows})
	r.mu.Unlock()
	return r.inner.Resize(name, cols, rows)
}

func (r *recordingController) WriteInput(name string, p []byte) bool {
	return r.inner.WriteInput(name, p)
}

func (r *recordingController) SignalGroup(name string, sig syscall.Signal) bool {
	return r.inner.SignalGroup(name, sig)
}

func (r *recordingController) snapshot() [][2]int {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([][2]int, len(r.resizes))
	copy(out, r.resizes)
	return out
}

// recordResizes swaps a recording Controller in front of the daemon's Manager.
// The muxes resolve the controller on every callback, so this works whether or
// not a Mux already exists.
func recordResizes(td *testDaemon) *recordingController {
	rec := &recordingController{inner: td.mgr}
	td.reg.SetController(rec)
	return rec
}

// wedge makes c answer exactly one PING and then stop reading forever — the
// #183 client: still connected, still absorbing the daemon's writes into its
// socket receive buffer, but no longer processing anything. The single PONG is
// what makes it eligible for reaping at all (see conn.stale).
func wedge(t *testing.T, c *client.Client) {
	t.Helper()
	if err := c.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	for {
		f, err := c.Conn().ReadFrame()
		if err != nil {
			t.Fatalf("waiting for the first PING: %v", err)
		}
		if f.Type != protocol.TypePing {
			continue
		}
		if err := c.Conn().WriteFrame(protocol.TypePong, nil); err != nil {
			t.Fatalf("write PONG: %v", err)
		}
		break
	}
	if err := c.SetReadDeadline(time.Time{}); err != nil {
		t.Fatalf("clear read deadline: %v", err)
	}
}

// pongForever drains c and answers every PING until the connection dies — a
// well-behaved client. The returned func closes the connection and waits for
// the reader to finish, so no goroutine outlives the test.
func pongForever(c *client.Client) (stop func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			f, err := c.Conn().ReadFrame()
			if err != nil {
				return
			}
			if f.Type == protocol.TypePing {
				if err := c.Conn().WriteFrame(protocol.TypePong, nil); err != nil {
					return
				}
			}
		}
	}()
	return func() {
		_ = c.Close()
		<-done
	}
}

// waitSessions polls the harness's attach snapshot until it holds want sessions.
func waitSessions(t *testing.T, td *testDaemon, name string, want int) attach.MuxSnapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var snap attach.MuxSnapshot
	for {
		snap, _ = td.reg.SnapshotFor(name)
		if len(snap.Sessions) == want {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatalf("attach sessions = %d (%+v), want %d within timeout", len(snap.Sessions), snap.Sessions, want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// startSleeper boots the sleeper harness through a throwaway control client.
func startSleeper(t *testing.T, td *testDaemon) {
	t.Helper()
	if _, err := td.dial(t, nil).Start("sleeper"); err != nil {
		t.Fatalf("start: %v", err)
	}
}

// TestWedgedAttachSessionIsEvicted is the core of #183: the only attached
// client stops reading, and the daemon reaps it instead of holding the session
// until a daemon restart. It also pins the flip side of ADR-0003 that must NOT
// change — with the last session gone the mux retains its last authoritative
// size and drives no shrink onto the guest.
func TestWedgedAttachSessionIsEvicted(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML, reapFast)
	rec := recordResizes(td)
	startSleeper(t, td)

	stuck := td.dial(t, nil)
	wedge(t, stuck)
	if err := stuck.AttachOpen(1, "sleeper", 90, 30, protocol.AttachRW); err != nil {
		t.Fatalf("attach: %v", err)
	}
	snap := waitSessions(t, td, "sleeper", 1)
	if snap.Cols != 90 || snap.Rows != 30 {
		t.Fatalf("viewport while attached = %dx%d, want 90x30", snap.Cols, snap.Rows)
	}

	// The client is alive and its socket still accepts the daemon's PINGs; only
	// its silence identifies it.
	snap = waitSessions(t, td, "sleeper", 0)

	// Retain-last-size is deliberate (applyResizeLocked: "with no sessions the
	// last size is retained") — the guest keeps its window rather than
	// collapsing when everyone leaves, evicted or not.
	if snap.Cols != 90 || snap.Rows != 30 {
		t.Errorf("viewport after eviction = %dx%d, want 90x30 retained", snap.Cols, snap.Rows)
	}
	for _, r := range rec.snapshot()[1:] {
		t.Errorf("eviction of the last session drove a resize to %v, want none after the attach", r)
	}
}

// TestEvictionRestoresSurvivingClientViewport is the user-visible bug: a
// healthy 200x50 client is clamped to 80x24 by a wedged one, and once the
// wedged session is reaped the guest PTY must actually be driven back up
// (ADR-0003 recomputed over the survivors) — not merely un-clamped in the
// bookkeeping.
func TestEvictionRestoresSurvivingClientViewport(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML, reapFast)
	rec := recordResizes(td)
	startSleeper(t, td)

	healthy := td.dial(t, nil)
	stopHealthy := pongForever(healthy)
	defer stopHealthy()
	if err := healthy.AttachOpen(1, "sleeper", 200, 50, protocol.AttachRW); err != nil {
		t.Fatalf("healthy attach: %v", err)
	}
	snap := waitSessions(t, td, "sleeper", 1)
	if snap.Cols != 200 || snap.Rows != 50 {
		t.Fatalf("viewport with only the healthy client = %dx%d, want 200x50", snap.Cols, snap.Rows)
	}

	stuck := td.dial(t, nil)
	wedge(t, stuck)
	if err := stuck.AttachOpen(1, "sleeper", 80, 24, protocol.AttachRW); err != nil {
		t.Fatalf("stuck attach: %v", err)
	}
	snap = waitSessions(t, td, "sleeper", 2)
	if snap.Cols != 80 || snap.Rows != 24 {
		t.Fatalf("viewport with the stuck client = %dx%d, want the 80x24 clamp", snap.Cols, snap.Rows)
	}

	// Eviction, then recovery: the survivor's geometry comes back on its own.
	snap = waitSessions(t, td, "sleeper", 1)
	if snap.Cols != 200 || snap.Rows != 50 {
		t.Fatalf("viewport after eviction = %dx%d, want 200x50 restored", snap.Cols, snap.Rows)
	}
	if snap.Sessions[0].Cols != 200 || snap.Sessions[0].Rows != 50 {
		t.Fatalf("surviving session = %+v, want the healthy 200x50 one", snap.Sessions[0])
	}

	// And the guest PTY was really driven back up, not just the bookkeeping.
	resizes := rec.snapshot()
	if len(resizes) == 0 || resizes[len(resizes)-1] != [2]int{200, 50} {
		t.Fatalf("PTY resizes = %v, want the last one to raise the guest to 200x50", resizes)
	}
}

// TestClientThatNeverPongedIsNotEvicted is the compatibility guard: a client
// that has never answered a PING is an old client that does not speak PONG, not
// a wedged one, and reaping it would kill a working session on an older build.
func TestClientThatNeverPongedIsNotEvicted(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML, reapFast)
	startSleeper(t, td)

	silent := td.dial(t, nil)
	if err := silent.AttachOpen(1, "sleeper", 80, 24, protocol.AttachRW); err != nil {
		t.Fatalf("attach: %v", err)
	}
	waitSessions(t, td, "sleeper", 1)

	// Well past the point a client that had ponged once would have been reaped
	// (the eviction tests above land in ~4 ping intervals).
	time.Sleep(20 * 75 * time.Millisecond)

	snap, ok := td.reg.SnapshotFor("sleeper")
	if !ok || len(snap.Sessions) != 1 {
		t.Fatalf("sessions for a never-PONGed client = %+v, want it still attached", snap.Sessions)
	}
}

// TestHealthyClientIsNeverEvicted guards the expensive failure mode: a false
// eviction kills a session that was working. A client answering every PING must
// survive indefinitely, and its connection must still be usable afterwards.
func TestHealthyClientIsNeverEvicted(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML, reapPatient)
	startSleeper(t, td)

	healthy := td.dial(t, nil)
	stopHealthy := pongForever(healthy)
	defer stopHealthy()
	if err := healthy.AttachOpen(1, "sleeper", 120, 40, protocol.AttachRW); err != nil {
		t.Fatalf("attach: %v", err)
	}
	waitSessions(t, td, "sleeper", 1)

	// Four full silence budgets' worth of PINGs, every one of them answered.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snap, ok := td.reg.SnapshotFor("sleeper")
		if !ok || len(snap.Sessions) != 1 {
			t.Fatalf("healthy client evicted: sessions = %+v", snap.Sessions)
		}
		if snap.Cols != 120 || snap.Rows != 40 {
			t.Fatalf("viewport = %dx%d, want the healthy client's 120x40", snap.Cols, snap.Rows)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The connection is still live: a resize from it still reaches the mux.
	if err := healthy.AttachResize(1, 130, 45); err != nil {
		t.Fatalf("resize on the surviving connection: %v", err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		snap, _ := td.reg.SnapshotFor("sleeper")
		if snap.Cols == 130 && snap.Rows == 45 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("viewport after resize = %dx%d, want 130x45 — the connection did not survive", snap.Cols, snap.Rows)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// chattyTOML is a harness that never stops writing. Its output is what fills a
// wedged client's socket buffers, which is the condition the reaper has to
// survive — see TestWedgedClientReapedWhileOutputBacksUp.
const chattyTOML = `
[harness.chatty]
cmd = "sh"
args = ["-c", "while :; do printf 'harness output line for the wedged client %s\\n' $i; i=$((i+1)); done"]
description = "never stops talking"
`

// TestWedgedClientReapedWhileOutputBacksUp pins the regression behind #183's
// incomplete fix: the reaper must stay armed while the daemon's write to the
// wedged client is blocked.
//
// The reaper used to share heartbeat's loop, so the eviction check was
// reachable only between PING writes. A wedged client stops reading, its
// socket buffers fill, the session pump blocks in net.Write holding the
// protocol write mutex, and the next PING blocks acquiring it — leaving the
// reaper unreachable exactly when it was needed. Backpressure disarmed the
// backpressure defense.
//
// The old code passed on Linux by accident: a ~208 KiB socket buffer swallows
// a quiet harness's output, so nothing blocked. This test removes that luck by
// keeping output flowing, which fills any platform's buffer.
func TestWedgedClientReapedWhileOutputBacksUp(t *testing.T) {
	td := newTestDaemon(t, chattyTOML, reapFast)
	if _, err := td.dial(t, nil).Start("chatty"); err != nil {
		t.Fatalf("start: %v", err)
	}

	healthy := td.dial(t, nil)
	stopHealthy := pongForever(healthy)
	defer stopHealthy()
	if err := healthy.AttachOpen(1, "chatty", 200, 50, protocol.AttachRW); err != nil {
		t.Fatalf("healthy attach: %v", err)
	}
	waitSessions(t, td, "chatty", 1)

	stuck := td.dial(t, nil)
	wedge(t, stuck)
	if err := stuck.AttachOpen(1, "chatty", 80, 24, protocol.AttachRW); err != nil {
		t.Fatalf("stuck attach: %v", err)
	}
	waitSessions(t, td, "chatty", 2)

	// The wedged client goes, and the survivor's geometry comes back with it.
	snap := waitSessions(t, td, "chatty", 1)
	if snap.Cols != 200 || snap.Rows != 50 {
		t.Fatalf("viewport after eviction = %dx%d, want 200x50 restored", snap.Cols, snap.Rows)
	}
}

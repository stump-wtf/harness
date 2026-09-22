package supervisor

// Graceful Close Tests
//
// Every scenario of SPEC-0012 REQ "Graceful Shutdown" and the runtime half of
// REQ "Shutdown Mode", driven through the Manager with the turn-state watch
// stubbed: the close decision runs on the actor loop against the times the
// test passes CloseStep, so a 13:00 close and its 13:15 cap take microseconds
// and the real process only has to be up or down, not on a schedule.
//
// The durable-log assertion reads the harness's log file itself: logEvent
// writes "close ended reason=…" lines there, and which condition ended each
// close is part of the requirement.
//
// Governing: ADR-0019, SPEC-0012 REQ "Graceful Shutdown", REQ "Shutdown
// Mode", REQ "Turn State Signal"; design.md § "Hold and Release on the
// Manager" (the Closing/CloseStep half).
//
// @joestump-agent 09/21/2026 - Added for stump.wtf/harness#384.
//
// @joestump-agent 09/22/2026 - A restart during a close, and the watch's
// anchor for an unanchored close.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/runtrace"
)

// gracefulH is a gated harness whose closes wait on the agent, capped at
// timeout past the close.
func gracefulH(h core.Harness, timeout time.Duration) core.Harness {
	h.OperatingHours = "Mon 09:00-13:00"
	h.HoursShutdown = core.HoursShutdownGraceful
	h.HoursShutdownTimeout = timeout
	return h
}

// stubWatch is the TurnBridge stand-in: Turn answers from a map the test
// mutates between steps, and Follow/Unfollow are counted so the tests can
// assert the watch is armed and torn down with the close.
type stubWatch struct {
	mu        sync.Mutex
	follows   int
	unfollows int
	closeAt   time.Time // the anchor the last Follow was given
	ts        map[string]runtrace.TurnState
	ok        map[string]bool
	why       map[string]string
}

func newStubWatch() *stubWatch {
	return &stubWatch{ts: map[string]runtrace.TurnState{}, ok: map[string]bool{}, why: map[string]string{}}
}

func (w *stubWatch) Follow(name string, _ runtrace.Scope, _ runtrace.Window, _ []runtrace.Scope, closeAt time.Time) {
	w.mu.Lock()
	w.follows++
	w.closeAt = closeAt
	w.mu.Unlock()
}

func (w *stubWatch) Unfollow(name string) {
	w.mu.Lock()
	w.unfollows++
	w.mu.Unlock()
}

func (w *stubWatch) Turn(name string) (runtrace.TurnState, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.ts[name], w.ok[name]
}

func (w *stubWatch) Unavailable(name string) string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.why[name]
}

func (w *stubWatch) set(name string, ts runtrace.TurnState, ok bool, why string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.ts[name], w.ok[name], w.why[name] = ts, ok, why
}

func (w *stubWatch) counts() (follows, unfollows int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.follows, w.unfollows
}

// newCloseRig builds a manager around one graceful harness with the watch
// stubbed in, plus the paths the assertions need.
func newCloseRig(t *testing.T, h core.Harness) (*Manager, *stubWatch, string, string) {
	t.Helper()
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	logDir := filepath.Join(dir, "logs")
	m := NewManager(managerCfg(h), ManagerOptions{Policy: fastPolicy(), StatePath: statePath, LogDir: logDir})
	t.Cleanup(m.Close)
	w := newStubWatch()
	m.watch = w
	return m, w, statePath, logDir
}

// closeAt is the instant a window ending 13:00 closed, on Monday 2026-09-21.
func closeAt() time.Time { return time.Date(2026, 9, 21, 13, 0, 0, 0, time.UTC) }

// closeLog asserts the durable log recorded the close outcome.
func closeLog(t *testing.T, logDir, name, wantReason string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(logDir, name+".log"))
	if err != nil {
		t.Fatalf("read durable log: %v", err)
	}
	if !strings.Contains(string(data), "close ended") || !strings.Contains(string(data), wantReason) {
		t.Errorf("durable log has no \"close ended\" with reason %q; log tail:\n%s", wantReason, tailOf(string(data), 400))
	}
}

func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// waitNotClosing polls until the harness's close has ended (stopped) — the
// stop itself is synchronous, but the snapshot read after CloseStep can race
// the publish.
func waitStopped(t *testing.T, m *Manager, name string) Snapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap, _ := m.Snapshot(name)
		if snap.State == core.StateStopped {
			return snap
		}
		time.Sleep(5 * time.Millisecond)
	}
	snap, _ := m.Snapshot(name)
	t.Fatalf("%s never stopped: state=%s", name, snap.State)
	return snap
}

// SPEC-0012 Scenario "Turn finishes after the close": mid-turn at the close,
// the turn ends 13:04, the settle period passes, the harness stops at about
// 13:04:10 with `enabled` still true, and the log says turn end.
func TestCloseStopsAfterTurnEndSettles(t *testing.T) {
	m, w, statePath, logDir := newCloseRig(t, gracefulH(shHarness("g", "while true; do sleep 0.02; done", 0), 15*time.Minute))
	m.Start("g")
	waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("g"); return s.State == core.StateRunning })

	m.Hold("g", core.HoursShutdownGraceful, closeAt())
	snap, _ := m.Snapshot("g")
	if !snap.Closing || !snap.Held || snap.State != core.StateRunning || !snap.Enabled {
		t.Fatalf("after hold: closing=%v held=%v state=%s enabled=%v, want closing, held, running, enabled",
			snap.Closing, snap.Held, snap.State, snap.Enabled)
	}
	if !snap.CloseAt.Equal(closeAt()) {
		t.Errorf("CloseAt = %v, want %v", snap.CloseAt, closeAt())
	}

	w.set("g", runtrace.TurnState{LastEventAt: closeAt().Add(4 * time.Minute), TurnMarkers: true, TurnEnded: true}, true, "")
	// The settle period has not passed: still closing.
	m.CloseStep("g", closeAt().Add(4*time.Minute).Add(closeSettle-time.Millisecond))
	if s, _ := m.Snapshot("g"); s.State != core.StateRunning {
		t.Fatalf("stopped before the settle period: state=%s", s.State)
	}
	// Settle met: stopped.
	m.CloseStep("g", closeAt().Add(4*time.Minute).Add(closeSettle))
	stopped := waitStopped(t, m, "g")
	if !stopped.Enabled {
		t.Error("a close must not touch enabled intent")
	}
	if stopped.RestartCount != 0 {
		t.Errorf("restart count %d after the close: the close respawned", stopped.RestartCount)
	}
	ph := waitPersisted(t, statePath, "g", core.StateStopped)
	if !ph.Enabled {
		t.Error("state.json recorded enabled=false for a closed harness")
	}
	closeLog(t, logDir, "g", "turn ended")
}

// SPEC-0012 Scenario "Turn never finishes": still mid-turn at the cap, the
// harness stops at the deadline and the log records a forced stop.
func TestCloseForcedAtCap(t *testing.T) {
	m, w, _, logDir := newCloseRig(t, gracefulH(shHarness("g", "while true; do sleep 0.02; done", 0), 15*time.Minute))
	m.Start("g")
	waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("g"); return s.State == core.StateRunning })

	m.Hold("g", core.HoursShutdownGraceful, closeAt())
	w.set("g", runtrace.TurnState{LastEventAt: closeAt().Add(14 * time.Minute), TurnMarkers: true, TurnEnded: false}, true, "")
	m.CloseStep("g", closeAt().Add(15*time.Minute).Add(-time.Second))
	if s, _ := m.Snapshot("g"); s.State != core.StateRunning {
		t.Fatalf("stopped one second before the cap: state=%s", s.State)
	}
	m.CloseStep("g", closeAt().Add(15*time.Minute))
	waitStopped(t, m, "g")
	closeLog(t, logDir, "g", "deadline reached")
}

// SPEC-0012 Scenario "No turn markers": the reader reports no boundaries, so
// the quiet period decides — last event 13:01, stopped at 13:03.
func TestCloseOnQuietWithoutMarkers(t *testing.T) {
	m, w, _, logDir := newCloseRig(t, gracefulH(shHarness("g", "while true; do sleep 0.02; done", 0), 15*time.Minute))
	m.Start("g")
	waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("g"); return s.State == core.StateRunning })

	m.Hold("g", core.HoursShutdownGraceful, closeAt())
	w.set("g", runtrace.TurnState{LastEventAt: closeAt().Add(time.Minute)}, true, "")
	// Quiet is measured from the last event: 13:01 + 2m = 13:03.
	m.CloseStep("g", closeAt().Add(3*time.Minute).Add(-time.Millisecond))
	if s, _ := m.Snapshot("g"); s.State != core.StateRunning {
		t.Fatalf("stopped one millisecond before quiet: state=%s", s.State)
	}
	m.CloseStep("g", closeAt().Add(3*time.Minute))
	waitStopped(t, m, "g")
	closeLog(t, logDir, "g", "quiet")
}

// SPEC-0012 REQ "Graceful Shutdown": nothing attributable to the run — a
// generic harness here — stops at once, and the log says why.
func TestCloseWithoutAttributableTraceStopsAtOnce(t *testing.T) {
	m, w, _, logDir := newCloseRig(t, gracefulH(shHarness("g", "while true; do sleep 0.02; done", 0), 15*time.Minute))
	m.Start("g")
	waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("g"); return s.State == core.StateRunning })

	m.Hold("g", core.HoursShutdownGraceful, closeAt())
	w.set("g", runtrace.TurnState{}, false, "no agent-trace session is attributable to this run")
	m.CloseStep("g", closeAt().Add(time.Second))
	waitStopped(t, m, "g")
	closeLog(t, logDir, "g", "graceful unavailable")

	// The watch is torn down with the close, in both the available and the
	// unavailable shape.
	if _, unfollows := w.counts(); unfollows == 0 {
		t.Error("the watch was never unfollowed after the close ended")
	}
}

// SPEC-0012 Scenario "Reopened during a close": hours reopening cancels the
// close and the harness keeps running.
func TestHoursReopeningCancelsTheClose(t *testing.T) {
	m, _, _, _ := newCloseRig(t, gracefulH(shHarness("g", "while true; do sleep 0.02; done", 0), 15*time.Minute))
	m.Start("g")
	waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("g"); return s.State == core.StateRunning })

	m.Hold("g", core.HoursShutdownGraceful, closeAt())
	m.Release("g") // hours reopen at 12:05
	m.CloseStep("g", closeAt().Add(5*time.Minute))
	snap, _ := m.Snapshot("g")
	if snap.State != core.StateRunning || snap.Closing || snap.Held {
		t.Fatalf("after reopen: state=%s closing=%v held=%v, want running, neither", snap.State, snap.Closing, snap.Held)
	}
}

// SPEC-0012 REQ "Graceful Shutdown": a prompt that starts a new turn during a
// close does not extend the deadline — fresh events keep the close off the
// quiet condition and the cap still lands.
func TestNewPromptDuringCloseDoesNotExtendDeadline(t *testing.T) {
	m, w, _, logDir := newCloseRig(t, gracefulH(shHarness("g", "while true; do sleep 0.02; done", 0), 15*time.Minute))
	m.Start("g")
	waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("g"); return s.State == core.StateRunning })

	m.Hold("g", core.HoursShutdownGraceful, closeAt())
	// A doorbell starts a turn at 13:08; events keep arriving until 13:14:59.
	for _, at := range []time.Duration{8 * time.Minute, 12 * time.Minute, 14 * time.Minute, 14*time.Minute + 59*time.Second} {
		w.set("g", runtrace.TurnState{LastEventAt: closeAt().Add(at), TurnMarkers: true, TurnEnded: false}, true, "")
		m.CloseStep("g", closeAt().Add(at))
		if s, _ := m.Snapshot("g"); s.State != core.StateRunning {
			t.Fatalf("stopped early at %v: state=%s", closeAt().Add(at), s.State)
		}
	}
	m.CloseStep("g", closeAt().Add(15*time.Minute))
	waitStopped(t, m, "g")
	closeLog(t, logDir, "g", "deadline reached")
}

// SPEC-0012 REQ "Graceful Shutdown": a process that exits on its own while
// closing is held without a restart — the gate owns the exit, not the restart
// policy.
func TestSelfExitWhileClosingIsHeldWithoutRestart(t *testing.T) {
	p := fastPolicy()
	p.MaxRestarts = 100 // the policy would respawn; the hold must not let it
	m, _, statePath, _ := newCloseRig(t, gracefulH(shHarness("g", "sleep 0.05; exit 1", 50*time.Millisecond), 15*time.Minute))
	m.Start("g")
	waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("g"); return s.State == core.StateRunning })

	m.Hold("g", core.HoursShutdownGraceful, closeAt().Add(-time.Second))
	stopped := waitStopped(t, m, "g")
	if !stopped.Held || !stopped.Enabled {
		t.Fatalf("after self-exit while closing: held=%v enabled=%v, want both", stopped.Held, stopped.Enabled)
	}
	if stopped.RestartCount != 0 {
		t.Errorf("restart count %d: the exit was counted against the restart policy", stopped.RestartCount)
	}
	time.Sleep(200 * time.Millisecond) // several restart delays
	if s, _ := m.Snapshot("g"); s.State != core.StateStopped {
		t.Errorf("respawned after a hold exit: state=%s", s.State)
	}
	if ph := waitPersisted(t, statePath, "g", core.StateStopped); !ph.Enabled {
		t.Error("state.json recorded enabled=false for a harness held by its own exit")
	}
}

// SPEC-0012 REQ "Shutdown Mode", runtime half: mode and timeout changes apply
// to a close in progress without a restart.
func TestReloadAppliesToACloseInProgress(t *testing.T) {
	t.Run("immediate mid-close stops on the next step", func(t *testing.T) {
		m, _, _, _ := newCloseRig(t, gracefulH(shHarness("g", "while true; do sleep 0.02; done", 0), 15*time.Minute))
		m.Start("g")
		waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("g"); return s.State == core.StateRunning })
		m.Hold("g", core.HoursShutdownGraceful, closeAt())

		h := m.Config().Harnesses["g"]
		h.HoursShutdown = core.HoursShutdownImmediate
		m.get("g").applyConfig(h) // supervision keys apply without a restart

		m.CloseStep("g", closeAt().Add(time.Second))
		waitStopped(t, m, "g")
	})

	t.Run("shortened timeout past the deadline stops", func(t *testing.T) {
		m, _, _, _ := newCloseRig(t, gracefulH(shHarness("g", "while true; do sleep 0.02; done", 0), 15*time.Minute))
		m.Start("g")
		waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("g"); return s.State == core.StateRunning })
		m.Hold("g", core.HoursShutdownGraceful, closeAt())

		h := m.Config().Harnesses["g"]
		h.HoursShutdownTimeout = time.Minute // new deadline 13:01, already past
		m.get("g").applyConfig(h)

		m.CloseStep("g", closeAt().Add(15*time.Minute))
		waitStopped(t, m, "g")
	})

	t.Run("lengthened timeout keeps waiting", func(t *testing.T) {
		m, _, _, _ := newCloseRig(t, gracefulH(shHarness("g", "while true; do sleep 0.02; done", 0), 15*time.Minute))
		m.Start("g")
		waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("g"); return s.State == core.StateRunning })
		m.Hold("g", core.HoursShutdownGraceful, closeAt())

		h := m.Config().Harnesses["g"]
		h.HoursShutdownTimeout = 30 * time.Minute
		m.get("g").applyConfig(h)

		w := m.watch.(*stubWatch)
		w.set("g", runtrace.TurnState{LastEventAt: closeAt(), TurnMarkers: true, TurnEnded: false}, true, "")
		m.CloseStep("g", closeAt().Add(20*time.Minute))
		if s, _ := m.Snapshot("g"); s.State != core.StateRunning {
			t.Fatalf("stopped inside the lengthened cap: state=%s", s.State)
		}
		m.CloseStep("g", closeAt().Add(30*time.Minute))
		waitStopped(t, m, "g")
	})
}

// The DST scenario's deadline arithmetic: the close anchored to 01:30 EDT
// (05:30 UTC) with a 15-minute cap stops at 01:45 on the real timeline
// (05:45 UTC) — not an hour later when the repeated wall-clock hour reaches
// 01:45 the second time (06:45 UTC).
func TestCloseDeadlineIsOnTheRealTimeline(t *testing.T) {
	m, w, _, logDir := newCloseRig(t, gracefulH(shHarness("g", "while true; do sleep 0.02; done", 0), 15*time.Minute))
	m.Start("g")
	waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("g"); return s.State == core.StateRunning })

	anchor := time.Date(2026, 11, 1, 5, 30, 0, 0, time.UTC) // 01:30 EDT, first pass
	m.Hold("g", core.HoursShutdownGraceful, anchor)
	w.set("g", runtrace.TurnState{LastEventAt: anchor, TurnMarkers: true, TurnEnded: false}, true, "")

	m.CloseStep("g", anchor.Add(14*time.Minute).Add(59*time.Second))
	if s, _ := m.Snapshot("g"); s.State != core.StateRunning {
		t.Fatalf("stopped before the real-timeline cap: state=%s", s.State)
	}
	m.CloseStep("g", anchor.Add(15*time.Minute)) // 01:45 EDT == 05:45 UTC
	waitStopped(t, m, "g")
	closeLog(t, logDir, "g", "deadline reached")
}

// SPEC-0012 REQ "After-Hours Lease" × "Graceful Shutdown": a stop during a
// close stops at once, discards the lease, and clears enabled; a lease
// starting cancels the close.
func TestStopAndLeaseDuringAClose(t *testing.T) {
	t.Run("stop mid-close", func(t *testing.T) {
		m, _, _, _ := newCloseRig(t, gracefulH(shHarness("g", "while true; do sleep 0.02; done", 0), 15*time.Minute))
		m.Start("g")
		waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("g"); return s.State == core.StateRunning })
		m.Hold("g", core.HoursShutdownGraceful, closeAt())
		m.Stop("g")
		snap, _ := m.Snapshot("g")
		if snap.State != core.StateStopped || snap.Closing || snap.Held || snap.Enabled {
			t.Fatalf("after stop mid-close: state=%s closing=%v held=%v enabled=%v", snap.State, snap.Closing, snap.Held, snap.Enabled)
		}
	})

	t.Run("lease start cancels the close", func(t *testing.T) {
		m, w, _, _ := newCloseRig(t, gracefulH(shHarness("g", "while true; do sleep 0.02; done", 0), 15*time.Minute))
		m.Start("g")
		waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("g"); return s.State == core.StateRunning })
		m.Hold("g", core.HoursShutdownGraceful, closeAt())
		if err := m.StartFor("g", time.Hour); err != nil {
			t.Fatalf("StartFor: %v", err)
		}
		m.CloseStep("g", closeAt().Add(time.Minute))
		snap, _ := m.Snapshot("g")
		if snap.State != core.StateRunning || snap.Closing {
			t.Fatalf("after lease start: state=%s closing=%v, want running, not closing", snap.State, snap.Closing)
		}
		if _, unfollows := w.counts(); unfollows == 0 {
			t.Error("the watch survived a cancelled close")
		}
	})
}

// A restart during a close cancels it, as a start does. The gate then decides
// the harness afresh on its next tick (up and out of hours: hold again), and
// the harness still comes back when its hours open. Before the fix the close
// survived the restart on a harness no longer held, so the pass kept stepping
// it, the stop that ended it left the harness down with held=false, and no
// window opening ever released it.
func TestRestartDuringACloseCancelsIt(t *testing.T) {
	m, w, _, _ := newCloseRig(t, gracefulH(shHarness("g", "while true; do sleep 0.02; done", 0), 15*time.Minute))
	m.Start("g")
	waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("g"); return s.State == core.StateRunning })
	m.Hold("g", core.HoursShutdownGraceful, closeAt())
	w.set("g", runtrace.TurnState{LastEventAt: closeAt().Add(14 * time.Minute), TurnMarkers: true}, true, "")

	m.Restart("g")
	waitFor(t, 3*time.Second, "running after the restart", func() bool { s, _ := m.Snapshot("g"); return s.State == core.StateRunning })

	// What the gate pass does on each later out-of-hours tick: step a close in
	// flight, otherwise hold a harness that is up (gate.go gatePass).
	gateTick := func(now time.Time) {
		if s, _ := m.Snapshot("g"); s.Closing {
			m.CloseStep("g", now)
		} else if snapUp(s.State) {
			m.Hold("g", core.HoursShutdownGraceful, closeAt())
		}
	}
	gateTick(closeAt().Add(time.Minute))
	gateTick(closeAt().Add(15 * time.Minute)) // the cap
	stopped := waitStopped(t, m, "g")
	if !stopped.Held || !stopped.Enabled {
		t.Fatalf("after the close that followed a restart: held=%v enabled=%v, want both", stopped.Held, stopped.Enabled)
	}
	m.Release("g") // the next window opens
	waitFor(t, 3*time.Second, "running at the next open", func() bool { s, _ := m.Snapshot("g"); return s.State == core.StateRunning })
}

// An unanchored hold (the gate could not find the boundary, closeAt zero) is
// capped from now on the actor loop, and the watch must be armed with that
// same anchor. The zero time would bound the watcher's linger backstop at
// 00:05 UTC, year 1, retiring the watch on its first poll and ending the
// close as "graceful unavailable".
func TestUnanchoredCloseArmsTheWatchWithItsRealAnchor(t *testing.T) {
	m, w, _, _ := newCloseRig(t, gracefulH(shHarness("g", "while true; do sleep 0.02; done", 0), 15*time.Minute))
	m.Start("g")
	waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("g"); return s.State == core.StateRunning })

	m.Hold("g", core.HoursShutdownGraceful, time.Time{})
	snap, _ := m.Snapshot("g")
	if !snap.Closing || snap.CloseAt.IsZero() {
		t.Fatalf("unanchored hold: closing=%v closeAt=%v, want a close anchored to now", snap.Closing, snap.CloseAt)
	}
	w.mu.Lock()
	got := w.closeAt
	w.mu.Unlock()
	if !got.Equal(snap.CloseAt) {
		t.Fatalf("watch armed with closeAt %v, want the close's own anchor %v", got, snap.CloseAt)
	}
}

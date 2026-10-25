package supervisor

// Graceful Stop Tests
//
// The stop paths an operator takes by hand, asked to wait for the turn in
// progress instead of cutting it short (stump.wtf/harness#871). Everything here
// reuses the operating-hours close machinery, so the scenarios mirror
// hours_close_test.go: the turn-state watch is stubbed, the close's decision runs
// on the actor loop, and the only new thing under test is WHO enters the close and
// WHO steps it, since a manual stop has no scheduler pass looking after it.
//
// The manager drives these steps on its own clock, so the tests shorten
// closeTick rather than passing fake times: what must be observable is that the
// harness stayed up while the turn was running and went down once the wait ended,
// with the stop itself recorded exactly as an immediate stop records it.
//
// Governing: SPEC-0012 REQ "Graceful Shutdown", REQ "Turn State Signal";
// SPEC-0003 REQ "Graceful Stop"; ADR-0019.

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/runtrace"
)

// turnRunning is the watch saying this harness is mid-turn right now.
func turnRunning() runtrace.TurnState {
	return runtrace.TurnState{LastEventAt: time.Now(), TurnMarkers: true, TurnEnded: false}
}

// turnEnded is the watch saying the turn ended a while ago, past the settle
// period, so the next step reads it as done.
func turnEnded() runtrace.TurnState {
	return runtrace.TurnState{LastEventAt: time.Now().Add(-time.Hour), TurnMarkers: true, TurnEnded: true}
}

// stopRig is one ungated harness with the watch stubbed in and a fast close tick:
// ungated, because nothing here should involve the hours gate, and a gate would
// give the close a second driver and blur what is being asserted.
func stopRig(t *testing.T, name string) (*Manager, *stubWatch, string) {
	t.Helper()
	dir := t.TempDir()
	logDir := filepath.Join(dir, "logs")
	h := shHarness(name, "while true; do sleep 0.02; done", 0)
	m := NewManager(managerCfg(h), ManagerOptions{
		Policy:    fastPolicy(),
		StatePath: filepath.Join(dir, "state.json"),
		LogDir:    logDir,
	})
	t.Cleanup(m.Close)
	m.watch = newStubWatch()
	m.closeTick = 5 * time.Millisecond
	m.Start(name)
	waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot(name); return s.State == core.StateRunning })
	return m, m.watch.(*stubWatch), logDir
}

// A graceful stop leaves the harness running while the turn is in progress, then
// stops it with the operator's intent cleared (SPEC-0003 REQ "Graceful Stop"), and
// says in the durable log what ended the wait.
func TestStopGracefulWaitsForTurnEnd(t *testing.T) {
	const name = "g"
	m, w, logDir := stopRig(t, name)
	w.set(name, turnRunning(), true, "")

	done := make(chan bool, 1)
	go func() { done <- m.StopGraceful(name, 30*time.Second) }()

	waitFor(t, 3*time.Second, "close entered", func() bool { s, _ := m.Snapshot(name); return s.Closing })
	// Intent clears the moment the stop is asked for, as it always has: the wait
	// moves the signal, not the operator's decision, so nothing can restart the
	// harness while it is draining.
	if s, _ := m.Snapshot(name); s.State != core.StateRunning || s.Enabled {
		t.Fatalf("while waiting: state=%s enabled=%v, want still running with intent cleared", s.State, s.Enabled)
	}
	if follows, _ := w.counts(); follows == 0 {
		t.Error("the stop armed no turn-state watch, so nothing could be waited on")
	}

	w.set(name, turnEnded(), true, "")
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("StopGraceful reported an unknown harness")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("StopGraceful did not return after the turn ended")
	}

	stopped := waitStopped(t, m, name)
	if stopped.Enabled {
		t.Error("a graceful stop left enabled=true; stopping is the operator's intent either way")
	}
	if m.get(name).Snapshot().Closing {
		t.Error("the close is still marked in flight after the stop returned")
	}
	closeLog(t, logDir, name, "turn ended")
}

// The cap bounds the wait: a turn that never ends is stopped at the cap, not left
// running, and not left waiting (SPEC-0012 REQ "Graceful Shutdown").
func TestStopGracefulCapForcesTheStop(t *testing.T) {
	const name = "g"
	m, w, logDir := stopRig(t, name)
	w.set(name, turnRunning(), true, "")

	started := time.Now()
	if !m.StopGraceful(name, 200*time.Millisecond) {
		t.Fatal("StopGraceful reported an unknown harness")
	}
	if elapsed := time.Since(started); elapsed < 150*time.Millisecond {
		t.Errorf("returned after %v, before the cap could pass", elapsed)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Errorf("returned after %v, long past its 200ms cap", elapsed)
	}
	waitStopped(t, m, name)
	if s, _ := m.Snapshot(name); s.Enabled {
		t.Error("the cap passed but the stop did not clear intent")
	}
	closeLog(t, logDir, name, "deadline reached")
}

// Without the flag nothing waits and nothing is watched: an immediate stop keeps
// its exact old behavior, including tearing down any watch it finds.
func TestStopWithoutFlagDoesNotWait(t *testing.T) {
	const name = "g"
	m, w, logDir := stopRig(t, name)
	w.set(name, turnRunning(), true, "")

	if !m.Stop(name) {
		t.Fatal("Stop reported an unknown harness")
	}
	stopped := waitStopped(t, m, name)
	if stopped.Closing {
		t.Error("an immediate stop left a close in flight")
	}
	follows, unfollows := w.counts()
	if follows != 0 {
		t.Errorf("an immediate stop armed the turn-state watch %d times", follows)
	}
	if unfollows == 0 {
		t.Error("an immediate stop left the watch armed")
	}
	if log := readLog(t, logDir, name); strings.Contains(log, "graceful stop waiting") {
		t.Errorf("an immediate stop logged a graceful wait:\n%s", tailOf(log, 400))
	}
}

// A graceful restart waits for the turn and then brings the harness straight back
// up, with its intent kept: the only difference from an immediate restart is when
// the process is replaced.
func TestRestartGracefulWaitsThenStartsAgain(t *testing.T) {
	const name = "g"
	m, w, logDir := stopRig(t, name)
	before, _ := m.Snapshot(name)
	w.set(name, turnRunning(), true, "")

	done := make(chan error, 1)
	go func() {
		ok, refused := m.RestartGracefulPeer(name, "", 30*time.Second)
		if !ok {
			done <- errors.New("unknown harness")
		} else {
			done <- refused
		}
	}()

	waitFor(t, 3*time.Second, "close entered", func() bool { s, _ := m.Snapshot(name); return s.Closing })
	w.set(name, turnEnded(), true, "")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("graceful restart: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RestartGracefulPeer did not return after the turn ended")
	}

	waitFor(t, 5*time.Second, "running again", func() bool {
		s, _ := m.Snapshot(name)
		return s.State == core.StateRunning && s.PID > 0 && s.PID != before.PID
	})
	snap, _ := m.Snapshot(name)
	if !snap.Enabled {
		t.Error("a restart cleared enabled intent")
	}
	closeLog(t, logDir, name, "turn ended")
}

// Daemon shutdown drains every resident harness when it was armed to, and stops
// the run as interrupted rather than cancelled: the daemon went down under it
// (SPEC-0022 REQ-5), it was not stopped by anyone.
func TestCloseDrainsTurnsWhenArmed(t *testing.T) {
	const name = "g"
	m, w, logDir := stopRig(t, name)
	w.set(name, turnRunning(), true, "")

	m.ArmShutdownDrain(30 * time.Second)
	if s, _ := m.Snapshot(name); s.State != core.StateRunning {
		t.Fatalf("arming a drain stopped the harness: state=%s", s.State)
	}
	w.set(name, turnEnded(), true, "")

	m.Close()
	snap, ok := m.Snapshot(name)
	if !ok {
		t.Fatalf("%s is gone from the manager after Close", name)
	}
	if snap.State != core.StateStopped {
		t.Errorf("state after a drained shutdown = %s, want stopped", snap.State)
	}
	closeLog(t, logDir, name, "turn ended")
}

// An unarmed daemon shutdown is untouched by any of this: no close is entered, so
// the harness is stopped by the normal sequence at once.
func TestCloseWithoutDrainIsUnchanged(t *testing.T) {
	const name = "g"
	m, w, logDir := stopRig(t, name)
	w.set(name, turnRunning(), true, "")

	m.Close()
	snap, _ := m.Snapshot(name)
	if snap.State != core.StateStopped {
		t.Errorf("state after a plain shutdown = %s, want stopped", snap.State)
	}
	follows, _ := w.counts()
	if follows != 0 {
		t.Errorf("a plain shutdown armed the turn-state watch %d times", follows)
	}
	if log := readLog(t, logDir, name); strings.Contains(log, "close ended") {
		t.Errorf("a plain shutdown ran a graceful close:\n%s", tailOf(log, 400))
	}
}

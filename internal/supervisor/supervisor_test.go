package supervisor

// Governing tests: SPEC-0003 REQ "State Model", "Autostart", "Restart On Exit",
// "Crash-Loop Detection", "Backoff Give-Up", "Graceful Stop", "Config Change
// Application". Each spec scenario is exercised explicitly against a real PTY-
// spawned process; the whole file is meant to run under `go test -race`.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// ---- test helpers --------------------------------------------------------

// fastPolicy shrinks every duration so the state machine runs in milliseconds.
func fastPolicy() Policy {
	return Policy{
		CrashWindow:    150 * time.Millisecond,
		CrashThreshold: 3,
		BackoffBase:    5 * time.Millisecond,
		BackoffCap:     30 * time.Millisecond,
		MaxRestarts:    3,
		StopGrace:      80 * time.Millisecond,
	}
}

// shHarness builds a harness that runs `sh -c script`.
func shHarness(name, script string, restartDelay time.Duration) core.Harness {
	return core.Harness{
		Name:         name,
		Cmd:          "sh",
		Args:         []string{"-c", script},
		Backend:      core.BackendNative,
		RestartDelay: restartDelay,
	}
}

// newTestSupervisor builds a supervisor writing logs into a temp dir.
func newTestSupervisor(t *testing.T, h core.Harness, p Policy) *Supervisor {
	t.Helper()
	s := New(h, Options{Policy: p, Bus: NewBus(), LogCfg: LogConfig{Dir: t.TempDir()}})
	t.Cleanup(s.Shutdown)
	return s
}

// waitFor polls cond until true or the deadline; fails the test otherwise.
func waitFor(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timed out after %v waiting for: %s", timeout, desc)
}

func waitState(t *testing.T, s *Supervisor, want core.State) {
	t.Helper()
	waitFor(t, 3*time.Second, "state == "+string(want), func() bool {
		return s.Snapshot().State == want
	})
}

// ---- SPEC-0003 REQ "State Model" / "Autostart" ---------------------------

func TestStartBringsHarnessUp(t *testing.T) {
	s := newTestSupervisor(t, shHarness("up", "while true; do sleep 0.02; done", 0),
		Policy{CrashWindow: time.Second, CrashThreshold: 3, MaxRestarts: 5, StopGrace: 200 * time.Millisecond})
	s.Start()
	waitState(t, s, core.StateRunning)
	snap := s.Snapshot()
	if !snap.Enabled {
		t.Fatal("expected enabled=true after Start (intent)")
	}
	if snap.PID == 0 {
		t.Fatal("expected a live PID once running")
	}
}

// ---- SPEC-0003 REQ "Restart On Exit": clean exit while enabled -----------

func TestCleanExitWhileEnabledRestarts(t *testing.T) {
	// Never flap or give up: exercise pure restart-on-clean-exit.
	p := Policy{CrashWindow: 10 * time.Millisecond, CrashThreshold: 1000, MaxRestarts: 0, StopGrace: 100 * time.Millisecond}
	s := newTestSupervisor(t, shHarness("clean", "exit 0", 5*time.Millisecond), p)
	s.Start()
	// Each clean exit (code 0) must be followed by a restart, bumping ↻.
	waitFor(t, 3*time.Second, "restart count grows past 2", func() bool {
		return s.Snapshot().RestartCount >= 2
	})
	if code := s.Snapshot().LastExitCode; code != 0 {
		t.Fatalf("last exit code = %d, want 0", code)
	}
}

// ---- SPEC-0003 REQ "Restart On Exit": exit while disabled ----------------

func TestStopLeavesHarnessStoppedNotRespawned(t *testing.T) {
	s := newTestSupervisor(t, shHarness("dis", "while true; do sleep 0.02; done", 0), fastPolicy())
	s.Start()
	waitState(t, s, core.StateRunning)
	s.Stop()
	if snap := s.Snapshot(); snap.State != core.StateStopped {
		t.Fatalf("after Stop state = %s, want stopped", snap.State)
	}
	if s.Snapshot().Enabled {
		t.Fatal("Stop must clear enabled intent")
	}
	// Must NOT respawn: still stopped a beat later.
	time.Sleep(60 * time.Millisecond)
	if snap := s.Snapshot(); snap.State != core.StateStopped {
		t.Fatalf("respawned after stop: state = %s", snap.State)
	}
}

// ---- SPEC-0003 REQ "Crash-Loop Detection": flapping reaches degraded -----

func TestCrashLoopReachesDegraded(t *testing.T) {
	s := newTestSupervisor(t, shHarness("flap", "exit 1", 0), fastPolicy())
	s.Start()
	waitFor(t, 3*time.Second, "harness flags flapping/degraded", func() bool {
		snap := s.Snapshot()
		return snap.Flapping || snap.State == core.StateDegraded || snap.State == core.StateFailed
	})
	if !s.Snapshot().Flapping && s.Snapshot().State != core.StateFailed {
		t.Fatal("expected flapping to be detected")
	}
}

// ---- SPEC-0003 REQ "Crash-Loop Detection": recovery on a clean run -------

func TestCrashLoopRecoversOnSurvivingRun(t *testing.T) {
	dir := t.TempDir()
	counter := filepath.Join(dir, "n")
	// Crash the first 3 runs, then stay up longer than the crash window so the
	// counter resets and the harness returns to running.
	script := "n=$(cat '" + counter + "' 2>/dev/null || echo 0); n=$((n+1)); echo $n > '" + counter + "'; " +
		"if [ $n -le 3 ]; then exit 1; fi; sleep 5"
	p := Policy{CrashWindow: 120 * time.Millisecond, CrashThreshold: 3, BackoffBase: 3 * time.Millisecond,
		BackoffCap: 20 * time.Millisecond, MaxRestarts: 50, StopGrace: 100 * time.Millisecond}
	s := newTestSupervisor(t, shHarness("recover", script, 3*time.Millisecond), p)
	s.Start()
	// It should flap, then survive and recover to running with flapping cleared.
	waitFor(t, 4*time.Second, "recovered to running, flapping cleared", func() bool {
		snap := s.Snapshot()
		return snap.State == core.StateRunning && !snap.Flapping
	})
}

// ---- SPEC-0003 REQ "Backoff Give-Up": parks failed, manual restart revives

func TestGiveUpFailsThenManualRestartRevives(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	// Exit 1 until a "good" file exists, then stay up.
	script := "if [ -f '" + good + "' ]; then sleep 5; else exit 1; fi"
	p := Policy{CrashWindow: 200 * time.Millisecond, CrashThreshold: 2, BackoffBase: 3 * time.Millisecond,
		BackoffCap: 15 * time.Millisecond, MaxRestarts: 3, StopGrace: 100 * time.Millisecond}
	s := newTestSupervisor(t, shHarness("giveup", script, 3*time.Millisecond), p)
	s.Start()

	// Exhausts attempts → failed, stops respawning.
	waitState(t, s, core.StateFailed)
	failedAt := s.Snapshot().RestartCount
	time.Sleep(60 * time.Millisecond)
	if s.Snapshot().State != core.StateFailed {
		t.Fatal("failed harness must stop respawning")
	}
	if s.Snapshot().RestartCount != failedAt {
		t.Fatal("restart count moved while failed — still respawning")
	}

	// Operator fixes the environment and issues a manual restart.
	if err := os.WriteFile(good, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.Restart()
	// Latch clears; a fresh start cycle brings it back to running.
	waitState(t, s, core.StateRunning)
}

// ---- SPEC-0003 REQ "Graceful Stop": fast SIGTERM path --------------------

func TestGracefulStopFast(t *testing.T) {
	s := newTestSupervisor(t, shHarness("term", "while true; do sleep 0.02; done", 0), fastPolicy())
	s.Start()
	waitState(t, s, core.StateRunning)
	start := time.Now()
	s.Stop()
	elapsed := time.Since(start)
	if s.Snapshot().State != core.StateStopped {
		t.Fatalf("state = %s, want stopped", s.Snapshot().State)
	}
	// A well-behaved process exits on SIGTERM well before the grace period.
	if elapsed >= fastPolicy().StopGrace {
		t.Fatalf("SIGTERM path took %v (>= grace); should exit promptly", elapsed)
	}
}

// ---- SPEC-0003 REQ "Graceful Stop": SIGKILL escalation -------------------

func TestGracefulStopSigkillEscalation(t *testing.T) {
	// Ignore SIGTERM in the shell; the group SIGKILL after grace must reap it.
	// The script touches a readiness file only after installing the TERM trap,
	// so the test never races the stop against an un-armed handler.
	ready := filepath.Join(t.TempDir(), "ready")
	script := "trap '' TERM; : > '" + ready + "'; while true; do sleep 0.02; done"
	s := newTestSupervisor(t, shHarness("kill", script, 0), fastPolicy())
	s.Start()
	waitState(t, s, core.StateRunning)
	waitFor(t, 2*time.Second, "TERM trap installed", func() bool {
		_, err := os.Stat(ready)
		return err == nil
	})
	start := time.Now()
	s.Stop()
	elapsed := time.Since(start)
	if s.Snapshot().State != core.StateStopped {
		t.Fatalf("state = %s, want stopped after SIGKILL", s.Snapshot().State)
	}
	// It should have waited out the grace period before escalating.
	if elapsed < fastPolicy().StopGrace {
		t.Fatalf("stopped in %v (< grace); SIGTERM-ignoring process should force the grace wait", elapsed)
	}
}

// ---- SPEC-0003 REQ "Config Change Application" ---------------------------

func TestConfigChangeAppliesOnNextRestart(t *testing.T) {
	s := newTestSupervisor(t, shHarness("cfg", "while true; do sleep 0.02; done", 0),
		Policy{CrashWindow: time.Second, CrashThreshold: 3, MaxRestarts: 5, StopGrace: 200 * time.Millisecond})
	s.Start()
	waitState(t, s, core.StateRunning)
	origPID := s.Snapshot().PID

	// Change the definition while running: the process must be untouched and the
	// change flagged "restart to apply".
	changed := shHarness("cfg", "sleep 60", 0)
	s.ApplyConfig(changed)
	if snap := s.Snapshot(); !snap.ConfigChanged {
		t.Fatal("expected ConfigChanged=true after editing a running harness")
	}
	if s.Snapshot().PID != origPID {
		t.Fatal("running process was bounced by a config change (must not be)")
	}

	// The change takes effect on the next restart.
	s.Restart()
	waitState(t, s, core.StateRunning)
	if s.Snapshot().ConfigChanged {
		t.Fatal("ConfigChanged should clear once the change is applied")
	}
	if s.Snapshot().PID == origPID {
		t.Fatal("restart should have replaced the process")
	}
}

// ---- Idempotent Start ----------------------------------------------------

func TestStartIsIdempotent(t *testing.T) {
	s := newTestSupervisor(t, shHarness("idem", "while true; do sleep 0.02; done", 0),
		Policy{CrashWindow: time.Second, CrashThreshold: 3, MaxRestarts: 5, StopGrace: 200 * time.Millisecond})
	s.Start()
	waitState(t, s, core.StateRunning)
	pid := s.Snapshot().PID
	s.Start() // second start must not spawn a new process
	if s.Snapshot().PID != pid {
		t.Fatal("second Start replaced the running process")
	}
}

// Backoff Give-Up reachability + scheduled-harness exit handling
//
// Governing: SPEC-0003 REQ "Crash-Loop Detection", REQ "Backoff Give-Up";
// SPEC-0008 REQ "Firing And Overlap".
//
// Give-up used to be gated on `flapping`, which is only ever set for crashes
// fast enough to land inside CrashWindow (10s). A harness that failed reliably
// but slowly reset the crash counter on every run, never tripped flapping, and
// so restarted forever — two scheduled sweeps did exactly that 6,212 times,
// reporting `flapping no` throughout, until they exhausted the model
// provider's weekly quota.
//
// @joestump 08/22/2026 - Added with the consecFailures fix.

package supervisor

import (
	"path/filepath"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// scheduledHarness builds a cron one-shot.
func scheduledHarness(name, script, spec string) core.Harness {
	h := shHarnessWithRestart(name, script, time.Millisecond, core.RestartOnFailure)
	h.Schedule = spec
	return h
}

// waitUntil polls cond, returning whether it became true before the deadline.
func waitUntil(timeout time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(2 * time.Millisecond)
	}
	return false
}

// The regression: every run outlives CrashWindow, so the flap counter reset
// each time. Give-up must still be reached.
func TestSlowReliableFailureGivesUp(t *testing.T) {
	p := Policy{
		CrashWindow:    20 * time.Millisecond, // run below is 3x longer
		CrashThreshold: 3,
		BackoffBase:    2 * time.Millisecond,
		BackoffCap:     10 * time.Millisecond,
		MaxRestarts:    5,
		StopGrace:      80 * time.Millisecond,
	}
	s := newTestSupervisor(t, shHarnessWithRestart("slowfail", "sleep 0.06; exit 1", time.Millisecond, core.RestartOnFailure), p)
	s.Start()

	if !waitUntil(3*time.Second, func() bool { return s.Snapshot().State == core.StateFailed }) {
		snap := s.Snapshot()
		t.Fatalf("never gave up: restarts=%d flapping=%v state=%s (MaxRestarts=%d)",
			snap.RestartCount, snap.Flapping, snap.State, p.MaxRestarts)
	}
	if n := s.Snapshot().RestartCount; n > p.MaxRestarts+2 {
		t.Errorf("gave up after %d restarts, want ~%d", n, p.MaxRestarts)
	}
}

// A run that lasts HealthyRun came up successfully, so it clears the failure
// budget: a service that works for a long stretch and then dies must never
// accumulate toward give-up across unrelated crashes.
func TestHealthyRunClearsFailureBudget(t *testing.T) {
	p := Policy{
		CrashWindow:    time.Millisecond, // HealthyRun derives to 30ms
		CrashThreshold: 1000,
		BackoffBase:    time.Millisecond,
		BackoffCap:     2 * time.Millisecond,
		MaxRestarts:    3,
		StopGrace:      80 * time.Millisecond,
	}
	s := newTestSupervisor(t, shHarnessWithRestart("healthy", "sleep 0.09; exit 1", time.Millisecond, core.RestartOnFailure), p)
	s.Start()

	// Well past MaxRestarts worth of runs; each one is healthy before it dies.
	if !waitUntil(2*time.Second, func() bool { return s.Snapshot().RestartCount > p.MaxRestarts+1 }) {
		t.Fatalf("expected repeated restarts, got %d", s.Snapshot().RestartCount)
	}
	if st := s.Snapshot().State; st == core.StateFailed {
		t.Fatalf("parked a harness whose every run was healthy before dying (state=%s)", st)
	}
}

// A clean exit clears the budget, so alternating success and failure never
// walks into give-up.
func TestCleanExitClearsFailureBudget(t *testing.T) {
	p := Policy{
		CrashWindow:    time.Millisecond,
		CrashThreshold: 1000,
		BackoffBase:    time.Millisecond,
		BackoffCap:     2 * time.Millisecond,
		MaxRestarts:    2,
		StopGrace:      80 * time.Millisecond,
	}
	// Fails, then succeeds, forever — never two failures in a row. The marker
	// path is fixed for the test (not $$) so it persists across runs.
	marker := filepath.Join(t.TempDir(), "alt")
	script := `if [ -f ` + marker + ` ]; then rm -f ` + marker + `; exit 0; else : > ` + marker + `; exit 1; fi`
	s := newTestSupervisor(t, shHarnessWithRestart("alt", script, time.Millisecond, core.RestartAlways), p)
	s.Start()

	if !waitUntil(2*time.Second, func() bool { return s.Snapshot().RestartCount > p.MaxRestarts+2 }) {
		t.Fatalf("expected repeated restarts, got %d", s.Snapshot().RestartCount)
	}
	if st := s.Snapshot().State; st == core.StateFailed {
		t.Fatalf("parked a harness that never failed twice in a row (state=%s)", st)
	}
}

// A scheduled harness is a cron one-shot: the schedule is its retry mechanism,
// so a failed run must land and wait rather than respawn.
func TestScheduledFailureDoesNotRespawn(t *testing.T) {
	s := newTestSupervisor(t, scheduledHarness("sched-fail", "exit 1", "0 */6 * * *"), fastPolicy())
	s.Start()

	if !waitUntil(3*time.Second, func() bool { return s.Snapshot().State == core.StateFailed }) {
		t.Fatalf("expected failed, got %s", s.Snapshot().State)
	}
	before := s.Snapshot().RestartCount
	time.Sleep(300 * time.Millisecond) // many restart_delays worth
	if after := s.Snapshot().RestartCount; after != before {
		t.Fatalf("scheduled one-shot respawned on failure: restarts %d -> %d", before, after)
	}
	if st := s.Snapshot().State; st != core.StateFailed {
		t.Fatalf("state drifted off failed: %s", st)
	}
}

// The clean-exit half: a scheduled run that succeeds parks in stopped and
// waits for its next firing.
func TestScheduledCleanExitStopsAndWaits(t *testing.T) {
	s := newTestSupervisor(t, scheduledHarness("sched-ok", "exit 0", "0 */6 * * *"), fastPolicy())
	s.Start()

	if !waitUntil(3*time.Second, func() bool { return s.Snapshot().State == core.StateStopped }) {
		t.Fatalf("expected stopped, got %s", s.Snapshot().State)
	}
	before := s.Snapshot().RestartCount
	time.Sleep(300 * time.Millisecond)
	if after := s.Snapshot().RestartCount; after != before {
		t.Fatalf("scheduled one-shot respawned after a clean exit: restarts %d -> %d", before, after)
	}
}

// A scheduled harness's RestartCount can never increase — the branch above
// returns before the increment, on every exit. That makes any non-zero value
// a permanent relic of a window when the harness did respawn (before it gained
// a schedule, or before that branch existed), and nothing else clears it:
// cmdRestart resets the flap counters and leaves this one alone.
//
// The 6,212-restart loop this file was written for left exactly that behind —
// two sweeps still reporting 4417 and 1795 in `harness list` days after the
// fix, which is the first number an operator reads when triaging them.
func TestScheduledFiringClearsStaleRestartCount(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
		want   core.State
	}{
		{"failed firing", "exit 1", core.StateFailed},
		{"clean firing", "exit 0", core.StateStopped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestSupervisor(t, scheduledHarness("sweeper", tc.script, "0 */6 * * *"), fastPolicy())
			// Seed the relic the way a daemon restart does (ADR-0007).
			s.Restore(false, 4417, 1, time.Time{})
			if got := s.Snapshot().RestartCount; got != 4417 {
				t.Fatalf("precondition: RestartCount = %d, want the seeded 4417", got)
			}

			s.Start()
			if !waitUntil(3*time.Second, func() bool { return s.Snapshot().State == tc.want }) {
				t.Fatalf("expected %s, got %s", tc.want, s.Snapshot().State)
			}
			if got := s.Snapshot().RestartCount; got != 0 {
				t.Errorf("RestartCount = %d after a firing, want 0 — a scheduled "+
					"harness cannot accumulate restarts, so a stale count is "+
					"reported to the operator forever", got)
			}
		})
	}
}

// The counterpart: an ordinary always-on harness keeps its restart history.
// It is real — those restarts happened, the counter can still move, and the
// #99 fallback contract preserves it across a daemon restart.
func TestUnscheduledHarnessKeepsRestartHistory(t *testing.T) {
	s := newTestSupervisor(t, shHarnessWithRestart("worker", "exit 0", time.Millisecond, core.RestartNo), fastPolicy())
	s.Restore(false, 7, 0, time.Time{})

	s.Start()
	if !waitUntil(3*time.Second, func() bool { return s.Snapshot().State == core.StateStopped }) {
		t.Fatalf("expected stopped, got %s", s.Snapshot().State)
	}
	if got := s.Snapshot().RestartCount; got != 7 {
		t.Errorf("RestartCount = %d, want 7 preserved — an unscheduled harness's "+
			"restart history is real and must survive", got)
	}
}

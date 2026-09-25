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

	"github.com/stump-wtf/harness/internal/core"
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
			s.Restore(false, 4417, 1, time.Time{}, time.Time{})
			if got := s.Snapshot().RestartCount; got != 4417 {
				t.Fatalf("precondition: RestartCount = %d, want the seeded 4417", got)
			}

			// StartTransient, not Start: this is what cmd/harness/daemon.go
			// hands the scheduler, and it is the entry point that leaves
			// enabled == false. Testing through Start would set enabled = true
			// and exercise a fall-through the scheduler never takes.
			s.StartTransient()
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
	s.Restore(false, 7, 0, time.Time{}, time.Time{})

	s.Start()
	if !waitUntil(3*time.Second, func() bool { return s.Snapshot().State == core.StateStopped }) {
		t.Fatalf("expected stopped, got %s", s.Snapshot().State)
	}
	if got := s.Snapshot().RestartCount; got != 7 {
		t.Errorf("RestartCount = %d, want 7 preserved — an unscheduled harness's "+
			"restart history is real and must survive", got)
	}
}

// The ordering invariant in onProcessGone, pinned directly: a scheduled
// harness's exit must be classified by its schedule, not by the transient
// intent StartTransient leaves behind. With the !s.enabled return ahead of
// the Schedule branch, a firing that exits non-zero settled as StateStopped
// and was indistinguishable from a clean one — the sweep that failed and the
// sweep that succeeded rendered identically in `harness list`.
func TestScheduledFailedFiringLandsInFailed(t *testing.T) {
	s := newTestSupervisor(t, scheduledHarness("sweeper", "exit 1", "0 */6 * * *"), fastPolicy())
	s.StartTransient()

	if !waitUntil(3*time.Second, func() bool { return s.Snapshot().State == core.StateFailed }) {
		t.Fatalf("a scheduled firing that exited 1 settled as %s, want failed — "+
			"a failed sweep must not read as a clean one", s.Snapshot().State)
	}
}

// The counterpart the reorder had to preserve: `harness stop` on a RUNNING
// scheduled harness still lands in stopped, despite the SIGTERM exit being
// non-zero. It holds because a graceful stop never reaches onProcessGone at
// all — gracefulStop consumes the exit off exitCh itself and transitions
// directly — so the Schedule branch cannot misroute an operator's stop into
// failed. This test exists to fail loudly if that ever stops being true.
func TestStoppingRunningScheduledHarnessLandsInStopped(t *testing.T) {
	s := newTestSupervisor(t, scheduledHarness("sweeper", "sleep 30", "0 */6 * * *"), fastPolicy())
	s.StartTransient()
	if !waitUntil(3*time.Second, func() bool { return s.Snapshot().State == core.StateRunning }) {
		t.Fatalf("sweep never came up: %s", s.Snapshot().State)
	}

	s.Stop()
	if !waitUntil(3*time.Second, func() bool { return s.Snapshot().State == core.StateStopped }) {
		t.Fatalf("harness stop on a running sweep landed in %s, want stopped — "+
			"an operator's stop is not a failure", s.Snapshot().State)
	}
}

// The snapshot must carry the loop's give-up accounting, so a harness walking
// its budget toward `failed` is visible before it arrives (SPEC-0013 REQ-2,
// harness_consecutive_failures). The value at give-up is the one the loop
// compared against MaxRestarts; a snapshot that forgot to copy the field
// reads 0 here and fails.
//
// @joestump-agent 09/21/2026 - Added with Snapshot.ConsecutiveFailures.
func TestSnapshotReportsConsecutiveFailures(t *testing.T) {
	p := Policy{
		CrashWindow:    time.Millisecond,
		CrashThreshold: 1000,
		BackoffBase:    time.Millisecond,
		BackoffCap:     2 * time.Millisecond,
		MaxRestarts:    4,
		StopGrace:      80 * time.Millisecond,
	}
	s := newTestSupervisor(t, shHarnessWithRestart("walker", "exit 1", time.Millisecond, core.RestartOnFailure), p)
	s.Start()

	seen := map[int]bool{}
	if !waitUntil(3*time.Second, func() bool {
		snap := s.Snapshot()
		seen[snap.ConsecutiveFailures] = true
		return snap.State == core.StateFailed
	}) {
		t.Fatalf("never gave up: %+v", s.Snapshot())
	}
	if got, want := s.Snapshot().ConsecutiveFailures, p.MaxRestarts+1; got != want {
		t.Errorf("ConsecutiveFailures at give-up = %d, want %d (the count that exceeded MaxRestarts)", got, want)
	}
	// It climbed rather than jumping: at least one intermediate value was
	// visible on the way, which is the whole point of exposing it.
	intermediate := false
	for n := 1; n <= p.MaxRestarts; n++ {
		if seen[n] {
			intermediate = true
		}
	}
	if !intermediate {
		t.Errorf("no intermediate ConsecutiveFailures value observed on the way to give-up: %v", seen)
	}

	// A deliberate start resets the budget (clearFailLatch), and the snapshot
	// must say so.
	s.Start()
	if !waitUntil(time.Second, func() bool { return s.Snapshot().ConsecutiveFailures < p.MaxRestarts+1 }) {
		t.Errorf("ConsecutiveFailures stayed %d after a deliberate start", s.Snapshot().ConsecutiveFailures)
	}
}

// A harness that failed and then came up and stayed up has, by the
// supervisor's own definition, come up successfully once its run passes
// HealthyRun — the next exit would clear the count. The snapshot must say
// so while the run is still going, not only when it ends: otherwise a
// harness that recovered reports its old failures for as long as it keeps
// running, and harness_consecutive_failures >= N (SPEC-0013 REQ-2) fires
// for a week on a healthy harness.
//
// @joestump-agent 09/23/2026 - Added in review (harness#589).
//
// @joestump-agent 09/24/2026 - Judge HealthyRun through snapshotAt instead
// of the wall clock. With a 300ms HealthyRun, a poller starved past 300ms
// under -race never saw the count at 2 and failed a correct supervisor (CI
// run 13224). HealthyRun is now an hour, so the real clock can never clear
// the count mid-poll, and the test picks the instants either side of it.
func TestSnapshotClearsConsecutiveFailuresOnceTheRunIsHealthy(t *testing.T) {
	p := Policy{
		CrashWindow:    time.Millisecond,
		CrashThreshold: 1000,
		BackoffBase:    time.Millisecond,
		BackoffCap:     2 * time.Millisecond,
		HealthyRun:     time.Hour,
		MaxRestarts:    10,
		StopGrace:      80 * time.Millisecond,
	}
	count := filepath.Join(t.TempDir(), "runs")
	// Fail twice, then stay up.
	script := `n=$(cat ` + count + ` 2>/dev/null || echo 0); n=$((n+1)); echo $n > ` + count + `; [ $n -le 2 ] && exit 1; exec sleep 30`
	s := newTestSupervisor(t, shHarnessWithRestart("recovers", script, time.Millisecond, core.RestartOnFailure), p)
	s.Start()

	// The third run is up, carrying the two failures before it.
	var live Snapshot
	if !waitUntil(3*time.Second, func() bool {
		live = s.Snapshot()
		return live.State == core.StateRunning && live.PID != 0 && live.ConsecutiveFailures == 2
	}) {
		t.Fatalf("never reached a live run after two failures: %+v", s.Snapshot())
	}

	// Up for exactly HealthyRun is not yet healthy: the rule is strict.
	if got := s.snapshotAt(live.LastStarted.Add(p.HealthyRun)).ConsecutiveFailures; got != 2 {
		t.Fatalf("ConsecutiveFailures = %d at exactly HealthyRun; want 2", got)
	}
	// Past HealthyRun the run has come up successfully, and the snapshot
	// says so while that same run is still going — not only at its exit.
	healthy := s.snapshotAt(live.LastStarted.Add(p.HealthyRun + time.Nanosecond))
	if healthy.ConsecutiveFailures != 0 {
		t.Fatalf("ConsecutiveFailures = %d after the run outlived HealthyRun; want 0", healthy.ConsecutiveFailures)
	}
	if healthy.State != core.StateRunning || healthy.PID != live.PID {
		t.Fatalf("state %s, PID %d; want the same run (PID %d) still running — the reset must come from a healthy run, not an exit",
			healthy.State, healthy.PID, live.PID)
	}
	// The clear is the snapshot's reading, not the loop's: the loop still
	// holds the streak until the run exits.
	if got := s.Snapshot().ConsecutiveFailures; got != 2 {
		t.Fatalf("wall-clock Snapshot ConsecutiveFailures = %d; want 2 (the run is minutes short of HealthyRun)", got)
	}
}

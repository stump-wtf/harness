// Backoff escalation reachability: the restart delay must climb to
// BackoffCap across a real run of crashes, not just in the formula.
//
// Governing: SPEC-0003 REQ "Crash-Loop Detection" ("subsequent restart
// delays escalate exponentially up to the cap").
//
// flapAttempts used to increment only when CrashThreshold exits landed
// inside CrashWindow. The delay is BackoffBase * 2^(attempt-1), so once it
// passes CrashWindow/2 three exits can no longer fit inside the window, the
// cluster stops tripping, the counter freezes, and the delay plateaus below
// the cap forever — a flapping harness in the #315 repro logged
// `next_retry_in=8s` indefinitely. This test drives the plateau shape at the
// supervisor level (crash exits spaced further apart than the window) and
// asserts the delay actually reaches BackoffCap.
//
// @joestump-agent 10/24/2026 - Added with the #335 fix.

package supervisor

import (
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/testwait"
)

// TestFlapBackoffEscalatesToCap fails on the frozen-counter behavior: with a
// cap wider than the window can produce from in-window clusters alone, the
// plateau sits strictly below BackoffCap and the wait below never succeeds.
func TestFlapBackoffEscalatesToCap(t *testing.T) {
	p := Policy{
		CrashWindow:    200 * time.Millisecond,
		CrashThreshold: 3,
		BackoffBase:    5 * time.Millisecond,
		// Above what CrashWindow/2 escalation can reach from cluster trips
		// alone (attempts freeze at 6 → 160ms), so the cap is only observable
		// if the counter keeps climbing between clusters.
		BackoffCap:  200 * time.Millisecond,
		MaxRestarts: 1000, // give-up is SPEC-0003's other REQ; #315 covers it
		StopGrace:   100 * time.Millisecond,
	}
	s := newTestSupervisor(t, shHarnessWithRestart("plateau", "exit 1", time.Millisecond, core.RestartOnFailure), p)
	s.Start()

	flapped := false
	var maxDelay time.Duration
	deadline := time.Now().Add(testwait.Budget(t, 4*time.Second))
	for time.Now().Before(deadline) {
		snap := s.Snapshot()
		if snap.Flapping {
			flapped = true
			if snap.NextRetryIn > maxDelay {
				maxDelay = snap.NextRetryIn
			}
			if maxDelay >= p.BackoffCap {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	snap := s.Snapshot()
	if !flapped {
		t.Fatalf("harness never flagged flapping: restarts=%d state=%s", snap.RestartCount, snap.State)
	}
	t.Fatalf("backoff plateaued at %v, want it to escalate to cap %v (restarts=%d flapping=%v) — the flap counter froze because clusters stopped forming once the delay outgrew the window",
		maxDelay, p.BackoffCap, snap.RestartCount, snap.Flapping)
}

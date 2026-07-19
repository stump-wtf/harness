package supervisor

// Governing: SPEC-0003 (harness-lifecycle) REQ "Crash-Loop Detection" and
// "Backoff Give-Up"; ADR-0005 (capped-exponential backoff instead of today's
// fixed `sleep $HR_DELAY` loop). Policy is the one place the crash-loop
// thresholds, the backoff curve, and the stop grace period are defined, so the
// supervisor goroutine stays pure mechanism.

import "time"

// Policy bundles the tunables that govern restart timing, crash-loop
// detection, backoff escalation, give-up, and graceful stop. The daemon builds
// one Policy (typically DefaultPolicy) and shares it across supervisors; tests
// shrink the durations to run the whole machine in milliseconds.
type Policy struct {
	// CrashWindow (T) is the observation window for crash-loop detection. A
	// single run that survives longer than CrashWindow resets the crash
	// counter (SPEC-0003 REQ "Crash-Loop Detection").
	CrashWindow time.Duration
	// CrashThreshold (N) is how many exits within CrashWindow tip a harness
	// into `degraded`/flapping.
	CrashThreshold int

	// BackoffBase is the first flapping restart delay; each subsequent
	// flapping restart doubles it up to BackoffCap.
	BackoffBase time.Duration
	// BackoffCap is the ceiling for the capped-exponential backoff.
	BackoffCap time.Duration

	// MaxRestarts is the number of consecutive flapping restart attempts the
	// daemon makes before giving up and parking the harness in `failed`
	// (SPEC-0003 REQ "Backoff Give-Up"). Zero or negative means "never give
	// up".
	MaxRestarts int

	// StopGrace is how long a `stopping` harness has to exit on SIGTERM before
	// the daemon escalates to SIGKILL (SPEC-0003 REQ "Graceful Stop").
	StopGrace time.Duration
}

// DefaultPolicy returns production-sane lifecycle tunables: 3 crashes within
// 10s is a loop, backoff runs 1s→30s, we give up after 5 flapping attempts,
// and a stop waits 10s before SIGKILL.
func DefaultPolicy() Policy {
	return Policy{
		CrashWindow:    10 * time.Second,
		CrashThreshold: 3,
		BackoffBase:    1 * time.Second,
		BackoffCap:     30 * time.Second,
		MaxRestarts:    5,
		StopGrace:      10 * time.Second,
	}
}

// normalize fills in safe fallbacks for any zero fields so a partially
// configured Policy still behaves.
func (p Policy) normalize() Policy {
	if p.CrashWindow <= 0 {
		p.CrashWindow = 10 * time.Second
	}
	if p.CrashThreshold <= 0 {
		p.CrashThreshold = 3
	}
	if p.BackoffBase <= 0 {
		p.BackoffBase = 1 * time.Second
	}
	if p.BackoffCap <= 0 {
		p.BackoffCap = 30 * time.Second
	}
	if p.BackoffCap < p.BackoffBase {
		p.BackoffCap = p.BackoffBase
	}
	if p.StopGrace <= 0 {
		p.StopGrace = 10 * time.Second
	}
	return p
}

// backoff returns the flapping restart delay for the attempt-th consecutive
// flapping restart (attempt is 1-based). It is a capped exponential:
// BackoffBase * 2^(attempt-1), clamped to BackoffCap (SPEC-0003 REQ
// "Crash-Loop Detection"; ADR-0005).
func (p Policy) backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	d := p.BackoffBase
	for i := 1; i < attempt; i++ {
		d *= 2
		if d >= p.BackoffCap {
			return p.BackoffCap
		}
	}
	if d > p.BackoffCap {
		return p.BackoffCap
	}
	return d
}

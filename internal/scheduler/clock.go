package scheduler

// Clock Seam
//
// Every read of the current time and every timer the scheduler holds goes
// through Clock, so the temporal scenarios SPEC-0008 names — a suspend across
// several windows, a DST transition, the clock stepping backwards — run in
// milliseconds against a fake instead of in real time. It follows the
// supervisor.Policy precedent: production values, substituted in tests.
//
// This is the only file in the package allowed to touch the time package's
// clock or timers; TestNoWallClockReadsOutsideClock enforces it.
//
// Governing: ADR-0013, SPEC-0008 REQ "Suspend-Safe Schedule Evaluation".
//
// @joestump-agent 09/11/2026 - Added for issue #117.

import "time"

// Clock supplies the wall clock and the scheduler's heartbeat ticker.
type Clock interface {
	// Now returns the current wall-clock time.
	Now() time.Time
	// NewTicker returns a channel that delivers roughly every d, and a func
	// that stops it.
	NewTicker(d time.Duration) (ticks <-chan time.Time, stop func())
}

// realClock is the production Clock.
type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) NewTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

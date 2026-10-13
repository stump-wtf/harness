package channel

// Backoff tests. The jitter direction and the reset condition are the two
// things worth pinning, because both are promises made elsewhere: "retried no
// more often than once every 5 minutes" is broken by downward jitter, and
// "resets once a stream has stayed open for 5 minutes" is broken by resetting
// on a successful connect.
//
// Governing: ADR-0021; SPEC-0014 REQ "Channel Reconnection".
//
// @joestump 09/23/2026 - Introduced with SPEC-0014 channel reconnection (#474).

import (
	"testing"
	"time"
)

// fixed returns a Rand that always answers r, so a delay is exact.
func fixed(r float64) func() float64 { return func() float64 { return r } }

func TestBackoffDoublesToTheCeiling(t *testing.T) {
	b := &Backoff{Rand: fixed(0)}
	want := []time.Duration{
		1 * time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second,
		16 * time.Second, 32 * time.Second, 64 * time.Second, 128 * time.Second,
		256 * time.Second, 5 * time.Minute, 5 * time.Minute, 5 * time.Minute,
	}
	for i, w := range want {
		if got := b.Next(); got != w {
			t.Errorf("delay %d = %s, want %s", i+1, got, w)
		}
	}
	// And it stays there rather than overflowing into something absurd: the
	// shift is capped, so a source down for a week still retries every five
	// minutes.
	for i := 0; i < 200; i++ {
		b.Next()
	}
	if got := b.Next(); got != BackoffCeiling {
		t.Errorf("delay after 200 more failures = %s, want the ceiling", got)
	}
}

// TestJitterIsUpwardOnly is the guard on the promise REQ "Channel
// Reconnection" makes about an authentication failure: retried "no more often
// than once every 5 minutes". Downward jitter would break that at the ceiling,
// which is exactly where the promise applies.
func TestJitterIsUpwardOnly(t *testing.T) {
	for _, r := range []float64{0, 0.001, 0.5, 0.999} {
		b := &Backoff{Rand: fixed(r)}
		if got := b.Ceiling(); got < BackoffCeiling {
			t.Errorf("Ceiling() with rand %v = %s, below the %s floor", r, got, BackoffCeiling)
		}
		b2 := &Backoff{Rand: fixed(r)}
		if got := b2.Next(); got < BackoffBase {
			t.Errorf("Next() with rand %v = %s, below the %s base", r, got, BackoffBase)
		}
	}
	// It does spread, though — a fleet that lost one server must not return
	// to it in lockstep.
	lo := (&Backoff{Rand: fixed(0)}).Ceiling()
	hi := (&Backoff{Rand: fixed(0.99)}).Ceiling()
	if hi <= lo {
		t.Errorf("no jitter is applied: rand 0 and rand 0.99 both give %s", lo)
	}
}

// TestCeilingDoesNotEscalate: an `error` source is retried at the ceiling
// without advancing the schedule, so a transient failure afterwards still
// starts from the base rather than inheriting the error's escalation.
func TestCeilingDoesNotEscalate(t *testing.T) {
	b := &Backoff{Rand: fixed(0)}
	for i := 0; i < 5; i++ {
		b.Ceiling()
	}
	if b.Attempts() != 0 {
		t.Errorf("Attempts after 5 Ceiling calls = %d, want 0", b.Attempts())
	}
	if got := b.Next(); got != BackoffBase {
		t.Errorf("the first Next after Ceiling calls = %s, want the base", got)
	}
}

// TestResetNeedsAStreamThatStayedOpen is the other half of the flapping-link
// guard. Resetting on a successful connect alone would turn a link that
// connects and dies in two seconds into a tight reconnect loop.
func TestResetNeedsAStreamThatStayedOpen(t *testing.T) {
	b := &Backoff{Rand: fixed(0)}
	for i := 0; i < 6; i++ {
		b.Next()
	}
	escalated := b.Next()

	b.ResetAfter(2 * time.Second)
	if got := b.Next(); got <= escalated {
		t.Errorf("a two-second connection reset the backoff: next delay %s", got)
	}
	b.ResetAfter(BackoffResetAfter)
	if got := b.Next(); got != BackoffBase {
		t.Errorf("a %s connection did not reset the backoff: next delay %s", BackoffResetAfter, got)
	}
}

// TestResetAccumulatesConsecutiveHealthyStreams is the regression guard for a
// proxy that cancels idle streams at ~125 s. No single stream reaches the
// 5-minute mark, and the old reset never fired, so the delay ratcheted to the
// ceiling and the channel was down most of the time; the same streams,
// counted together, must bring the base delay back.
func TestResetAccumulatesConsecutiveHealthyStreams(t *testing.T) {
	b := &Backoff{Rand: fixed(0)}
	for i := 0; i < 6; i++ {
		b.Next()
	}
	escalated := b.Next()

	// Two 125-second streams: 250 s of credit, short of the 5-minute mark.
	b.ResetAfter(125 * time.Second)
	b.ResetAfter(125 * time.Second)
	if got := b.Next(); got <= escalated {
		t.Errorf("two healthy streams reset the backoff early: next delay %s", got)
	}
	// The third crosses it, and the delay comes home.
	b.ResetAfter(125 * time.Second)
	if got := b.Next(); got != BackoffBase {
		t.Errorf("three healthy 125 s streams did not reset the backoff: next delay %s", got)
	}
}

// TestHardFailureClearsTheCredit: a failure that never opened a stream breaks
// the run of consecutive healthy streams, so the credit the healthy streams
// earned does not carry through a real outage.
func TestHardFailureClearsTheCredit(t *testing.T) {
	b := &Backoff{Rand: fixed(0)}
	for i := 0; i < 6; i++ {
		b.Next()
	}
	escalated := b.Next()

	b.ResetAfter(125 * time.Second)
	// A hard failure: nothing opened.
	b.ResetAfter(0)
	// Two more healthy streams: 250 s of fresh credit, still short.
	b.ResetAfter(125 * time.Second)
	b.ResetAfter(125 * time.Second)
	if got := b.Next(); got <= escalated {
		t.Errorf("the credit survived a hard failure: next delay %s", got)
	}
	b.ResetAfter(125 * time.Second)
	if got := b.Next(); got != BackoffBase {
		t.Errorf("the credit rebuilt after the outage did not reset the backoff: next delay %s", got)
	}
}

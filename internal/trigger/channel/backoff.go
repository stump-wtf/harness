package channel

// Reconnection backoff.
//
// Two properties matter beyond "wait longer each time". The jitter is
// UPWARD-only, because REQ "Channel Reconnection" promises an authentication
// failure is retried "no more often than once every 5 minutes" — downward
// jitter would break that promise at the ceiling, which is exactly where the
// promise applies. And the reset is driven by how long a stream STAYED OPEN,
// not by a successful connect: a session that connects and dies in two seconds
// has not recovered, and resetting on connect alone turns a flapping link into
// a tight reconnect loop.
//
// The 5-minute mark is reached by ACCUMULATING open time across consecutive
// healthy streams. A proxy that cancels an idle stream after ~125 s never
// hands a single stream 5 minutes, and a threshold on one stream alone would
// ratchet the delay to the ceiling and leave the channel down most of the
// time; the same streams, counted together, are a link that is repeatedly
// healthy and deserves the base delay back. A single stream open for 5
// minutes still resets on its own, as REQ "Channel Reconnection" promises,
// and a tight flap (a stream that opens and dies in seconds) accrues only a
// few seconds of credit per cycle, so the ladder still climbs to the ceiling
// and stays there.
//
// Governing: ADR-0021; SPEC-0014 REQ "Channel Reconnection".
//
// @joestump 09/23/2026 - Introduced with SPEC-0014 channel reconnection (#474).
// @joestump-agent 10/13/2026 - Reset now accumulates open time across consecutive healthy streams (harness#38 on GitHub); a failure that never opened a stream clears the credit.

import (
	"math/rand/v2"
	"time"
)

const (
	// BackoffBase is the first delay after a failure.
	BackoffBase = time.Second
	// BackoffCeiling is the longest delay, and the rate an `error` source is
	// retried at.
	BackoffCeiling = 5 * time.Minute
	// BackoffResetAfter is how much open time, accumulated across consecutive
	// streams that stayed open, takes the delay back to BackoffBase.
	BackoffResetAfter = 5 * time.Minute
	// backoffJitter is the fraction added on top of a delay, spread over
	// [0, backoffJitter]. Upward only — see the file comment.
	backoffJitter = 0.25
)

// Backoff computes reconnection delays.
//
// It is not safe for concurrent use: one session owns one Backoff, on the one
// goroutine that runs its connect loop.
type Backoff struct {
	// Rand returns a value in [0, 1). Defaults to the global source; a test
	// pins it to make a delay exact.
	Rand func() float64

	attempts int
	// credit is the open time accumulated across consecutive streams that
	// stayed open, counting toward BackoffResetAfter. A failure that never
	// opened a stream clears it.
	credit time.Duration
}

// Next returns the delay after a failure, and advances the escalation.
func (b *Backoff) Next() time.Duration {
	d := BackoffBase << min(b.attempts, 16)
	if d > BackoffCeiling || d <= 0 {
		d = BackoffCeiling
	}
	b.attempts++
	return b.jitter(d)
}

// Ceiling returns a jittered ceiling delay WITHOUT advancing the escalation.
// It is the rate a source in `error` is retried at: a wrong credential or a
// server that is not a channel server will not be fixed by trying sooner, and
// hammering an auth endpoint is its own problem.
func (b *Backoff) Ceiling() time.Duration { return b.jitter(BackoffCeiling) }

// ResetAfter returns the escalation to the start once a stream has shown
// enough health to count as a recovery.
//
// `open` is the duration the stream was READ for, not the time since the
// connect attempt: a session that connects and dies in two seconds has not
// recovered, and resetting on a successful connect alone is what turns a
// flapping link into a tight reconnect loop.
//
// Open time ACCUMULATES across consecutive healthy streams, so a proxy that
// cuts idle streams at ~125 s gets the delay back within a few cycles instead
// of ratcheting it to the ceiling, where it would stay for good: no single
// stream ever reaches 5 minutes there, but the repeated 125-second streams
// are a link that is repeatedly healthy. A single stream open for 5 minutes
// still resets on its own. `open` of zero means the attempt never opened a
// stream — a hard failure — which breaks the chain and clears the credit the
// healthy streams earned.
func (b *Backoff) ResetAfter(open time.Duration) {
	if open <= 0 {
		b.credit = 0
		return
	}
	b.credit += open
	if b.credit >= BackoffResetAfter {
		b.attempts = 0
		b.credit = 0
	}
}

// Attempts is how many failures have escalated the delay. Exposed for tests
// and for a status line, not for control flow.
func (b *Backoff) Attempts() int { return b.attempts }

// jitter spreads a delay upward by up to backoffJitter, so a fleet of daemons
// that lost one server does not return to it in lockstep.
func (b *Backoff) jitter(d time.Duration) time.Duration {
	r := b.Rand
	if r == nil {
		r = rand.Float64
	}
	return d + time.Duration(float64(d)*backoffJitter*r())
}

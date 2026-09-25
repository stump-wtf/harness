package webhook

// The per-route token bucket: pipeline step 8.
//
// `rate_limit = "<n>/<unit>"` is a bucket of capacity n that refills n tokens
// per unit, continuously — so "2/m" allows a burst of two, then one more every
// 30 seconds. A delivery that finds no token is answered 429 with a
// Retry-After saying when the next one will be there.
//
// It is the LAST check before firing, so only a delivery that verified, passed
// `events` and was not a duplicate can spend a token. That ordering is the
// whole defense: a sender who knows the URL but not the secret cannot drain
// the budget and lock the real sender out (design.md "Webhook pipeline
// order").
//
// Not safe for concurrent use: the route's limits (limits.go) serialize it.
//
// Governing: ADR-0021; SPEC-0014 REQ "Webhook Rate Limit".
//
// @joestump 09/24/2026 - Introduced with de-duplication and rate limits (#460).

import (
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// tokenBucket enforces one route's rate_limit. A nil *tokenBucket is the
// `"0"` limit: every take succeeds.
//
// It is the token bucket in its GCRA form: instead of a fractional token
// count it keeps tat, the time at which the bucket would be full again, which
// makes every step integer arithmetic on durations. A float count drifts: 20 s
// into a 2/m refill, (1 - 2/3) x 30 s comes out a hair over 10 s, and
// Retry-After rounds that up to 11. Here it is exactly 10.
type tokenBucket struct {
	// perToken is how long one token takes to refill: the unit over n.
	perToken time.Duration
	// burst is how far tat may run ahead of now and still admit: n-1 tokens'
	// worth, so a full bucket admits n in the same instant.
	burst time.Duration
	// tat is when the bucket will have refilled everything spent so far.
	// Zero, or anything in the past, is a full bucket.
	tat time.Time
}

// newTokenBucket returns a full bucket for rl, or nil when rl is unlimited.
func newTokenBucket(rl core.RateLimit) *tokenBucket {
	if rl.Unlimited() || rl.Window <= 0 {
		return nil
	}
	per := rl.Window / time.Duration(rl.Count)
	return &tokenBucket{perToken: per, burst: per * time.Duration(rl.Count-1)}
}

// take spends one token if there is one. When there is not, retryAfter is how
// long until there will be.
//
// A clock that stepped backwards sees the bucket emptier, never fuller: tat
// does not move back, so the step cannot mint a burst.
func (b *tokenBucket) take(now time.Time) (ok bool, retryAfter time.Duration) {
	if b == nil {
		return true, 0
	}
	tat := b.tat
	if tat.Before(now) {
		tat = now
	}
	if ahead := tat.Sub(now); ahead > b.burst {
		return false, ahead - b.burst
	}
	b.tat = tat.Add(b.perToken)
	return true, 0
}

// retryAfterSeconds renders d as a Retry-After value: whole seconds, rounded
// up, and never less than 1, because "0" invites an immediate retry that
// fails.
func retryAfterSeconds(d time.Duration) int {
	secs := int((d + time.Second - 1) / time.Second)
	return max(secs, 1)
}

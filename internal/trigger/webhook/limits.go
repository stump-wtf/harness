package webhook

// A route's limits: de-duplication (step 7) and the rate limit (step 8), as
// one decision under one lock.
//
// They are one decision because the spec couples them. A duplicate must not
// spend a token, and a rate-limited delivery must not be remembered as seen —
// or the sender's retry of a 429 would come back `duplicate` and never fire.
// So the ID is recorded only once the token is spent, and check, spend and
// record happen together: two copies of one delivery racing in on separate
// connections get one firing and one `duplicate`, never two firings.
//
// A route's limits outlive its table. On reload, a route whose name and
// rate_limit are unchanged keeps the same *routeLimits (table.inherit), so a
// config rewrite neither refills a drained bucket nor forgets what fired.
//
// Governing: ADR-0021; SPEC-0014 REQ "Webhook Filtering", REQ "Webhook Rate
// Limit", REQ "Source Reconciliation On Reload", REQ "Concurrency Safety".
//
// @joestump 09/24/2026 - Introduced with de-duplication and rate limits (#460).

import (
	"sync"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// admission is what a route's limits decided for one delivery.
type admission int

const (
	// admitFire: fire it. A delivery ID it carried is now remembered.
	admitFire admission = iota
	// admitDuplicate: its delivery ID fired on this route already.
	admitDuplicate
	// admitRateLimited: the bucket is empty. Nothing was remembered.
	admitRateLimited
)

// routeLimits is one route's de-duplication set and token bucket.
type routeLimits struct {
	// rateLimit is what the bucket was built from; a reload that changes it
	// gets fresh limits.
	rateLimit core.RateLimit

	mu     sync.Mutex
	dedup  *dedupSet
	bucket *tokenBucket // nil for rate_limit = "0"
}

func newRouteLimits(rl core.RateLimit) *routeLimits {
	return &routeLimits{
		rateLimit: rl,
		dedup:     newDedupSet(DedupWindow, DedupCapacity),
		bucket:    newTokenBucket(rl),
	}
}

// admit applies de-duplication, then the rate limit, to a verified delivery
// that passed `events`. delivery is the raw delivery-header value, "" when the
// scheme has no delivery header or the delivery carried none — and then there
// is nothing to de-duplicate on. eventID is what the firing will report.
//
// For admitDuplicate, seenID is the event_id the original firing reported. For
// admitRateLimited, retryAfter is when the next token will be there.
func (l *routeLimits) admit(delivery, eventID string, now time.Time) (a admission, seenID string, retryAfter time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if delivery != "" {
		if id, dup := l.dedup.seen(delivery, now); dup {
			return admitDuplicate, id, 0
		}
	}
	if ok, wait := l.bucket.take(now); !ok {
		return admitRateLimited, "", wait
	}
	if delivery != "" {
		l.dedup.add(delivery, eventID, now)
	}
	return admitFire, "", 0
}

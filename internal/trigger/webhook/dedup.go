package webhook

// Delivery-ID de-duplication: pipeline step 7.
//
// A sender that did not see our 202 — a timeout, a proxy hiccup, an operator
// pressing "redeliver" — sends the same delivery again under the same ID. Each
// route remembers the IDs it fired for 24 hours, at most the last 1024, and
// answers a repeat `202 duplicate` without firing.
//
// The set is held in memory only. REQ "Webhook Filtering" does not require it
// to survive a restart, and a daemon that restarted has no run in flight for
// the redelivery to double anyway.
//
// It keys on a SHA-256 of the ID, not the ID. The ID is sender-controlled and
// may be as long as the header limit allows (64 KiB); 1024 of those per route
// would be 64 MiB an authenticated sender could make the daemon hold. A digest
// is 32 bytes whatever arrives.
//
// Not safe for concurrent use: the route's limits (limits.go) serialize it.
//
// Governing: ADR-0021; SPEC-0014 REQ "Webhook Filtering" (item 2).
//
// @joestump 09/24/2026 - Introduced with de-duplication and rate limits (#460).

import (
	"crypto/sha256"
	"time"
)

// The de-duplication bounds REQ "Webhook Filtering" fixes.
const (
	// DedupWindow is how long a fired delivery ID is remembered.
	DedupWindow = 24 * time.Hour
	// DedupCapacity is how many IDs a route remembers; the oldest goes first.
	DedupCapacity = 1024
)

type deliveryKey [sha256.Size]byte

// seenDelivery is one remembered ID.
type seenDelivery struct {
	key deliveryKey
	at  time.Time
	// eventID is what the firing reported as its event_id, so a duplicate's
	// 202 names the same event the sender's first attempt did — including a
	// daemon-made ID, when the sender's own was unusable as one.
	eventID string
}

// dedupSet is one route's remembered delivery IDs, oldest first.
type dedupSet struct {
	window   time.Duration
	capacity int
	byKey    map[deliveryKey]*seenDelivery
	order    []*seenDelivery
}

func newDedupSet(window time.Duration, capacity int) *dedupSet {
	return &dedupSet{window: window, capacity: capacity, byKey: map[deliveryKey]*seenDelivery{}}
}

func keyOf(delivery string) deliveryKey { return sha256.Sum256([]byte(delivery)) }

// seen reports whether delivery fired on this route within the window, and
// the event_id that firing reported.
func (d *dedupSet) seen(delivery string, now time.Time) (eventID string, dup bool) {
	e, ok := d.byKey[keyOf(delivery)]
	if !ok || !d.live(e, now) {
		return "", false
	}
	return e.eventID, true
}

// add remembers delivery as fired at now. Callers check seen first; an entry
// that is present but expired is replaced, not duplicated.
func (d *dedupSet) add(delivery, eventID string, now time.Time) {
	d.expire(now)
	k := keyOf(delivery)
	if old, ok := d.byKey[k]; ok {
		d.remove(old)
	}
	for len(d.order) >= d.capacity {
		d.remove(d.order[0])
	}
	e := &seenDelivery{key: k, at: now, eventID: eventID}
	d.byKey[k] = e
	d.order = append(d.order, e)
}

// live reports whether e is still inside the window. A clock that stepped
// backwards reads as "still live": refusing a redelivery is the safe side.
func (d *dedupSet) live(e *seenDelivery, now time.Time) bool {
	return now.Sub(e.at) < d.window
}

// expire drops the expired entries at the front. Entries are appended in
// clock order, so the first live one ends the scan.
func (d *dedupSet) expire(now time.Time) {
	n := 0
	for n < len(d.order) && !d.live(d.order[n], now) {
		delete(d.byKey, d.order[n].key)
		n++
	}
	if n > 0 {
		d.order = append(d.order[:0], d.order[n:]...)
	}
}

// remove drops e wherever it sits. O(capacity), which is 1024.
func (d *dedupSet) remove(e *seenDelivery) {
	delete(d.byKey, e.key)
	for i, o := range d.order {
		if o == e {
			d.order = append(d.order[:i], d.order[i+1:]...)
			return
		}
	}
}

// size is how many IDs are held, expired or not.
func (d *dedupSet) size() int { return len(d.order) }

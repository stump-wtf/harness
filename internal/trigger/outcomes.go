package trigger

// Per-source counters for the deliveries that end without a firing.
//
// An ignored, duplicate or rate-limited delivery makes no run record — REQ
// "Webhook Filtering" forbids one — so without a counter it would leave no
// trace at all, and an operator whose `events` list is missing the event they
// wanted would see nothing but silence. These are the trace: in memory,
// cumulative since the daemon started, keyed by source reference. The
// visibility and metrics stories surface them; this file only counts.
//
// Governing: ADR-0021; SPEC-0014 REQ "Webhook Filtering", REQ "Webhook Rate
// Limit", REQ "Trigger Visibility".
//
// @joestump 09/24/2026 - Introduced with de-duplication and rate limits (#460).
// @joestump 09/25/2026 - Outcome and its values now live in state.go, the
//   one vocabulary `harness triggers` also reports (#476); only the
//   counters stay here.

import "sync"

// OutcomeCounters counts non-firing outcomes per source reference. The zero
// value is ready to use and safe for concurrent use.
type OutcomeCounters struct {
	mu sync.Mutex
	m  map[string]map[Outcome]uint64
}

// Inc counts one o for source.
func (c *OutcomeCounters) Inc(source string, o Outcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]map[Outcome]uint64{}
	}
	per := c.m[source]
	if per == nil {
		per = map[Outcome]uint64{}
		c.m[source] = per
	}
	per[o]++
}

// Count is how many o source has had.
func (c *OutcomeCounters) Count(source string, o Outcome) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[source][o]
}

// Snapshot is a copy of every count, safe to keep.
func (c *OutcomeCounters) Snapshot() map[string]map[Outcome]uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]map[Outcome]uint64, len(c.m))
	for src, per := range c.m {
		cp := make(map[Outcome]uint64, len(per))
		for o, n := range per {
			cp[o] = n
		}
		out[src] = cp
	}
	return out
}

package trigger

// Per-source counters: how each delivery or doorbell ended, when the source
// last fired, and how often a channel reconnected.
//
// An ignored, duplicate or rate-limited delivery makes no run record — REQ
// "Webhook Filtering" forbids one — so without a counter it would leave no
// trace at all, and an operator whose `events` list is missing the event they
// wanted would see nothing but silence. These are the trace: in memory,
// cumulative since the daemon started, keyed by source reference.
//
// One instance is shared by everything that counts — the source manager owns
// it, the webhook listener is handed it, the metrics collector and the
// visibility surfaces read it — so a delivery is counted once, in one place,
// whichever surface an operator looks at. The daemon wires that sharing; a
// component built without one gets its own and counts in isolation.
//
// Governing: ADR-0021; SPEC-0014 REQ "Webhook Filtering", REQ "Webhook Rate
// Limit", REQ "Trigger Visibility", REQ "Trigger Metrics".
//
// @joestump 09/24/2026 - Introduced with de-duplication and rate limits (#460).
// @joestump 09/24/2026 - Every REQ "Trigger Metrics" outcome, the last firing,
// channel reconnects, and Retain for sources a reload removed (#480).

import (
	"sync"
	"time"
)

// Outcome is how one delivery or doorbell ended. The set is closed: it is the
// `outcome` label of harness_trigger_events_total, and a label value that
// could come from anywhere else would be unbounded cardinality.
type Outcome string

const (
	// OutcomeFired: the event reached the fan-out to the bound harnesses.
	OutcomeFired Outcome = "fired"
	// OutcomeIgnored: the `events` allowlist did not list the event, or no
	// harness binds the source any more (a reload landed mid-delivery).
	OutcomeIgnored Outcome = "ignored"
	// OutcomeDuplicate: the delivery ID fired on this route already.
	OutcomeDuplicate Outcome = "duplicate"
	// OutcomeUnauthorized: the delivery failed verification (a 401).
	OutcomeUnauthorized Outcome = "unauthorized"
	// OutcomeTooLarge: the body was over the route's max_body (a 413).
	OutcomeTooLarge Outcome = "too_large"
	// OutcomeRateLimited: the route's rate_limit had no token left.
	OutcomeRateLimited Outcome = "rate_limited"
	// OutcomeInvalid: a channel message that violates REQ "Channel
	// Notification Handling", or a webhook body that could not be read to
	// the end. Neither fires.
	OutcomeInvalid Outcome = "invalid"
)

// Outcomes is every outcome, in REQ "Trigger Metrics" order, for renderers
// that want a stable column order and for the zero-initialized metric set.
var Outcomes = []Outcome{
	OutcomeFired, OutcomeIgnored, OutcomeDuplicate, OutcomeUnauthorized,
	OutcomeTooLarge, OutcomeRateLimited, OutcomeInvalid,
}

// SourceCounts is one source's counters, as a copy safe to keep.
type SourceCounts struct {
	// Outcomes counts each outcome. Absent keys are zero.
	Outcomes map[Outcome]uint64
	// LastEvent is when the source last fired, zero for never.
	LastEvent time.Time
	// Reconnects counts a channel session's returns to `connected` after
	// its first connection. Always zero for a webhook source.
	Reconnects uint64
}

// perSource is one source's live counters.
type perSource struct {
	outcomes   map[Outcome]uint64
	lastEvent  time.Time
	reconnects uint64
}

// OutcomeCounters counts per source reference. The zero value is ready to use
// and safe for concurrent use.
type OutcomeCounters struct {
	mu sync.Mutex
	m  map[string]*perSource
}

// entryLocked returns source's counters, creating them. Caller holds c.mu.
func (c *OutcomeCounters) entryLocked(source string) *perSource {
	if c.m == nil {
		c.m = map[string]*perSource{}
	}
	p := c.m[source]
	if p == nil {
		p = &perSource{outcomes: map[Outcome]uint64{}}
		c.m[source] = p
	}
	return p
}

// Inc counts one o for source.
func (c *OutcomeCounters) Inc(source string, o Outcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entryLocked(source).outcomes[o]++
}

// Fired counts one firing for source and records at as its last event. The
// last event never moves backwards: a firing stamped earlier than one already
// recorded (two deliveries racing through) must not rewind the timestamp an
// alert measures silence from.
func (c *OutcomeCounters) Fired(source string, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := c.entryLocked(source)
	p.outcomes[OutcomeFired]++
	if at.After(p.lastEvent) {
		p.lastEvent = at
	}
}

// Reconnected counts one channel reconnection for source.
func (c *OutcomeCounters) Reconnected(source string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entryLocked(source).reconnects++
}

// Count is how many o source has had.
func (c *OutcomeCounters) Count(source string, o Outcome) uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if p := c.m[source]; p != nil {
		return p.outcomes[o]
	}
	return 0
}

// Counts is a copy of source's counters; the zero SourceCounts (with an
// empty, non-nil Outcomes) when it has none.
func (c *OutcomeCounters) Counts(source string) SourceCounts {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := SourceCounts{Outcomes: map[Outcome]uint64{}}
	p := c.m[source]
	if p == nil {
		return out
	}
	for o, n := range p.outcomes {
		out.Outcomes[o] = n
	}
	out.LastEvent, out.Reconnects = p.lastEvent, p.reconnects
	return out
}

// Snapshot is a copy of every outcome count, safe to keep.
func (c *OutcomeCounters) Snapshot() map[string]map[Outcome]uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]map[Outcome]uint64, len(c.m))
	for src, p := range c.m {
		cp := make(map[Outcome]uint64, len(p.outcomes))
		for o, n := range p.outcomes {
			cp[o] = n
		}
		out[src] = cp
	}
	return out
}

// Retain forgets every source not in declared. A reload that removes or
// renames a source calls it, so the counters of a name the config no longer
// declares do not outlive it — and a source re-added later starts from zero,
// which a scraper reads as the counter reset it is.
func (c *OutcomeCounters) Retain(declared map[string]bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for src := range c.m {
		if !declared[src] {
			delete(c.m, src)
		}
	}
}

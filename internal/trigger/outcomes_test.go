package trigger

import (
	"sync"
	"testing"
)

// TestOutcomeCounters: the zero value counts, per source and per outcome,
// under concurrent increments, and a snapshot is a copy.
func TestOutcomeCounters(t *testing.T) {
	var c OutcomeCounters
	if n := c.Count("webhook.ci", OutcomeIgnored); n != 0 {
		t.Fatalf("zero value count = %d", n)
	}
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Inc("webhook.ci", OutcomeIgnored)
			c.Inc("webhook.gh", OutcomeRateLimited)
		}()
	}
	wg.Wait()
	c.Inc("webhook.ci", OutcomeDuplicate)
	if got := c.Count("webhook.ci", OutcomeIgnored); got != 50 {
		t.Errorf("ci ignored = %d, want 50", got)
	}
	if got := c.Count("webhook.gh", OutcomeRateLimited); got != 50 {
		t.Errorf("gh rate_limited = %d, want 50", got)
	}
	if got := c.Count("webhook.gh", OutcomeIgnored); got != 0 {
		t.Errorf("gh ignored = %d, want 0", got)
	}
	snap := c.Snapshot()
	snap["webhook.ci"][OutcomeDuplicate] = 99
	if got := c.Count("webhook.ci", OutcomeDuplicate); got != 1 {
		t.Errorf("mutating a snapshot changed the counter: %d", got)
	}
}

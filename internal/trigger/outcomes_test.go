package trigger

import (
	"sync"
	"testing"
	"time"
)

// TestOutcomeCountersFiredLastEventAndReconnects: a firing counts `fired` and
// moves the last event forward only, reconnects count per source, and Counts
// is a copy.
func TestOutcomeCountersFiredLastEventAndReconnects(t *testing.T) {
	var c OutcomeCounters
	if got := c.Counts("channel.sb"); got.Outcomes == nil || len(got.Outcomes) != 0 || !got.LastEvent.IsZero() || got.Reconnects != 0 {
		t.Fatalf("an unknown source's counts = %+v, want empty and non-nil", got)
	}
	t0 := time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	c.Fired("channel.sb", t0)
	c.Fired("channel.sb", t0.Add(-time.Minute)) // a late racer must not rewind it
	c.Reconnected("channel.sb")
	c.Reconnected("channel.sb")

	got := c.Counts("channel.sb")
	if got.Outcomes[OutcomeFired] != 2 {
		t.Errorf("fired = %d, want 2", got.Outcomes[OutcomeFired])
	}
	if !got.LastEvent.Equal(t0) {
		t.Errorf("last event = %v, want %v (never backwards)", got.LastEvent, t0)
	}
	if got.Reconnects != 2 {
		t.Errorf("reconnects = %d, want 2", got.Reconnects)
	}
	got.Outcomes[OutcomeFired] = 99
	if n := c.Count("channel.sb", OutcomeFired); n != 2 {
		t.Errorf("mutating Counts changed the counter: %d", n)
	}
}

// TestOutcomeCountersRetain: Retain forgets exactly the undeclared sources.
func TestOutcomeCountersRetain(t *testing.T) {
	var c OutcomeCounters
	c.Inc("webhook.ci", OutcomeUnauthorized)
	c.Inc("webhook.old", OutcomeUnauthorized)
	c.Retain(map[string]bool{"webhook.ci": true})
	if n := c.Count("webhook.ci", OutcomeUnauthorized); n != 1 {
		t.Errorf("a declared source lost its count: %d", n)
	}
	if _, ok := c.Snapshot()["webhook.old"]; ok {
		t.Error("an undeclared source outlived Retain")
	}
}

// TestOutcomesIsTheSpecSet pins the label values of
// harness_trigger_events_total to REQ "Trigger Metrics", in its order.
func TestOutcomesIsTheSpecSet(t *testing.T) {
	want := []string{"fired", "ignored", "duplicate", "unauthorized", "too_large", "rate_limited", "invalid"}
	if len(Outcomes) != len(want) {
		t.Fatalf("Outcomes = %v, want %v", Outcomes, want)
	}
	for i, o := range Outcomes {
		if string(o) != want[i] {
			t.Errorf("Outcomes[%d] = %q, want %q", i, o, want[i])
		}
	}
}

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

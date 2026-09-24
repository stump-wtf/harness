package source

// The source manager's share of harness_trigger_events_total and
// harness_trigger_reconnects_total: what it counts, when, and what a reload
// forgets.
//
// Governing: SPEC-0014 REQ "Trigger Metrics", REQ "Channel Notification
// Handling", REQ "Source Reconciliation On Reload".
//
// @joestump 09/24/2026 - Introduced with the SPEC-0014 trigger metrics (#480).

import (
	"context"
	"testing"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/trigger"
	"github.com/stump-wtf/harness/internal/trigger/channel/testserver"
)

// TestFireCountsFiredOnceAndStampsTheLastEvent: a bound event counts one
// `fired` and records its received_at; an event for a source nothing binds is
// `ignored`, never `fired`; an envelope that does not validate counts nothing.
// The counters are the ones Options.Counters handed in, so the daemon can
// share them with the webhook listener.
func TestFireCountsFiredOnceAndStampsTheLastEvent(t *testing.T) {
	shared := &trigger.OutcomeCounters{}
	r := &fakeRunner{}
	cfg := cfgWith([2]string{"one", "webhook.gh"}, [2]string{"two", "webhook.gh"})
	m := New(Options{Runner: r, Config: func() *core.Config { return cfg }, Log: log.New(discard{}), Counters: shared})
	m.Start(context.Background())
	t.Cleanup(m.Close)
	if m.Counters() != shared {
		t.Fatal("the manager did not keep the counters it was handed")
	}

	ev := webhookEvent("webhook.gh", "d-1")
	ev.ReceivedAt = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)
	if got := m.Fire(ev); len(got) != 2 {
		t.Fatalf("fired %d harnesses, want 2", len(got))
	}
	c := shared.Counts("webhook.gh")
	if c.Outcomes[trigger.OutcomeFired] != 1 {
		t.Errorf("fired = %d for one event fanned out to two harnesses, want 1", c.Outcomes[trigger.OutcomeFired])
	}
	if !c.LastEvent.Equal(ev.ReceivedAt) {
		t.Errorf("last event = %v, want the envelope's received_at %v", c.LastEvent, ev.ReceivedAt)
	}

	m.Fire(webhookEvent("webhook.nobody", "d-2"))
	if n := shared.Count("webhook.nobody", trigger.OutcomeIgnored); n != 1 {
		t.Errorf("an unbound source's event: ignored = %d, want 1", n)
	}
	if n := shared.Count("webhook.nobody", trigger.OutcomeFired); n != 0 {
		t.Errorf("an unbound source's event counted as fired: %d", n)
	}

	m.Fire(webhookEvent("webhook.gh", "")) // no event_id: invalid envelope
	if n := shared.Count("webhook.gh", trigger.OutcomeFired); n != 1 {
		t.Errorf("an envelope that does not validate was counted: fired = %d", n)
	}
}

// TestChannelOutcomesAndReconnectsAreCounted: over a real session to the fake
// server, a malformed doorbell counts `invalid`, a valid one `fired`, the
// first connection is not a reconnect, and a stream the server drops and the
// session re-opens counts exactly one.
func TestChannelOutcomesAndReconnectsAreCounted(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	t.Cleanup(srv.Close) // after the manager's cleanup: LIFO

	clk := newClock()
	cfg := channelCfg(srv.URL, true, "sweep")
	m, states := reconnectManager(t, &fakeRunner{}, func() *core.Config { return cfg }, clk)
	waitState(t, m, "channel.sb", trigger.StateConnected)
	if n := m.Counters().Counts("channel.sb").Reconnects; n != 0 {
		t.Fatalf("the first connection counted %d reconnects, want 0", n)
	}

	srv.PushNotification(`{"content":"ok","meta":{"todo_id":7}}`)
	srv.PushNotification(`{"content":"fine"}`)
	waitFor(t, 5*time.Second, "both doorbells are counted", func() bool {
		c := m.Counters().Counts("channel.sb")
		return c.Outcomes[trigger.OutcomeInvalid] == 1 && c.Outcomes[trigger.OutcomeFired] == 1
	})
	if m.Counters().Counts("channel.sb").LastEvent.IsZero() {
		t.Error("a fired doorbell left no last event")
	}

	srv.CloseStreams()
	states.waitSubsequence(t, "the source reconnects",
		trigger.StateConnected, trigger.StateBackoff, trigger.StateConnected)
	if n := m.Counters().Counts("channel.sb").Reconnects; n != 1 {
		t.Errorf("reconnects after one dropped stream = %d, want 1", n)
	}
}

// TestReconcileForgetsARemovedSourcesCounters: a reload that renames a source
// drops the old name's counters and keeps the survivors'.
func TestReconcileForgetsARemovedSourcesCounters(t *testing.T) {
	cfg := holding(webhookConfig(map[string]bool{"ci": true, "off": true}, "ci", "off"))
	m := New(Options{Runner: &fakeRunner{}, Config: cfg.get, Log: log.New(discard{})})
	m.Start(context.Background())
	t.Cleanup(m.Close)
	m.Fire(webhookEvent("webhook.ci", "d-1"))
	m.Counters().Inc("webhook.off", trigger.OutcomeUnauthorized)

	next := webhookConfig(map[string]bool{"ci": true, "lonely": true}, "ci", "lonely")
	cfg.set(next)
	m.Reconcile(next)

	snap := m.Counters().Snapshot()
	if _, ok := snap["webhook.off"]; ok {
		t.Error("a source the reload removed kept its counters")
	}
	if n := snap["webhook.ci"][trigger.OutcomeFired]; n != 1 {
		t.Errorf("a surviving source's fired = %d, want 1", n)
	}
}

package source

// Fan-out tests. A fake Runner stands in for supervisor.Manager so the
// assertions are about what the manager ASKED for — the order, the trigger,
// the source, the event — without spawning 50 processes to find out.
//
// The properties that matter are all about what happens when one harness
// misbehaves: the others must still fire, in order, and the daemon must
// survive. Those are the ones the fake makes cheap to drive.
//
// Governing: ADR-0021; SPEC-0014 REQ "Firing", REQ "Concurrency Safety".
//
// @joestump 09/22/2026 - Introduced with SPEC-0014 firing fan-out (#457).

import (
	"context"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/supervisor"
	"github.com/stump-wtf/harness/internal/trigger"
)

// fakeRunner records every StartRun it is asked for, and can be told to panic
// or to refuse for a given harness.
type fakeRunner struct {
	mu      sync.Mutex
	calls   []call
	panicOn map[string]bool
	unknown map[string]bool
	block   chan struct{} // when non-nil, StartRun waits on it
}

type call struct {
	name string
	req  supervisor.RunRequest
}

func (f *fakeRunner) StartRun(name string, req supervisor.RunRequest) (supervisor.RunDecision, bool) {
	// Recorded BEFORE blocking, so a test can observe that the firing has
	// entered the run entry point — which is the exact moment Close must
	// start waiting rather than abandoning it.
	f.mu.Lock()
	f.calls = append(f.calls, call{name: name, req: req})
	f.mu.Unlock()
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	n := len(f.calls)
	shouldPanic := f.panicOn[name]
	isUnknown := f.unknown[name]
	f.mu.Unlock()
	if shouldPanic {
		panic("boom in " + name)
	}
	if isUnknown {
		return supervisor.RunDecision{}, false
	}
	return supervisor.RunDecision{
		Kind: supervisor.DecisionStarted,
		Run:  supervisor.RunRecord{RunID: n},
	}, true
}

func (f *fakeRunner) names() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.name
	}
	return out
}

// cfgWith builds a config whose harnesses bind the given source refs, in the
// order listed.
func cfgWith(bindings ...[2]string) *core.Config {
	cfg := &core.Config{Harnesses: map[string]core.Harness{}}
	for _, b := range bindings {
		name, ref := b[0], b[1]
		h := cfg.Harnesses[name]
		h.Name = name
		h.Triggers = append(h.Triggers, ref)
		if _, seen := cfg.Harnesses[name]; !seen {
			cfg.HarnessOrder = append(cfg.HarnessOrder, name)
		}
		cfg.Harnesses[name] = h
	}
	return cfg
}

func webhookEvent(source, id string) *trigger.Envelope {
	e := &trigger.Envelope{
		Version:    trigger.EnvelopeVersion,
		Kind:       trigger.KindWebhook,
		Source:     source,
		EventID:    id,
		ReceivedAt: time.Now().UTC(),
		Webhook:    &trigger.WebhookEvent{Event: "pull_request", Delivery: id},
	}
	e.Webhook.SetBody("application/json", []byte(`{"n":1}`))
	return e
}

func newManager(t *testing.T, r Runner, cfg *core.Config) *Manager {
	t.Helper()
	m := New(Options{Runner: r, Config: func() *core.Config { return cfg }, Log: log.New(discard{})})
	m.Start(context.Background())
	t.Cleanup(m.Close)
	return m
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }

// TestFanOutInConfigOrder covers REQ "Firing"'s headline: one delivery reaches
// every harness that binds the source, in config order, each with its own
// decision.
func TestFanOutInConfigOrder(t *testing.T) {
	r := &fakeRunner{}
	cfg := cfgWith(
		[2]string{"pr-review", "webhook.gh"},
		[2]string{"unrelated", "channel.sb"},
		[2]string{"pr-labels", "webhook.gh"},
	)
	m := newManager(t, r, cfg)

	ev := webhookEvent("webhook.gh", "d-1")
	got := m.Fire(ev)

	if len(got) != 2 {
		t.Fatalf("decisions = %+v, want one per bound harness", got)
	}
	if got[0].Harness != "pr-review" || got[1].Harness != "pr-labels" {
		t.Errorf("fan-out order = %v, want config order [pr-review pr-labels]", []string{got[0].Harness, got[1].Harness})
	}
	if got[0].RunID == got[1].RunID {
		t.Error("both harnesses were given the same run id — each firing gets its own record")
	}
	for _, c := range r.calls {
		if c.req.Trigger != supervisor.TriggerWebhook {
			t.Errorf("%s: trigger = %q, want webhook", c.name, c.req.Trigger)
		}
		if c.req.Source != "webhook.gh" {
			t.Errorf("%s: source = %q", c.name, c.req.Source)
		}
		if c.req.Event == nil || c.req.Event.EventID != "d-1" {
			t.Errorf("%s: the event did not reach the run request", c.name)
		}
	}
	// The harness bound to a DIFFERENT source was not fired. Without this the
	// test would pass against a fan-out that simply started everything.
	for _, n := range r.names() {
		if n == "unrelated" {
			t.Error("a harness that does not bind the source was fired")
		}
	}
}

// TestChannelEventUsesTheChannelTrigger: the run's trigger follows the
// source's kind, which is what an operator reads off the history to tell a
// doorbell from a delivery.
func TestChannelEventUsesTheChannelTrigger(t *testing.T) {
	r := &fakeRunner{}
	m := newManager(t, r, cfgWith([2]string{"sweep", "channel.sb"}))

	ev := &trigger.Envelope{
		Version: trigger.EnvelopeVersion, Kind: trigger.KindChannel,
		Source: "channel.sb", EventID: "n-1", ReceivedAt: time.Now().UTC(),
		Channel: &trigger.ChannelEvent{Content: "todo ready"},
	}
	if got := m.Fire(ev); len(got) != 1 {
		t.Fatalf("decisions = %+v", got)
	}
	if r.calls[0].req.Trigger != supervisor.TriggerChannel {
		t.Errorf("trigger = %q, want channel", r.calls[0].req.Trigger)
	}
}

// TestOneHarnessPanickingDoesNotStopTheOthers covers REQ "Firing"'s "a failure
// to start one harness SHALL NOT prevent the others", and REQ "Concurrency
// Safety"'s "a panic while handling one event SHALL be recovered".
//
// The panic is planted on the FIRST harness deliberately: a recover placed
// around the whole fan-out instead of around each harness would pass a test
// that panicked on the last one.
func TestOneHarnessPanickingDoesNotStopTheOthers(t *testing.T) {
	r := &fakeRunner{panicOn: map[string]bool{"first": true}}
	m := newManager(t, r, cfgWith(
		[2]string{"first", "webhook.gh"},
		[2]string{"second", "webhook.gh"},
		[2]string{"third", "webhook.gh"},
	))

	got := m.Fire(webhookEvent("webhook.gh", "d-1"))
	if len(got) != 3 {
		t.Fatalf("decisions = %+v, want one per harness even when one panics", got)
	}
	if got[0].Err == "" {
		t.Error("the panicking harness has no error recorded")
	}
	for _, d := range got[1:] {
		if d.Err != "" || d.Kind != supervisor.DecisionStarted {
			t.Errorf("%s did not start after an earlier harness panicked: %+v", d.Harness, d)
		}
	}
	// And the next event still fires, which is the half that proves the
	// daemon survived rather than merely that one call returned.
	if next := m.Fire(webhookEvent("webhook.gh", "d-2")); len(next) != 3 {
		t.Errorf("the next event fired %d harnesses, want 3", len(next))
	}
}

// TestUnknownHarnessIsReportedNotFatal: a config naming a harness the Manager
// does not have is a per-harness error, not a failed delivery.
func TestUnknownHarnessIsReportedNotFatal(t *testing.T) {
	r := &fakeRunner{unknown: map[string]bool{"gone": true}}
	m := newManager(t, r, cfgWith(
		[2]string{"gone", "webhook.gh"},
		[2]string{"here", "webhook.gh"},
	))

	got := m.Fire(webhookEvent("webhook.gh", "d-1"))
	if len(got) != 2 || got[0].Err == "" || got[1].Err != "" {
		t.Fatalf("decisions = %+v, want an error for the unknown harness only", got)
	}
}

// TestFireIsANoOpForAnUnboundOrInvalidEvent: a source nothing binds, and an
// envelope that does not validate, both fire nothing — and neither is an
// error, because a declared-but-unbound source is a normal state.
func TestFireIsANoOpForAnUnboundOrInvalidEvent(t *testing.T) {
	r := &fakeRunner{}
	m := newManager(t, r, cfgWith([2]string{"sweep", "channel.sb"}))

	if got := m.Fire(webhookEvent("webhook.nobody", "d-1")); got != nil {
		t.Errorf("an unbound source fired %+v", got)
	}
	if got := m.Fire(nil); got != nil {
		t.Errorf("a nil event fired %+v", got)
	}
	bad := webhookEvent("webhook.gh", "")
	if got := m.Fire(bad); got != nil {
		t.Errorf("an invalid envelope fired %+v", got)
	}
	if len(r.calls) != 0 {
		t.Errorf("the runner was called %d times, want 0", len(r.calls))
	}
}

// TestBoundSetIsReReadPerFiring covers REQ "Source Reconciliation On Reload"'s
// "changes to the set of harnesses bound to a source apply from the next
// event". The manager holds a FUNCTION, not a snapshot, and this is why.
func TestBoundSetIsReReadPerFiring(t *testing.T) {
	r := &fakeRunner{}
	cfg := cfgWith([2]string{"one", "webhook.gh"})
	var mu sync.Mutex
	m := New(Options{
		Runner: r,
		Config: func() *core.Config {
			mu.Lock()
			defer mu.Unlock()
			return cfg
		},
		Log: log.New(discard{}),
	})
	m.Start(context.Background())
	t.Cleanup(m.Close)

	if got := m.Fire(webhookEvent("webhook.gh", "d-1")); len(got) != 1 {
		t.Fatalf("first firing = %+v", got)
	}
	mu.Lock()
	cfg = cfgWith([2]string{"one", "webhook.gh"}, [2]string{"two", "webhook.gh"})
	mu.Unlock()
	if got := m.Fire(webhookEvent("webhook.gh", "d-2")); len(got) != 2 {
		t.Errorf("after a reload the firing reached %+v, want both harnesses", got)
	}
}

// TestCloseAbandonsLaterFiringsAndWaitsForOne covers REQ "Concurrency
// Safety"'s shutdown rule: a firing that has not reached the run entry point
// is abandoned, and one that has is waited for.
func TestCloseAbandonsLaterFiringsAndWaitsForOne(t *testing.T) {
	r := &fakeRunner{block: make(chan struct{})}
	m := New(Options{
		Runner: r,
		Config: func() *core.Config { return cfgWith([2]string{"one", "webhook.gh"}) },
		Log:    log.New(discard{}),
	})
	m.Start(context.Background())

	// A firing parked inside StartRun.
	fired := make(chan []Decision, 1)
	go func() { fired <- m.Fire(webhookEvent("webhook.gh", "d-1")) }()
	waitFor(t, 2*time.Second, "the firing reaches the runner", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.calls) == 1
	})

	closed := make(chan struct{})
	go func() { m.Close(); close(closed) }()

	// Close must not return while a firing is inside the run entry point:
	// that firing has a record, and abandoning it would leave one with no
	// process and nothing to explain it.
	select {
	case <-closed:
		t.Fatal("Close returned while a firing was still in the run entry point")
	case <-time.After(100 * time.Millisecond):
	}

	close(r.block)
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not return after the firing finished")
	}
	<-fired

	// A firing that arrives after Close is abandoned: it has no record, so
	// dropping it is invisible and safe.
	if got := m.Fire(webhookEvent("webhook.gh", "d-2")); got != nil {
		t.Errorf("a firing after Close reached %+v", got)
	}
	r.mu.Lock()
	n := len(r.calls)
	r.mu.Unlock()
	if n != 1 {
		t.Errorf("the runner saw %d calls after Close, want 1", n)
	}
}

// TestCloseAbandonsTheRestOfAFanOutInProgress: Close arriving while a
// fan-out is parked on its FIRST harness must abandon the harnesses after it,
// which have not reached the run entry point — Start's contract ("firings
// that have not reached the run entry point are abandoned") is per harness,
// not per event. Checking the context once, before the loop, fired every
// remaining harness during shutdown.
func TestCloseAbandonsTheRestOfAFanOutInProgress(t *testing.T) {
	r := &fakeRunner{block: make(chan struct{})}
	m := New(Options{
		Runner: r,
		Config: func() *core.Config {
			return cfgWith([2]string{"first", "webhook.gh"}, [2]string{"second", "webhook.gh"})
		},
		Log: log.New(discard{}),
	})
	m.Start(context.Background())

	fired := make(chan []Decision, 1)
	go func() { fired <- m.Fire(webhookEvent("webhook.gh", "d-1")) }()
	waitFor(t, 2*time.Second, "the first harness reaches the runner", func() bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		return len(r.calls) == 1
	})

	closed := make(chan struct{})
	go func() { m.Close(); close(closed) }()
	// Close cancels before it waits; give it the moment to do so while the
	// first firing is still parked.
	waitFor(t, 2*time.Second, "Close cancels the context", func() bool {
		m.mu.Lock()
		defer m.mu.Unlock()
		return m.ctx.Err() != nil
	})
	close(r.block)
	<-closed
	got := <-fired

	if names := r.names(); len(names) != 1 {
		t.Errorf("the runner saw %v, want only the harness already in flight when Close began", names)
	}
	if len(got) != 2 || got[0].Err != "" || got[1].Err == "" {
		t.Errorf("decisions = %+v, want the first fired and the second abandoned with a reason", got)
	}
}

// waitFor polls until pred holds or the deadline passes.
func waitFor(t *testing.T, d time.Duration, what string, pred func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !pred() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for: %s", d, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

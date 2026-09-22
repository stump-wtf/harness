package metrics

// Collector Tests
//
// Governing tests: SPEC-0013 REQ-2 (every state value, zeros included),
// REQ-3 (last success omitted, not zeroed), REQ-4, REQ-5 (the cap and the
// forbidden labels), REQ-6 (honest absence and the collection-error counter);
// design.md "Testing".
//
// @joestump-agent 09/21/2026 - Added for harness#356.

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/observe"
	"github.com/stump-wtf/harness/internal/supervisor"
)

var t0 = time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC)

func newTestMetrics(t *testing.T, src *fakeSource, opts Options) *Metrics {
	t.Helper()
	if opts.Now == nil {
		c := &clock{t: t0}
		opts.Now = c.Now
	}
	m := New(src, opts)
	m.Start()
	t.Cleanup(m.Close)
	return m
}

// Every declared harness reports all four state values, exactly one of them 1,
// under the documented seven-to-four mapping.
func TestEveryDeclaredHarnessReportsAllFourStates(t *testing.T) {
	src := newFakeSource()
	cases := []struct {
		name string
		snap supervisor.Snapshot
		want string
	}{
		{"is-running", supervisor.Snapshot{State: core.StateRunning, PID: 1}, stateRunning},
		{"is-starting", supervisor.Snapshot{State: core.StateStarting}, stateRunning},
		{"is-stopped", supervisor.Snapshot{State: core.StateStopped}, stateStopped},
		{"is-stopping", supervisor.Snapshot{State: core.StateStopping, PID: 1}, stateStopped},
		{"is-restarting", supervisor.Snapshot{State: core.StateRestarting}, stateStopped},
		{"is-degraded", supervisor.Snapshot{State: core.StateDegraded, Flapping: true}, stateFlapping},
		{"restarting-flapping", supervisor.Snapshot{State: core.StateRestarting, Flapping: true}, stateFlapping},
		{"running-flapping", supervisor.Snapshot{State: core.StateRunning, Flapping: true, PID: 1}, stateFlapping},
		{"is-failed", supervisor.Snapshot{State: core.StateFailed, Flapping: true}, stateFailed},
		// Held by operating hours (SPEC-0012): shut down by the gate, not by
		// a fault, so stopped — even with crash-loop history caught mid-hold.
		{"held", supervisor.Snapshot{State: core.StateStopped, Gated: true, Held: true}, stateStopped},
		{"held-stopping", supervisor.Snapshot{State: core.StateStopping, Gated: true, Held: true, PID: 1}, stateStopped},
		{"held-was-flapping", supervisor.Snapshot{State: core.StateRestarting, Gated: true, Held: true, Flapping: true}, stateStopped},
		{"held-was-degraded", supervisor.Snapshot{State: core.StateDegraded, Gated: true, Held: true}, stateStopped},
		// A graceful close in flight (SPEC-0012 REQ "Graceful Shutdown"): held,
		// but the process is still up finishing its turn, so it is running
		// until the close lands.
		{"held-closing", supervisor.Snapshot{State: core.StateRunning, Gated: true, Held: true, Closing: true, PID: 1}, stateRunning},
		// Gated but in hours is an ordinary running harness.
		{"gated-running", supervisor.Snapshot{State: core.StateRunning, Gated: true, PID: 1}, stateRunning},
	}
	for _, c := range cases {
		src.add(core.Harness{Name: c.name, Adapter: "generic"}, c.snap)
	}
	fams := scrape(t, newTestMetrics(t, src, Options{}))

	for _, c := range cases {
		ones := 0
		for _, sv := range stateValues {
			v := fams.must(t, "harness_harness_state", lbls("harness", c.name, "state", sv))
			if v != 0 && v != 1 {
				t.Errorf("%s state=%s = %v, want 0 or 1", c.name, sv, v)
			}
			if v == 1 {
				ones++
				if sv != c.want {
					t.Errorf("%s reports state=%s, want %s", c.name, sv, c.want)
				}
			}
		}
		if ones != 1 {
			t.Errorf("%s has %d states at 1, want exactly 1", c.name, ones)
		}
	}
	// And no fifth state value sneaks in.
	if got, want := len(fams["harness_harness_state"].GetMetric()), 4*len(cases); got != want {
		t.Errorf("harness_harness_state has %d series, want %d (4 per harness)", got, want)
	}
}

// harness_consecutive_failures and harness_restarts_total come straight from
// the snapshot, at scrape time, so a harness walking toward give-up is visible
// before it lands in failed.
func TestSupervisorCountersReadAtScrape(t *testing.T) {
	src := newFakeSource()
	src.add(core.Harness{Name: "walker", Adapter: "generic"}, supervisor.Snapshot{State: core.StateRestarting, RestartCount: 1, ConsecutiveFailures: 1})
	m := newTestMetrics(t, src, Options{})
	for i := 1; i <= 5; i++ {
		src.add(core.Harness{Name: "walker", Adapter: "generic"}, supervisor.Snapshot{State: core.StateRestarting, RestartCount: i, ConsecutiveFailures: i})
		fams := scrape(t, m)
		if v := fams.must(t, "harness_consecutive_failures", lbls("harness", "walker")); v != float64(i) {
			t.Errorf("step %d: consecutive failures = %v", i, v)
		}
		if v := fams.must(t, "harness_restarts_total", lbls("harness", "walker")); v != float64(i) {
			t.Errorf("step %d: restarts = %v", i, v)
		}
	}
	src.add(core.Harness{Name: "walker", Adapter: "generic"}, supervisor.Snapshot{State: core.StateFailed, RestartCount: 6, ConsecutiveFailures: 6})
	if v := scrape(t, m).must(t, "harness_harness_state", lbls("harness", "walker", "state", "failed")); v != 1 {
		t.Errorf("failed = %v after give-up", v)
	}
}

// REQ-3: the timestamp is omitted, not zeroed, for a harness that has not
// succeeded — even while its other model series report errors — and appears
// with the item's own time once it does.
func TestLastSuccessOmittedUntilFirstSuccess(t *testing.T) {
	src := newFakeSource()
	src.add(crushHarness("worker"), runningSnap())
	feed := newFakeFeed()
	m := newTestMetrics(t, src, Options{Observer: feed})

	fams := scrape(t, m)
	if _, ok := fams.get("harness_last_successful_call_timestamp", lbls("harness", "worker")); ok {
		t.Fatal("last-success timestamp present before any call")
	}
	// The harness is observable, so its call counters exist at zero.
	if v := fams.must(t, "harness_model_calls_total", lbls("harness", "worker", "outcome", "success")); v != 0 {
		t.Errorf("success calls = %v", v)
	}

	feed.ch <- errorEvent("worker", "s1", "429 Too Many Requests", t0.Add(time.Second))
	eventually(t, "the error counted", func() bool {
		v, _ := scrape(t, m).get("harness_model_calls_total", lbls("harness", "worker", "outcome", "error"))
		return v == 1
	})
	if _, ok := scrape(t, m).get("harness_last_successful_call_timestamp", lbls("harness", "worker")); ok {
		t.Fatal("last-success timestamp present after only an error")
	}

	at := t0.Add(2 * time.Second)
	feed.ch <- toolEvent("worker", "s1", at)
	eventually(t, "the success counted", func() bool {
		_, ok := scrape(t, m).get("harness_last_successful_call_timestamp", lbls("harness", "worker"))
		return ok
	})
	if v := scrape(t, m).must(t, "harness_last_successful_call_timestamp", lbls("harness", "worker")); v != float64(at.Unix()) {
		t.Errorf("last success = %v, want %v", v, at.Unix())
	}

	// A late item never rewinds it.
	feed.ch <- toolEvent("worker", "s1", t0)
	feed.ch <- toolEvent("worker", "s1", at) // a marker to wait on
	eventually(t, "both late items counted", func() bool {
		v, _ := scrape(t, m).get("harness_model_calls_total", lbls("harness", "worker", "outcome", "success"))
		return v == 3
	})
	if v := scrape(t, m).must(t, "harness_last_successful_call_timestamp", lbls("harness", "worker")); v != float64(at.Unix()) {
		t.Errorf("late item rewound last success to %v", v)
	}
}

// REQ-6: a harness the observer cannot read has no model series at all. A
// zero would read as a healthy, idle agent.
func TestUnobservableHarnessOmitsModelSeries(t *testing.T) {
	src := newFakeSource()
	src.add(core.Harness{Name: "script", Adapter: "generic", Workdir: "/w"}, runningSnap())
	src.add(core.Harness{Name: "homeless", Adapter: "crush"}, runningSnap()) // no workdir
	src.add(crushHarness("worker"), runningSnap())
	fams := scrape(t, newTestMetrics(t, src, Options{Observer: newFakeFeed()}))

	for _, name := range []string{"harness_model_calls_total", "harness_model_call_errors_total", "harness_model_call_errors_unclassified_total", "harness_sessions_started_total", "harness_session_active"} {
		if got := fams.harnessValues(name); len(got) != 1 || got[0] != "worker" {
			t.Errorf("%s harness values = %v, want only [worker]", name, got)
		}
	}
	// State is still reported for every harness.
	if got := fams.harnessValues("harness_harness_state"); len(got) != 3 {
		t.Errorf("state harnesses = %v, want all three", got)
	}
}

// Without an observer the model series cannot be computed at all.
func TestNoObserverOmitsModelSeries(t *testing.T) {
	src := newFakeSource()
	src.add(crushHarness("worker"), runningSnap())
	fams := scrape(t, newTestMetrics(t, src, Options{}))
	if _, ok := fams["harness_model_calls_total"]; ok {
		t.Error("model calls reported with no observer")
	}
	if _, ok := fams["harness_observer_events_delivered_total"]; ok {
		t.Error("observer stats reported with no observer")
	}
}

// REQ-5: past the cap, harnesses share __other__, which counts them.
func TestCardinalityOverflowCollapsesIntoOther(t *testing.T) {
	src := newFakeSource()
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		src.add(crushHarness(n), runningSnap())
	}
	src.add(crushHarness("f"), supervisor.Snapshot{State: core.StateFailed, ConsecutiveFailures: 7})
	feed := newFakeFeed()
	m := newTestMetrics(t, src, Options{Observer: feed, MaxHarnesses: 3})

	fams := scrape(t, m)
	if got, want := strings.Join(fams.harnessValues("harness_harness_state"), ","), "__other__,a,b,c"; got != want {
		t.Fatalf("harness label values = %s, want %s", got, want)
	}
	if v := fams.must(t, "harness_harness_state", lbls("harness", OverflowLabel, "state", "running")); v != 2 {
		t.Errorf("__other__ running = %v, want 2 (d and e)", v)
	}
	if v := fams.must(t, "harness_harness_state", lbls("harness", OverflowLabel, "state", "failed")); v != 1 {
		t.Errorf("__other__ failed = %v, want 1 (f)", v)
	}
	if v := fams.must(t, "harness_consecutive_failures", lbls("harness", OverflowLabel)); v != 7 {
		t.Errorf("__other__ consecutive failures = %v, want the worst (7)", v)
	}
	if v := fams.must(t, "harness_metrics_harnesses_overflowed", nil); v != 3 {
		t.Errorf("overflowed = %v, want 3", v)
	}

	// Events for overflow harnesses land in __other__, summed.
	feed.ch <- errorEvent("d", "s1", "429", t0)
	feed.ch <- errorEvent("e", "s2", "429", t0)
	eventually(t, "overflow errors counted", func() bool {
		v, _ := scrape(t, m).get("harness_model_call_errors_total", lbls("harness", OverflowLabel, "class", "quota"))
		return v == 2
	})

	// A removed harness frees its slot; the next overflow harness seen takes
	// it and gets a series of its own.
	src.remove("a")
	fams = scrape(t, m)
	if got, want := strings.Join(fams.harnessValues("harness_harness_state"), ","), "__other__,b,c,d"; got != want {
		t.Errorf("after removing a, harness label values = %s, want %s", got, want)
	}
}

// A harness literally named __other__ never gets a slot of its own.
func TestHarnessNamedOtherIsFoldedIntoOverflow(t *testing.T) {
	src := newFakeSource()
	src.add(core.Harness{Name: OverflowLabel, Adapter: "generic"}, runningSnap())
	src.add(core.Harness{Name: "real", Adapter: "generic"}, runningSnap())
	fams := scrape(t, newTestMetrics(t, src, Options{MaxHarnesses: 1}))
	if got := strings.Join(fams.harnessValues("harness_harness_state"), ","); got != "__other__,real" {
		t.Errorf("harness label values = %s, want __other__,real (real keeps the one slot)", got)
	}
}

// REQ-5: session ids, prompt text, model names, credentials and environment
// are never labels. Feed all of them in and assert the exposition's label
// names are the fixed set and no label value carries any of them.
func TestForbiddenLabelsNeverAppear(t *testing.T) {
	const secret = "sk-ant-api03-SECRETSECRETSECRETSECRET"
	src := newFakeSource()
	h := crushHarness("worker")
	h.Model = "claude-opus-5-secret-model"
	h.EnvFile = "/secrets/ANTHROPIC.env"
	h.Prompt = "drain the queue PROMPTTEXT"
	src.add(h, runningSnap())
	feed := newFakeFeed()
	m := newTestMetrics(t, src, Options{Observer: feed})

	ev := errorEvent("worker", "session-id-SESSIONID", "weird failure "+secret, t0)
	ev.Session.Model = h.Model
	ev.Session.Title = "PROMPTTEXT"
	feed.ch <- ev
	tool := toolEvent("worker", "session-id-SESSIONID", t0)
	tool.Tool.Summary = "curl -H 'x-api-key: " + secret + "'"
	feed.ch <- tool
	eventually(t, "both items counted", func() bool {
		v, _ := scrape(t, m).get("harness_model_calls_total", lbls("harness", "worker", "outcome", "success"))
		return v == 1
	})

	allowed := map[string]bool{"harness": true, "state": true, "to": true, "outcome": true, "class": true, "collector": true, "subscriber": true, "adapter": true}
	for name, fam := range scrape(t, m) {
		if !strings.HasPrefix(name, "harness_") {
			continue
		}
		for _, mt := range fam.GetMetric() {
			for _, lp := range mt.GetLabel() {
				if !allowed[lp.GetName()] {
					t.Errorf("%s carries label %q", name, lp.GetName())
				}
				for _, bad := range []string{secret, "SESSIONID", "PROMPTTEXT", "secret-model", "ANTHROPIC"} {
					if strings.Contains(lp.GetValue(), bad) {
						t.Errorf("%s label %s=%q leaks %q", name, lp.GetName(), lp.GetValue(), bad)
					}
				}
			}
		}
	}
}

// State transitions count from the lifecycle bus, with every target state at
// zero from the first scrape; scheduled runs count only verdicts; the next run
// is read from the schedule at scrape time and omitted when there is none.
func TestTransitionsAndSchedules(t *testing.T) {
	src := newFakeSource()
	src.add(core.Harness{Name: "svc", Adapter: "generic"}, runningSnap())
	src.add(core.Harness{Name: "sweep", Adapter: "generic", Schedule: "*/5 * * * *"}, supervisor.Snapshot{State: core.StateStopped, Scheduled: true})
	src.add(core.Harness{Name: "idle-job", Adapter: "generic", Schedule: "@daily"}, supervisor.Snapshot{State: core.StateStopped, Scheduled: true})
	next := t0.Add(5 * time.Minute)
	m := newTestMetrics(t, src, Options{NextRun: func(name string) (time.Time, bool) {
		if name == "sweep" {
			return next, true
		}
		return time.Time{}, false
	}})

	fams := scrape(t, m)
	for _, to := range core.States {
		if v := fams.must(t, "harness_state_transitions_total", lbls("harness", "svc", "to", string(to))); v != 0 {
			t.Errorf("transitions to %s = %v before any", to, v)
		}
	}
	if _, ok := fams["harness_scheduled_runs_total"]; !ok {
		t.Fatal("no scheduled-run series")
	}
	if got := strings.Join(fams.harnessValues("harness_scheduled_runs_total"), ","); got != "idle-job,sweep" {
		t.Errorf("scheduled-run harnesses = %s, want only the scheduled ones", got)
	}
	if v := fams.must(t, "harness_scheduled_next_run_timestamp", lbls("harness", "sweep")); v != float64(next.Unix()) {
		t.Errorf("next run = %v, want %v", v, next.Unix())
	}
	if _, ok := fams.get("harness_scheduled_next_run_timestamp", lbls("harness", "idle-job")); ok {
		t.Error("next run reported for a harness with no next window")
	}

	src.bus.Publish(supervisor.Event{Kind: supervisor.EventStateChanged, Name: "svc", From: core.StateRunning, To: core.StateDegraded})
	src.bus.Publish(supervisor.Event{Kind: supervisor.EventStateChanged, Name: "svc", From: core.StateDegraded, To: core.StateFailed})
	for _, o := range []supervisor.RunOutcome{supervisor.OutcomeSuccess, supervisor.OutcomeFailed, supervisor.OutcomeTimedOut, supervisor.OutcomeSkipped, supervisor.OutcomeMissed, supervisor.OutcomeCancelled} {
		src.bus.Publish(supervisor.Event{Kind: supervisor.EventRunFinished, Name: "sweep", Run: supervisor.RunRecord{Outcome: o}})
	}
	src.bus.Publish(supervisor.Event{Kind: supervisor.EventRunFinished, Name: "sweep", Run: supervisor.RunRecord{Outcome: supervisor.OutcomeSuccess}})
	eventually(t, "the bus events counted", func() bool {
		v, _ := scrape(t, m).get("harness_scheduled_runs_total", lbls("harness", "sweep", "outcome", "success"))
		return v == 2
	})
	fams = scrape(t, m)
	if v := fams.must(t, "harness_scheduled_runs_total", lbls("harness", "sweep", "outcome", "failure")); v != 2 {
		t.Errorf("failures = %v, want 2 (failed + timed_out; skipped/missed/cancelled pass no verdict)", v)
	}
	if v := fams.must(t, "harness_state_transitions_total", lbls("harness", "svc", "to", "failed")); v != 1 {
		t.Errorf("transitions to failed = %v", v)
	}
	if v := fams.must(t, "harness_state_transitions_total", lbls("harness", "svc", "to", "degraded")); v != 1 {
		t.Errorf("transitions to degraded = %v", v)
	}
}

// REQ-6: lifecycle events the collector's bus subscription lost are
// transitions and run outcomes it will never count, so the loss is published
// as harness_metrics_collection_errors_total{collector="lifecycle"}.
//
// The subscriber is stalled by holding the collector's lock while a burst is
// published past the bus buffer. Every published event must then be either
// counted or reported lost — none may vanish — and the reported loss must be
// exactly what the bus recorded for this subscriber. (review, harness#356)
func TestLifecycleDropsAreCollectionErrors(t *testing.T) {
	src := newFakeSource()
	src.add(core.Harness{Name: "svc", Adapter: "generic"}, runningSnap())
	m := newTestMetrics(t, src, Options{})

	lost := func(f families) float64 {
		return f.must(t, "harness_metrics_collection_errors_total", lbls("collector", "lifecycle"))
	}
	if v := lost(scrape(t, m)); v != 0 {
		t.Fatalf("lifecycle errors = %v before any loss", v)
	}

	const burst = 400 // well past the bus's per-subscriber buffer
	m.mu.Lock()
	for i := 0; i < burst; i++ {
		src.bus.Publish(supervisor.Event{Kind: supervisor.EventStateChanged, Name: "svc", From: core.StateStarting, To: core.StateRunning})
	}
	m.mu.Unlock()

	var counted, dropped float64
	eventually(t, "every published event counted or reported lost", func() bool {
		f := scrape(t, m)
		counted = f.must(t, "harness_state_transitions_total", lbls("harness", "svc", "to", "running"))
		dropped = lost(f)
		return counted+dropped == burst
	})
	if dropped == 0 {
		t.Fatalf("no loss reported for a subscriber stalled through %d events (counted %v)", burst, counted)
	}
	if want := float64(m.lifecycleDrops()); dropped != want {
		t.Errorf("lifecycle errors = %v, want the bus's own count %v", dropped, want)
	}
	if v := lost(scrape(t, m)); v != dropped {
		t.Errorf("lifecycle errors = %v on a later scrape, want still %v (counted once)", v, dropped)
	}
}

// The daemon starts the collector before Autostart with no observer, and
// attaches the observer and the schedule reader once they exist. Until then
// the model series are omitted (they cannot be computed); after, they appear
// and count, and the next-run gauge reads the attached schedule.
// (review, harness#356)
func TestAttachAfterStart(t *testing.T) {
	src := newFakeSource()
	src.add(crushHarness("worker"), runningSnap())
	src.add(core.Harness{Name: "sweep", Adapter: "generic", Schedule: "@hourly"}, supervisor.Snapshot{State: core.StateStopped, Scheduled: true})
	m := newTestMetrics(t, src, Options{})

	fams := scrape(t, m)
	if _, ok := fams["harness_model_calls_total"]; ok {
		t.Error("model calls reported before an observer was attached")
	}
	if _, ok := fams.get("harness_scheduled_next_run_timestamp", lbls("harness", "sweep")); ok {
		t.Error("next run reported before a schedule reader was attached")
	}

	feed := newFakeFeed()
	next := t0.Add(time.Hour)
	m.Attach(feed, func(string) (time.Time, bool) { return next, true })
	feed.ch <- toolEvent("worker", "s1", t0)
	eventually(t, "the attached feed counted", func() bool {
		v, _ := scrape(t, m).get("harness_model_calls_total", lbls("harness", "worker", "outcome", "success"))
		return v == 1
	})
	if v := scrape(t, m).must(t, "harness_scheduled_next_run_timestamp", lbls("harness", "sweep")); v != float64(next.Unix()) {
		t.Errorf("next run = %v, want %v", v, next.Unix())
	}
}

// Sessions are counted once each; activity needs both a live process and a
// recent item.
func TestSessionsStartedAndActive(t *testing.T) {
	src := newFakeSource()
	src.add(crushHarness("worker"), runningSnap())
	feed := newFakeFeed()
	c := &clock{t: t0}
	m := newTestMetrics(t, src, Options{Observer: feed, Now: c.Now, SessionIdle: time.Minute})

	if v := scrape(t, m).must(t, "harness_session_active", lbls("harness", "worker")); v != 0 {
		t.Errorf("active = %v before any item", v)
	}
	feed.ch <- toolEvent("worker", "s1", t0)
	feed.ch <- toolEvent("worker", "s1", t0)
	feed.ch <- errorEvent("worker", "s2", "429", t0)
	eventually(t, "three items counted", func() bool {
		v, _ := scrape(t, m).get("harness_model_calls_total", lbls("harness", "worker", "outcome", "error"))
		return v == 1
	})
	fams := scrape(t, m)
	if v := fams.must(t, "harness_sessions_started_total", lbls("harness", "worker")); v != 2 {
		t.Errorf("sessions started = %v, want 2", v)
	}
	if v := fams.must(t, "harness_session_active", lbls("harness", "worker")); v != 1 {
		t.Errorf("active = %v right after items", v)
	}

	c.Set(t0.Add(2 * time.Minute))
	if v := scrape(t, m).must(t, "harness_session_active", lbls("harness", "worker")); v != 0 {
		t.Errorf("active = %v past the idle window", v)
	}

	c.Set(t0)
	src.add(crushHarness("worker"), supervisor.Snapshot{State: core.StateStopped})
	if v := scrape(t, m).must(t, "harness_session_active", lbls("harness", "worker")); v != 0 {
		t.Errorf("active = %v with no process", v)
	}
}

// REQ-6: a collector that fails omits what it could not read and says so.
func TestCollectionErrorsAreCountedNotFlattened(t *testing.T) {
	src := newFakeSource()
	src.add(crushHarness("worker"), runningSnap())
	feed := newFakeFeed()
	m := newTestMetrics(t, src, Options{Observer: feed})

	fams := scrape(t, m)
	for _, c := range collectorNames {
		if v := fams.must(t, "harness_metrics_collection_errors_total", lbls("collector", c)); v != 0 {
			t.Errorf("collector %s errors = %v at start", c, v)
		}
	}

	// A failing snapshot read: no harness series at all (not zeros), and the
	// failure counted.
	src.setPanic(true)
	fams = scrape(t, m)
	if _, ok := fams["harness_harness_state"]; ok {
		t.Error("state reported while the supervisor could not be read")
	}
	if v := fams.must(t, "harness_metrics_collection_errors_total", lbls("collector", "supervisor")); v != 1 {
		t.Errorf("supervisor errors = %v, want 1", v)
	}
	src.setPanic(false)

	// Items this subscriber lost are undercounted model calls.
	feed.setStats(observe.Stats{Dropped: map[string]uint64{subscriberName: 3, "otel": 9}})
	fams = scrape(t, m)
	if v := fams.must(t, "harness_metrics_collection_errors_total", lbls("collector", "observer")); v != 3 {
		t.Errorf("observer errors = %v, want the 3 drops", v)
	}
	if v := fams.must(t, "harness_observer_events_dropped_total", lbls("subscriber", "otel")); v != 9 {
		t.Errorf("otel drops = %v", v)
	}
	// Counted once, not per scrape.
	if v := scrape(t, m).must(t, "harness_metrics_collection_errors_total", lbls("collector", "observer")); v != 3 {
		t.Errorf("observer errors = %v on a second scrape, want still 3", v)
	}

	// The feed dying under a running Metrics: reachability can no longer be
	// computed, so it disappears rather than freezing.
	feed.close()
	eventually(t, "the dead feed noticed", func() bool {
		v, _ := scrape(t, m).get("harness_metrics_collection_errors_total", lbls("collector", "observer"))
		return v == 4
	})
	fams = scrape(t, m)
	if _, ok := fams["harness_model_calls_total"]; ok {
		t.Error("model calls still reported after the observer feed closed")
	}
	if _, ok := fams.get("harness_harness_state", lbls("harness", "worker", "state", "running")); !ok {
		t.Error("state vanished with the observer feed; it does not depend on it")
	}
}

// The observer's own counters are published, so a blind observer is visible.
func TestObserverStatsExposed(t *testing.T) {
	src := newFakeSource()
	feed := newFakeFeed()
	last := t0.Add(-time.Second)
	feed.setStats(observe.Stats{
		Delivered: 10, Ambiguous: 2, Unattributed: 1, ScanErrors: 4, Sessions: 3, Contested: 1, LastScan: last,
		ParseErrors: map[string]uint64{"crush": 5},
	})
	fams := scrape(t, newTestMetrics(t, src, Options{Observer: feed}))
	for name, want := range map[string]float64{
		"harness_observer_events_delivered_total":   10,
		"harness_observer_items_ambiguous_total":    2,
		"harness_observer_items_unattributed_total": 1,
		"harness_observer_scan_errors_total":        4,
		"harness_observer_sessions":                 3,
		"harness_observer_sessions_contested":       1,
		"harness_observer_last_scan_timestamp":      float64(last.Unix()),
	} {
		if v := fams.must(t, name, nil); v != want {
			t.Errorf("%s = %v, want %v", name, v, want)
		}
	}
	if v := fams.must(t, "harness_observer_parse_errors_total", lbls("adapter", "crush")); v != 5 {
		t.Errorf("parse errors = %v", v)
	}
}

// The Go and process collectors are registered (REQ-1), and the exposition
// passes promlint for every family this package defines.
func TestRuntimeCollectorsAndLint(t *testing.T) {
	src := newFakeSource()
	src.add(crushHarness("worker"), runningSnap())
	m := newTestMetrics(t, src, Options{Observer: newFakeFeed()})
	fams := scrape(t, m)
	if _, ok := fams["go_goroutines"]; !ok {
		t.Error("go_* collector not registered")
	}
	if _, ok := fams["process_start_time_seconds"]; !ok {
		t.Error("process_* collector not registered")
	}
	problems, err := testutil.GatherAndLint(m.Registry())
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range problems {
		if strings.HasPrefix(p.Metric, "harness_") {
			t.Errorf("promlint: %s: %s", p.Metric, p.Text)
		}
	}
}

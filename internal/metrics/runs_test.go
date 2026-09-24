package metrics

import (
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/ledger"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// openLedger is a real run ledger in a temp directory.
func openLedger(t *testing.T) *ledger.Ledger {
	t.Helper()
	l, err := ledger.Open(t.TempDir(), ledger.Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = l.Close(5 * time.Second) })
	return l
}

// decide appends a whole record for harness h run id with outcome, synced.
func decide(t *testing.T, l *ledger.Ledger, h string, id int, kind, outcome string, start time.Time, took time.Duration) {
	t.Helper()
	end := start.Add(took)
	if _, err := l.Append(ledger.Line{Type: ledger.TypeDecided, At: end, Harness: h, RunID: id,
		Record: ledger.Record{Kind: kind, Trigger: "schedule", Outcome: outcome, StartedAt: &start, EndedAt: &end}}, true); err != nil {
		t.Fatal(err)
	}
}

// Run outcomes are counted from the run ledger and from nothing else
// (SPEC-0022 REQ-10, REQ-11). A run-finished event on the lifecycle bus with
// no ledger record behind it must move no counter; the ledger record must.
// Run against the collector that still counted the bus, this fails on the
// first assertion.
func TestRunCountsComeFromTheLedgerNotTheBus(t *testing.T) {
	src := newFakeSource()
	src.add(core.Harness{Name: "sweep", Adapter: "generic", Schedule: "*/5 * * * *"}, supervisor.Snapshot{State: core.StateStopped, Scheduled: true, Triggered: true})
	l := openLedger(t)
	m := newTestMetrics(t, src, Options{Runs: l})

	src.bus.Publish(supervisor.Event{Kind: supervisor.EventRunFinished, Name: "sweep", Run: supervisor.RunRecord{Outcome: supervisor.OutcomeSuccess}})
	src.bus.Publish(supervisor.Event{Kind: supervisor.EventStateChanged, Name: "sweep", From: core.StateStopped, To: core.StateRunning})
	eventually(t, "the bus consumed", func() bool {
		v, _ := scrape(t, m).get("harness_state_transitions_total", lbls("harness", "sweep", "to", "running"))
		return v == 1
	})
	if v := scrape(t, m).must(t, "harness_scheduled_runs_total", lbls("harness", "sweep", "outcome", "success")); v != 0 {
		t.Fatalf("scheduled success = %v after a bus event with no ledger record; the collector counts the bus", v)
	}

	decide(t, l, "sweep", 1, ledger.KindOneshot, "success", t0, time.Minute)
	eventually(t, "the ledger record counted", func() bool {
		v, _ := scrape(t, m).get("harness_scheduled_runs_total", lbls("harness", "sweep", "outcome", "success"))
		return v == 1
	})
	if v := scrape(t, m).must(t, "harness_runs_total", lbls("harness", "sweep", "kind", "oneshot", "outcome", "success")); v != 1 {
		t.Errorf("harness_runs_total success = %v, want 1", v)
	}
}

// REQ-5's mapping for harness_scheduled_runs_total: model_mismatch and
// budget_exceeded count as failures, quota_parked as neither.
func TestScheduledRunsFollowTheVerdict(t *testing.T) {
	src := newFakeSource()
	src.add(core.Harness{Name: "sweep", Adapter: "generic", Schedule: "@daily"}, supervisor.Snapshot{State: core.StateStopped, Scheduled: true, Triggered: true})
	l := openLedger(t)
	m := newTestMetrics(t, src, Options{Runs: l})
	for i, o := range []string{"success", "model_mismatch", "budget_exceeded", "model_unattested", "quota_parked", "quota_parked"} {
		decide(t, l, "sweep", i+1, ledger.KindOneshot, o, t0, time.Second)
	}
	eventually(t, "every record counted", func() bool {
		v, _ := scrape(t, m).get("harness_runs_total", lbls("harness", "sweep", "kind", "oneshot", "outcome", "quota_parked"))
		return v == 2
	})
	fams := scrape(t, m)
	if v := fams.must(t, "harness_scheduled_runs_total", lbls("harness", "sweep", "outcome", "failure")); v != 3 {
		t.Errorf("failures = %v, want 3 (mismatch, budget, unattested)", v)
	}
	if v := fams.must(t, "harness_scheduled_runs_total", lbls("harness", "sweep", "outcome", "success")); v != 1 {
		t.Errorf("successes = %v, want 1", v)
	}
}

// REQ-11 "The CLI and the endpoint agree": the counter's increase equals the
// records the ledger query returns for the same window. The exposition is
// parsed with expfmt (scrape), never string-matched.
func TestRunsTotalAgreesWithTheLedgerQuery(t *testing.T) {
	src := newFakeSource()
	src.add(core.Harness{Name: "pr-review", Adapter: "generic", Triggers: []string{"webhook.gitea"}}, supervisor.Snapshot{State: core.StateStopped, Triggered: true})
	l := openLedger(t)
	m := newTestMetrics(t, src, Options{Runs: l})
	now := time.Now()
	for i := 1; i <= 12; i++ {
		o := "success"
		if i%4 == 0 {
			o = "failed"
		}
		decide(t, l, "pr-review", i, ledger.KindOneshot, o, now.Add(-time.Duration(i)*time.Hour), 3*time.Minute)
	}
	recs, _, err := l.Query(ledger.Query{Names: []string{"pr-review"}, Since: now.Add(-24 * time.Hour), Outcomes: []string{"failed"}})
	if err != nil {
		t.Fatal(err)
	}
	eventually(t, "the endpoint caught up", func() bool {
		v, _ := scrape(t, m).get("harness_runs_total", lbls("harness", "pr-review", "kind", "oneshot", "outcome", "success"))
		return v == 9
	})
	got := scrape(t, m).must(t, "harness_runs_total", lbls("harness", "pr-review", "kind", "oneshot", "outcome", "failed"))
	if len(recs) != 3 || got != float64(len(recs)) {
		t.Errorf("the query returned %d failed runs, the endpoint counted %v; want 3 and 3", len(recs), got)
	}
}

// REQ-11: every declared harness reports every kind/outcome pair it can
// produce, at zero, from the first scrape; the duration histogram too; tokens
// and cost are absent until a run carries them; the ledger's own health is
// exported.
func TestRunSeriesZerosAndLedgerHealth(t *testing.T) {
	src := newFakeSource()
	src.add(core.Harness{Name: "svc", Adapter: "generic"}, runningSnap())
	src.add(core.Harness{Name: "job", Adapter: "generic", Schedule: "@daily"}, supervisor.Snapshot{State: core.StateStopped, Scheduled: true, Triggered: true})
	l := openLedger(t)
	m := newTestMetrics(t, src, Options{Runs: l})

	fams := scrape(t, m)
	for _, o := range residentOutcomes {
		if v := fams.must(t, "harness_runs_total", lbls("harness", "svc", "kind", "resident", "outcome", o)); v != 0 {
			t.Errorf("svc %s = %v", o, v)
		}
	}
	for _, o := range oneshotOutcomes {
		fams.must(t, "harness_runs_total", lbls("harness", "job", "kind", "oneshot", "outcome", o))
	}
	if _, ok := fams.get("harness_runs_total", lbls("harness", "svc", "kind", "oneshot", "outcome", "missed")); ok {
		t.Error("a resident harness reports one-shot outcomes it cannot produce")
	}
	if _, ok := fams["harness_run_duration_seconds"]; !ok {
		t.Error("no duration histogram")
	}
	if _, ok := fams["harness_run_tokens_total"]; ok {
		t.Error("token series reported before any run carried tokens")
	}
	fams.must(t, "harness_ledger_append_errors_total", nil)
	fams.must(t, "harness_run_feed_dropped_total", lbls("subscriber", "metrics"))

	end := t0.Add(90 * time.Second)
	start := t0
	if _, err := l.Append(ledger.Line{Type: ledger.TypeDecided, At: end, Harness: "svc", RunID: 1, Record: ledger.Record{
		Kind: ledger.KindResident, Trigger: "autostart", Outcome: "failed", StartedAt: &start, EndedAt: &end,
		Tokens: &ledger.Tokens{Input: 100, Output: 7}, CostUSD: new(float64), CostSource: "recorded",
	}}, true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the resident run counted", func() bool {
		v, _ := scrape(t, m).get("harness_runs_total", lbls("harness", "svc", "kind", "resident", "outcome", "failed"))
		return v == 1
	})
	fams = scrape(t, m)
	if v := fams.must(t, "harness_run_tokens_total", lbls("harness", "svc", "type", "input")); v != 100 {
		t.Errorf("tokens input = %v", v)
	}
	if v := fams.must(t, "harness_ledger_bytes", nil); v <= 0 {
		t.Errorf("ledger bytes = %v after a write", v)
	}
	if _, ok := fams.get("harness_scheduled_runs_total", lbls("harness", "svc", "outcome", "failure")); ok {
		t.Error("a resident run reached harness_scheduled_runs_total")
	}
}

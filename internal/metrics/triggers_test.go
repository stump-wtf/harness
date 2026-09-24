package metrics

// Trigger Source Metrics tests.
//
// A stand-in source manager, scraped through Handler. cmd/harness drives the
// real one — real deliveries through the real listener, a real channel
// session against the fake server — and reads the same families; these cover
// the mapping and the label rules exhaustively where that is cheap.
//
// Governing: SPEC-0014 REQ "Trigger Metrics"; SPEC-0013 REQ-5, REQ-6.
//
// @joestump 09/24/2026 - Introduced with the SPEC-0014 trigger metrics (#480).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/trigger"
	"github.com/stump-wtf/harness/internal/trigger/source"
)

// fakeTriggers stands in for the source manager.
type fakeTriggers struct {
	mu       sync.Mutex
	statuses []source.Status
	counters *trigger.OutcomeCounters
	panic    bool
}

func newFakeTriggers(statuses ...source.Status) *fakeTriggers {
	return &fakeTriggers{statuses: statuses, counters: &trigger.OutcomeCounters{}}
}

func (f *fakeTriggers) Status() []source.Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.panic {
		panic("status read failed")
	}
	return append([]source.Status(nil), f.statuses...)
}

func (f *fakeTriggers) Counters() *trigger.OutcomeCounters { return f.counters }

func (f *fakeTriggers) set(statuses ...source.Status) {
	f.mu.Lock()
	f.statuses = statuses
	f.mu.Unlock()
}

func channelStatus(ref string, st trigger.SourceState) source.Status {
	return source.Status{Source: ref, Kind: "channel", State: st}
}

func webhookStatus(ref string, st trigger.SourceState) source.Status {
	return source.Status{Source: ref, Kind: "webhook", State: st}
}

const (
	famUp         = "harness_trigger_source_up"
	famEvents     = "harness_trigger_events_total"
	famLastEvent  = "harness_trigger_last_event_timestamp"
	famReconnects = "harness_trigger_reconnects_total"
)

// TestTriggerFamiliesZeroInitialized: two declared sources and not one event
// yet report the full label set — both kinds' up gauge, all seven outcomes
// each at 0, the channel's reconnects at 0 — and nothing else. The last-event
// timestamp is absent, not 1970.
func TestTriggerFamiliesZeroInitialized(t *testing.T) {
	trig := newFakeTriggers(channelStatus("channel.sb", trigger.StateConnected), webhookStatus("webhook.ci", trigger.StateListening))
	m := New(newFakeSource(), Options{Triggers: trig})
	fams := scrape(t, m)

	wantUp := []string{"{kind=channel,source=channel.sb}", "{kind=webhook,source=webhook.ci}"}
	if got := fams.series(famUp); strings.Join(got, " ") != strings.Join(wantUp, " ") {
		t.Errorf("%s = %v, want %v", famUp, got, wantUp)
	}
	var wantEvents []string
	for _, ref := range []string{"channel.sb", "webhook.ci"} {
		for _, o := range []string{"fired", "ignored", "duplicate", "unauthorized", "too_large", "rate_limited", "invalid"} {
			if v := fams.must(t, famEvents, lbls("source", ref, "outcome", o)); v != 0 {
				t.Errorf("%s{%s,%s} = %v, want 0", famEvents, ref, o, v)
			}
			wantEvents = append(wantEvents, "{outcome="+o+",source="+ref+"}")
		}
	}
	if got := fams.series(famEvents); len(got) != len(wantEvents) {
		t.Errorf("%s has %d series, want exactly %d: %v", famEvents, len(got), len(wantEvents), got)
	}
	if got := fams.series(famReconnects); strings.Join(got, " ") != "{source=channel.sb}" {
		t.Errorf("%s = %v, want the channel source only", famReconnects, got)
	}
	if _, ok := fams[famLastEvent]; ok {
		t.Errorf("%s present before any event: %v", famLastEvent, fams.series(famLastEvent))
	}
}

// TestTriggerCountsReachTheScrape: the counters' values, the last firing and
// the reconnects are what a scrape reads.
func TestTriggerCountsReachTheScrape(t *testing.T) {
	trig := newFakeTriggers(channelStatus("channel.sb", trigger.StateConnected), webhookStatus("webhook.ci", trigger.StateListening))
	at := time.Date(2026, 9, 24, 12, 0, 0, 500_000_000, time.UTC)
	trig.counters.Fired("webhook.ci", at)
	trig.counters.Inc("webhook.ci", trigger.OutcomeUnauthorized)
	trig.counters.Inc("webhook.ci", trigger.OutcomeUnauthorized)
	trig.counters.Inc("channel.sb", trigger.OutcomeInvalid)
	trig.counters.Reconnected("channel.sb")
	m := New(newFakeSource(), Options{Triggers: trig})
	fams := scrape(t, m)

	for _, c := range []struct {
		ref, outcome string
		want         float64
	}{
		{"webhook.ci", "fired", 1}, {"webhook.ci", "unauthorized", 2}, {"webhook.ci", "invalid", 0},
		{"channel.sb", "invalid", 1}, {"channel.sb", "fired", 0},
	} {
		if v := fams.must(t, famEvents, lbls("source", c.ref, "outcome", c.outcome)); v != c.want {
			t.Errorf("%s{%s,%s} = %v, want %v", famEvents, c.ref, c.outcome, v, c.want)
		}
	}
	if v, want := fams.must(t, famLastEvent, lbls("source", "webhook.ci")), float64(at.UnixNano())/1e9; v != want {
		t.Errorf("webhook.ci last event = %v, want %v", v, want)
	}
	if _, ok := fams.get(famLastEvent, lbls("source", "channel.sb")); ok {
		t.Error("channel.sb has a last-event timestamp and never fired")
	}
	if v := fams.must(t, famReconnects, lbls("source", "channel.sb")); v != 1 {
		t.Errorf("channel.sb reconnects = %v, want 1", v)
	}
}

// TestTriggerSourceUpIsOneOnlyWhileConnectedOrListening walks every source
// state through the gauge.
func TestTriggerSourceUpIsOneOnlyWhileConnectedOrListening(t *testing.T) {
	trig := newFakeTriggers()
	m := New(newFakeSource(), Options{Triggers: trig})
	for _, c := range []struct {
		st   trigger.SourceState
		want float64
	}{
		{trigger.StateConnected, 1}, {trigger.StateListening, 1},
		{trigger.StateConnecting, 0}, {trigger.StateBackoff, 0}, {trigger.StateError, 0},
		{trigger.StateNoListener, 0}, {trigger.StateDisabled, 0}, {trigger.StateUnbound, 0},
	} {
		trig.set(channelStatus("channel.sb", c.st))
		if v := scrape(t, m).must(t, famUp, lbls("source", "channel.sb", "kind", "channel")); v != c.want {
			t.Errorf("source_up in %s = %v, want %v", c.st, v, c.want)
		}
	}
}

// TestTriggerLabelsComeOnlyFromDeclaredSources: counter entries for a name
// the manager does not declare, and an outcome outside the closed set, never
// become series — the scrape's label values are the config's source names
// and the seven outcomes, whatever the counters hold.
func TestTriggerLabelsComeOnlyFromDeclaredSources(t *testing.T) {
	trig := newFakeTriggers(webhookStatus("webhook.ci", trigger.StateListening))
	const planted = "delivery-9f1c build 42 failed"
	trig.counters.Inc("webhook.gone", trigger.OutcomeFired)
	trig.counters.Inc(planted, trigger.OutcomeUnauthorized)
	trig.counters.Inc("webhook.ci", trigger.Outcome(planted))
	m := New(newFakeSource(), Options{Triggers: trig})

	fams := scrape(t, m)
	for _, fam := range []string{famUp, famEvents, famLastEvent, famReconnects} {
		for _, s := range fams.series(fam) {
			if !strings.Contains(s, "source=webhook.ci") {
				t.Errorf("%s carries an undeclared source: %s", fam, s)
			}
		}
	}
	if got := len(fams.series(famEvents)); got != len(trigger.Outcomes) {
		t.Errorf("%s has %d series, want %d", famEvents, got, len(trigger.Outcomes))
	}
	rec := scrapeBody(t, m)
	if strings.Contains(rec, "delivery-9f1c") || strings.Contains(rec, "webhook.gone") {
		t.Error("a counter key that is not a declared source or outcome reached the exposition")
	}
}

// TestTriggerSourceRemovedLosesItsSeries: a source the manager stops
// declaring (a reload removed or renamed it) is absent from the next scrape,
// and a rename's new name starts zero-initialized.
func TestTriggerSourceRemovedLosesItsSeries(t *testing.T) {
	trig := newFakeTriggers(channelStatus("channel.sb", trigger.StateConnected), webhookStatus("webhook.ci", trigger.StateListening))
	trig.counters.Fired("webhook.ci", time.Now())
	m := New(newFakeSource(), Options{Triggers: trig})
	if _, ok := scrape(t, m).get(famLastEvent, lbls("source", "webhook.ci")); !ok {
		t.Fatal("the check cannot fire: webhook.ci has no last-event series before the rename")
	}

	trig.set(webhookStatus("webhook.ci2", trigger.StateListening))
	fams := scrape(t, m)
	for _, fam := range []string{famUp, famEvents, famLastEvent, famReconnects} {
		for _, s := range fams.series(fam) {
			if strings.Contains(s, "source=webhook.ci,") || strings.HasSuffix(s, "source=webhook.ci}") || strings.Contains(s, "channel.sb") {
				t.Errorf("%s kept a series for a removed source: %s", fam, s)
			}
		}
	}
	if v := fams.must(t, famEvents, lbls("source", "webhook.ci2", "outcome", "fired")); v != 0 {
		t.Errorf("the renamed source starts at %v, want 0", v)
	}
}

// TestTriggerReadFailureIsACollectionError: a panicking status read omits the
// trigger families, counts collector="triggers", and leaves the rest of the
// scrape intact.
func TestTriggerReadFailureIsACollectionError(t *testing.T) {
	src := newFakeSource()
	src.add(crushHarness("worker"), runningSnap())
	trig := newFakeTriggers(webhookStatus("webhook.ci", trigger.StateListening))
	m := New(src, Options{Triggers: trig})
	if v := scrape(t, m).must(t, "harness_metrics_collection_errors_total", lbls("collector", "triggers")); v != 0 {
		t.Fatalf("triggers collection errors before any failure = %v", v)
	}

	trig.mu.Lock()
	trig.panic = true
	trig.mu.Unlock()
	scrape(t, m) // the failing scrape
	fams := scrape(t, m)
	if _, ok := fams[famUp]; ok {
		t.Error("trigger families present though the read failed")
	}
	if v := fams.must(t, "harness_metrics_collection_errors_total", lbls("collector", "triggers")); v != 2 {
		t.Errorf("triggers collection errors = %v, want 2 (one per failed scrape)", v)
	}
	fams.must(t, "harness_harness_state", lbls("harness", "worker", "state", "running"))
}

// TestTriggerFamiliesAbsentWithoutASourceManager: no manager, no series —
// and AttachTriggers after New supplies one.
func TestTriggerFamiliesAbsentWithoutASourceManager(t *testing.T) {
	m := New(newFakeSource(), Options{})
	for _, fam := range []string{famUp, famEvents, famLastEvent, famReconnects} {
		if _, ok := scrape(t, m)[fam]; ok {
			t.Errorf("%s present with no trigger source manager", fam)
		}
	}
	m.AttachTriggers(newFakeTriggers(webhookStatus("webhook.ci", trigger.StateListening)))
	scrape(t, m).must(t, famUp, lbls("source", "webhook.ci", "kind", "webhook"))
}

// scrapeBody is the raw exposition, for checks a parsed scrape cannot make.
func scrapeBody(t *testing.T, m *Metrics) string {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("scrape: status %d", rec.Code)
	}
	return rec.Body.String()
}

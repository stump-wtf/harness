package metrics

// Run Metrics From The Ledger
//
// Every run count on /metrics comes from committed run ledger records, read
// from the ledger's run feed (SPEC-0022 REQ-10, REQ-11). The lifecycle bus is
// lossy by design and still drives the TUI and clients; nothing here counts a
// run outcome from it, so a scrape and `harness runs` read the same facts.
//
// harness_scheduled_runs_total keeps SPEC-0013's name, labels and values; only
// its source moved: one-shot records, counted by RunOutcome.Verdict (success
// counts as success; failed, timed_out, budget_exceeded, model_mismatch and
// model_unattested as failure; everything else, quota_parked included, as
// neither; SPEC-0022 REQ-5).
//
// Tokens and cost are omitted, not zeroed, until a harness has a run that
// carried them: before stump.wtf/agent-trace#105 no record does, and a zero
// would read as "spent nothing" rather than "cannot see" (SPEC-0013 REQ-6).
//
// Governing: SPEC-0022 REQ-5, REQ-10, REQ-11; SPEC-0013 REQ-4, REQ-5, REQ-6;
// design "Metrics wiring".
//
// @joestump 09/24/2026 - Added for harness#450.

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stump-wtf/harness/internal/ledger"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// RunFeed is what the collector reads from the run ledger. *ledger.Ledger
// satisfies it.
type RunFeed interface {
	Subscribe(name string, buf int) (<-chan ledger.Committed, func())
	FeedDropped() map[string]uint64
	Stats() ledger.Stats
	Bytes() int64
}

// runsSubscriber is the collector's feed subscription, as it appears in
// harness_run_feed_dropped_total.
const runsSubscriber = "metrics"

// runsBuffer is sized for the acceptance load of SPEC-0022 REQ-11 (the
// collector only increments counters, so it drains far faster than runs can
// close); its drops are published, never hidden.
const runsBuffer = 8192

// collectorRuns names the run feed in harness_metrics_collection_errors_total.
const collectorRuns = "runs"

// runDurationBuckets span a failing spawn and a day-long resident (design
// "Metrics wiring"), in seconds.
var runDurationBuckets = []float64{1, 10, 30, 60, 300, 900, 1800, 3600, 3 * 3600, 12 * 3600, 24 * 3600}

// Outcomes each kind of run can close with, reported at zero from the first
// scrape (REQ-11: "every kind/outcome pair it can produce, including zeros").
var (
	oneshotOutcomes = []string{
		"success", "failed", "timed_out", "skipped", "replaced", "missed", "cancelled", "interrupted",
		"budget_exceeded", "quota_parked", "model_mismatch", "model_unattested",
	}
	residentOutcomes = []string{
		"success", "failed", "replaced", "cancelled", "interrupted",
		"budget_exceeded", "quota_parked", "model_mismatch", "model_unattested",
	}
)

var (
	descRuns = prometheus.NewDesc("harness_runs_total",
		"Closed runs from the run ledger, by kind (oneshot|resident) and outcome.",
		[]string{"harness", "kind", "outcome"}, nil)
	descRunDuration = prometheus.NewDesc("harness_run_duration_seconds",
		"Duration of closed runs with a known end, by kind.",
		[]string{"harness", "kind"}, nil)
	descRunTokens = prometheus.NewDesc("harness_run_tokens_total",
		"Tokens spent by closed runs, by type. Absent until a run records usage.",
		[]string{"harness", "type"}, nil)
	descRunCost = prometheus.NewDesc("harness_run_cost_usd_total",
		"Cost of closed runs in USD, by source (recorded|priced). Unknown cost is not counted.",
		[]string{"harness", "source"}, nil)
	descLedgerAppendErrors = prometheus.NewDesc("harness_ledger_append_errors_total",
		"Failed run ledger writes and syncs, retries included.", nil, nil)
	descLedgerBytes = prometheus.NewDesc("harness_ledger_bytes",
		"Total size of the run ledger's day files.", nil, nil)
	descRunFeedDropped = prometheus.NewDesc("harness_run_feed_dropped_total",
		"Run feed records a subscriber missed because its buffer was full.",
		[]string{"subscriber"}, nil)
)

var runDescs = []*prometheus.Desc{descRuns, descRunDuration, descRunTokens, descRunCost,
	descLedgerAppendErrors, descLedgerBytes, descRunFeedDropped}

// kindOutcome keys harness_runs_total.
type kindOutcome struct{ kind, outcome string }

// histo is one cumulative duration histogram.
type histo struct {
	count   uint64
	sum     float64
	buckets []uint64 // per runDurationBuckets, non-cumulative
}

func (h *histo) observe(sec float64) {
	if h.buckets == nil {
		h.buckets = make([]uint64, len(runDurationBuckets))
	}
	h.count++
	h.sum += sec
	for i, ub := range runDurationBuckets {
		if sec <= ub {
			h.buckets[i]++
			break
		}
	}
}

func (h histo) cumulative() map[float64]uint64 {
	out := make(map[float64]uint64, len(runDurationBuckets))
	var acc uint64
	for i, ub := range runDurationBuckets {
		if h.buckets != nil {
			acc += h.buckets[i]
		}
		out[ub] = acc
	}
	return out
}

// runSeries is one harness label's run counts.
type runSeries struct {
	runs      map[kindOutcome]uint64
	durations map[string]*histo // by kind
	tokens    [4]uint64         // input, output, cache_read, cache_write
	hasTokens bool
	cost      map[string]float64 // by source
}

func newRunSeries() *runSeries {
	return &runSeries{runs: map[kindOutcome]uint64{}, durations: map[string]*histo{}, cost: map[string]float64{}}
}

// subscribeRunsLocked subscribes to the run feed. Callers hold m.mu.
func (m *Metrics) subscribeRunsLocked() {
	ch, cancel := m.opts.Runs.Subscribe(runsSubscriber, runsBuffer)
	m.cancels = append(m.cancels, cancel)
	m.wg.Add(1)
	go m.consumeRuns(ch)
}

// consumeRuns counts every run the ledger commits as closed or decided.
func (m *Metrics) consumeRuns(ch <-chan ledger.Committed) {
	defer m.wg.Done()
	for c := range ch {
		if c.Type == ledger.TypeClosed || c.Type == ledger.TypeDecided {
			m.run(c.Record)
		}
	}
	m.feedClosed(collectorRuns)
}

// run counts one committed record.
func (m *Metrics) run(f ledger.Folded) {
	kind := f.Kind
	if kind == "" {
		kind = ledger.KindOneshot
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.seriesFor(f.Harness)
	rs := s.runSeries()
	rs.runs[kindOutcome{kind, f.Outcome}]++
	if kind == ledger.KindOneshot {
		// SPEC-0013 REQ-4's series, from the ledger now (REQ-5's verdict).
		switch supervisor.RunOutcome(f.Outcome).Verdict() {
		case 1:
			s.runs[0]++
		case -1:
			s.runs[1]++
		}
	}
	if f.StartedAt != nil && f.EndedAt != nil {
		h := rs.durations[kind]
		if h == nil {
			h = &histo{}
			rs.durations[kind] = h
		}
		h.observe(max(f.EndedAt.Sub(*f.StartedAt), 0).Seconds())
	}
	if t := f.Tokens; t != nil {
		rs.hasTokens = true
		rs.tokens[0] += uint64(max(t.Input, 0))
		rs.tokens[1] += uint64(max(t.Output, 0))
		rs.tokens[2] += uint64(max(t.CacheRead, 0))
		rs.tokens[3] += uint64(max(t.CacheWrite, 0))
	}
	if f.CostUSD != nil && (f.CostSource == ledger.CostRecorded || f.CostSource == ledger.CostPriced) {
		rs.cost[f.CostSource] += *f.CostUSD
	}
}

func (s *series) runSeries() *runSeries {
	if s.ledgerRuns == nil {
		s.ledgerRuns = newRunSeries()
	}
	return s.ledgerRuns
}

// runsSnapshot copies one label's run series for a scrape. Callers hold m.mu.
func (s *series) runsSnapshot() runSeries {
	out := *newRunSeries()
	if s == nil || s.ledgerRuns == nil {
		return out
	}
	src := s.ledgerRuns
	for k, v := range src.runs {
		out.runs[k] = v
	}
	for k, h := range src.durations {
		cp := *h
		cp.buckets = append([]uint64(nil), h.buckets...)
		out.durations[k] = &cp
	}
	out.tokens, out.hasTokens = src.tokens, src.hasTokens
	for k, v := range src.cost {
		out.cost[k] = v
	}
	return out
}

// collectRuns emits one harness label's run series. kinds are the kinds its
// declared harnesses produce.
func collectRuns(ch chan<- prometheus.Metric, lbl string, kinds map[string]bool, rs runSeries) {
	counter := func(d *prometheus.Desc, v float64, lv ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v, lv...)
	}
	emitted := map[kindOutcome]bool{}
	for kind := range kinds {
		outcomes := residentOutcomes
		if kind == ledger.KindOneshot {
			outcomes = oneshotOutcomes
		}
		for _, o := range outcomes {
			k := kindOutcome{kind, o}
			counter(descRuns, float64(rs.runs[k]), lbl, kind, o)
			emitted[k] = true
		}
	}
	// A kind the harness no longer produces (its config changed) still
	// reports what it counted.
	for k, v := range rs.runs {
		if !emitted[k] {
			counter(descRuns, float64(v), lbl, k.kind, k.outcome)
		}
	}
	for kind := range kinds {
		if _, ok := rs.durations[kind]; !ok {
			rs.durations[kind] = &histo{}
		}
	}
	for kind, h := range rs.durations {
		ch <- prometheus.MustNewConstHistogram(descRunDuration, h.count, h.sum, h.cumulative(), lbl, kind)
	}
	if rs.hasTokens {
		for i, typ := range []string{"input", "output", "cache_read", "cache_write"} {
			counter(descRunTokens, float64(rs.tokens[i]), lbl, typ)
		}
	}
	for _, src := range []string{ledger.CostRecorded, ledger.CostPriced} {
		if v, ok := rs.cost[src]; ok {
			counter(descRunCost, v, lbl, src)
		}
	}
}

// collectLedger emits the ledger's own health series.
func collectLedger(ch chan<- prometheus.Metric, f RunFeed) {
	st := f.Stats()
	ch <- prometheus.MustNewConstMetric(descLedgerAppendErrors, prometheus.CounterValue, float64(st.AppendErrors))
	ch <- prometheus.MustNewConstMetric(descLedgerBytes, prometheus.GaugeValue, float64(f.Bytes()))
	for sub, n := range f.FeedDropped() {
		ch <- prometheus.MustNewConstMetric(descRunFeedDropped, prometheus.CounterValue, float64(n), sub)
	}
}

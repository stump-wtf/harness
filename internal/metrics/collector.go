package metrics

// Scrape-Time Collector
//
// Every harness_* series is produced here, at scrape time, as a const metric:
// supervisor facts straight from the Manager's snapshots, event counts from
// the counters Metrics keeps. Series exist for the harnesses declared at the
// moment of the scrape and no others, so a removed harness goes stale like any
// vanished target instead of reporting a frozen last value forever.
//
// Zeros are deliberate where the value is known to be zero and omitted where it
// is unknown (SPEC-0013 REQ-2, REQ-6). Every declared harness reports all four
// state values, all seven transition targets and, when observable, both call
// outcomes and all five error classes — an absent series and a zero one are
// indistinguishable to an alert, and increase() needs a zero sample to count a
// series' first increment at all.
//
// Past the cardinality cap, harnesses share harness="__other__" (REQ-5), and
// the values combine the only way each can: counters and the state gauge sum
// (so __other__'s state value counts harnesses in that state), consecutive
// failures take the worst, the next scheduled run the soonest, and session
// activity and last success the most recent of any overflow harness.
//
// Governing: SPEC-0013 REQ-2..REQ-6; design.md "Where the numbers come from".
//
// @joestump-agent 09/21/2026 - Added for harness#356.

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/supervisor"
)

var (
	descState = prometheus.NewDesc("harness_harness_state",
		"1 for the harness's current state, 0 for the others (running|stopped|failed|flapping). Under harness=\"__other__\", the number of overflow harnesses in each state.",
		[]string{"harness", "state"}, nil)
	descRestarts = prometheus.NewDesc("harness_restarts_total",
		"Automatic restarts the supervisor has performed (the RESTARTS column of harness list).",
		[]string{"harness"}, nil)
	descConsecutive = prometheus.NewDesc("harness_consecutive_failures",
		"Failed exits since the last run that came up successfully; the supervisor gives up (state failed) once this exceeds its restart budget.",
		[]string{"harness"}, nil)
	descTransitions = prometheus.NewDesc("harness_state_transitions_total",
		"Lifecycle state transitions, by the supervisor state entered.",
		[]string{"harness", "to"}, nil)

	descCalls = prometheus.NewDesc("harness_model_calls_total",
		"Model calls observed in the agent's transcript: a tool call is a success, an agent error mark an error.",
		[]string{"harness", "outcome"}, nil)
	descErrors = prometheus.NewDesc("harness_model_call_errors_total",
		"Failed model calls by class (quota|auth|timeout|transport|other), classified where observed.",
		[]string{"harness", "class"}, nil)
	descUnclassified = prometheus.NewDesc("harness_model_call_errors_unclassified_total",
		"Failed model calls whose error matched no known shape (also counted as class=\"other\"). A rise means a provider changed its wording.",
		[]string{"harness"}, nil)
	descLastSuccess = prometheus.NewDesc("harness_last_successful_call_timestamp",
		"Unix time of the latest successful model call. Absent until the harness first succeeds in this daemon's lifetime.",
		[]string{"harness"}, nil)

	descSessionsStarted = prometheus.NewDesc("harness_sessions_started_total",
		"Agent sessions first seen active in this daemon's lifetime.",
		[]string{"harness"}, nil)
	descSessionActive = prometheus.NewDesc("harness_session_active",
		"1 while the harness's process is up and its agent wrote to a session within the idle window, else 0.",
		[]string{"harness"}, nil)
	descScheduledRuns = prometheus.NewDesc("harness_scheduled_runs_total",
		"Finished scheduled runs that passed a verdict, by outcome (success|failure).",
		[]string{"harness", "outcome"}, nil)
	descNextRun = prometheus.NewDesc("harness_scheduled_next_run_timestamp",
		"Unix time of the scheduled harness's next window. Absent when it has none.",
		[]string{"harness"}, nil)

	descRenderFailures = prometheus.NewDesc("harness_template_render_failures_total",
		"Spawns refused because a template could not be rendered, by reason (unresolved|grammar). Each is a skipped run or a failed start; nothing was exec'd.",
		[]string{"harness", "reason"}, nil)

	descCollectionErrors = prometheus.NewDesc("harness_metrics_collection_errors_total",
		"Failures to collect a metric family, by collector. Non-zero means some series are missing or undercounted, not zero.",
		[]string{"collector"}, nil)
	descOverflowed = prometheus.NewDesc("harness_metrics_harnesses_overflowed",
		"Declared harnesses folded into harness=\"__other__\" because the label cap was reached.",
		nil, nil)

	descObsDelivered = prometheus.NewDesc("harness_observer_events_delivered_total",
		"Agent items the observer delivered to its subscribers.", nil, nil)
	descObsDropped = prometheus.NewDesc("harness_observer_events_dropped_total",
		"Agent items a subscriber lost because its buffer was full.", []string{"subscriber"}, nil)
	descObsAmbiguous = prometheus.NewDesc("harness_observer_items_ambiguous_total",
		"Agent items withheld because more than one harness could have written their session.", nil, nil)
	descObsUnattributed = prometheus.NewDesc("harness_observer_items_unattributed_total",
		"Agent items read from a tracked session that no harness could claim when they were read.", nil, nil)
	descObsParseErrors = prometheus.NewDesc("harness_observer_parse_errors_total",
		"Failed session reads, by agent-trace adapter.", []string{"adapter"}, nil)
	descObsScanErrors = prometheus.NewDesc("harness_observer_scan_errors_total",
		"Failed agent session store listings.", nil, nil)
	descObsSessions = prometheus.NewDesc("harness_observer_sessions",
		"Agent sessions the observer is tracking.", nil, nil)
	descObsContested = prometheus.NewDesc("harness_observer_sessions_contested",
		"Tracked sessions that were ambiguous at their latest read.", nil, nil)
	descObsLastScan = prometheus.NewDesc("harness_observer_last_scan_timestamp",
		"Unix time the observer's latest scan began. Absent before the first.", nil, nil)
)

var allDescs = []*prometheus.Desc{
	descState, descRestarts, descConsecutive, descTransitions,
	descCalls, descErrors, descUnclassified, descLastSuccess,
	descSessionsStarted, descSessionActive, descScheduledRuns, descNextRun,
	descRenderFailures,
	descCollectionErrors, descOverflowed,
	descObsDelivered, descObsDropped, descObsAmbiguous, descObsUnattributed,
	descObsParseErrors, descObsScanErrors, descObsSessions, descObsContested, descObsLastScan,
}

// collector is Metrics' prometheus.Collector.
type collector struct{ m *Metrics }

func (c *collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range allDescs {
		ch <- d
	}
}

// row is one declared harness as read for this scrape.
type row struct {
	snap   supervisor.Snapshot
	def    core.Harness
	next   time.Time
	nextOK bool
}

// agg is one harness label's scrape-time supervisor facts; more than one row
// feeds it only under OverflowLabel.
type agg struct {
	states     map[string]float64
	restarts   float64
	consec     float64
	up         bool
	observable bool
	// errorsObservable: the adapter's provider errors reach the observer
	// too (ErrorsObservable), so the error side is computable.
	errorsObservable bool
	scheduled        bool
	next             time.Time
	// templated: a command harness, whose argv is rendered at spawn, so its
	// render failures are known (zero until one happens) rather than
	// meaningless.
	templated bool
}

func (c *collector) Collect(ch chan<- prometheus.Metric) {
	m := c.m
	now := m.opts.Now()

	// Everything that takes another component's lock happens before m.mu:
	// the Manager, the scheduler and the observer each answer from their own
	// short critical sections, and none of them waits on this one.
	snaps, snapsOK := m.snapshots()
	rows := make([]row, 0, len(snaps))
	for _, s := range snaps {
		rw := row{snap: s}
		rw.def, _ = m.harnessDef(s.Name)
		if s.Scheduled {
			rw.next, rw.nextOK = m.nextRun(s.Name)
		}
		rows = append(rows, rw)
	}
	st, stOK := m.observerStats()

	m.mu.Lock()
	if snapsOK {
		declared := make(map[string]bool, len(rows))
		for _, rw := range rows {
			declared[rw.snap.Name] = true
		}
		for _, gone := range m.labels.retain(declared) {
			delete(m.per, gone)
		}
	}
	aggs := make(map[string]*agg)
	var order []string
	overflowed := 0
	for _, rw := range rows {
		lbl := m.labels.label(rw.snap.Name)
		if lbl == OverflowLabel {
			overflowed++
		}
		a, ok := aggs[lbl]
		if !ok {
			a = &agg{states: make(map[string]float64, len(stateValues))}
			aggs[lbl] = a
			order = append(order, lbl)
		}
		a.states[StateValue(rw.snap)]++
		a.restarts += float64(rw.snap.RestartCount)
		if f := float64(rw.snap.ConsecutiveFailures); f > a.consec {
			a.consec = f
		}
		a.up = a.up || rw.snap.PID != 0
		a.observable = a.observable || Observable(rw.def)
		a.errorsObservable = a.errorsObservable || ErrorsObservable(rw.def)
		a.templated = a.templated || rw.def.Adapter == core.AdapterCommand
		if rw.snap.Scheduled {
			a.scheduled = true
			if rw.nextOK && !rw.next.IsZero() && (a.next.IsZero() || rw.next.Before(a.next)) {
				a.next = rw.next
			}
		}
	}
	counted := make(map[string]series, len(order))
	for _, lbl := range order {
		if s, ok := m.per[lbl]; ok {
			cp := *s
			cp.errors = make(map[Class]uint64, len(s.errors))
			for k, v := range s.errors {
				cp.errors[k] = v
			}
			cp.transitions = make(map[core.State]uint64, len(s.transitions))
			for k, v := range s.transitions {
				cp.transitions[k] = v
			}
			cp.renderFailures = make(map[string]uint64, len(s.renderFailures))
			for k, v := range s.renderFailures {
				cp.renderFailures[k] = v
			}
			counted[lbl] = cp
		}
	}
	if stOK {
		// Every item this subscriber lost is a model call or session the
		// counters above will never see: the counts are low, not true.
		if d := st.Dropped[subscriberName]; d > m.droppedSeen {
			m.collErrs[collectorObserver] += d - m.droppedSeen
			m.droppedSeen = d
		}
	}
	if m.lifecycleDrops != nil {
		// Every lifecycle event this subscriber lost is a transition or run
		// outcome the counters above will never see. The bus counts the loss
		// atomically; it is read here, like the observer's, and published
		// under the collector it undercounts.
		if d := m.lifecycleDrops(); d > m.lifecycleSeen {
			m.collErrs[collectorLifecycle] += d - m.lifecycleSeen
			m.lifecycleSeen = d
		}
	}
	reachable := m.opts.Observer != nil && !m.observerDead
	collErrs := make(map[string]uint64, len(m.collErrs))
	for k, v := range m.collErrs {
		collErrs[k] = v
	}
	m.mu.Unlock()

	counter := func(d *prometheus.Desc, v float64, lv ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.CounterValue, v, lv...)
	}
	gauge := func(d *prometheus.Desc, v float64, lv ...string) {
		ch <- prometheus.MustNewConstMetric(d, prometheus.GaugeValue, v, lv...)
	}
	unix := func(t time.Time) float64 { return float64(t.UnixNano()) / 1e9 }

	for _, lbl := range order {
		a := aggs[lbl]
		s := counted[lbl] // zero value when nothing has been counted yet
		for _, v := range stateValues {
			gauge(descState, a.states[v], lbl, v)
		}
		counter(descRestarts, a.restarts, lbl)
		gauge(descConsecutive, a.consec, lbl)
		for _, to := range core.States {
			counter(descTransitions, float64(s.transitions[to]), lbl, string(to))
		}

		if a.observable && reachable {
			counter(descCalls, float64(s.calls[0]), lbl, outcomeSuccess)
			// Omitted, not zeroed, where the adapter's errors never reach
			// the observer (ErrorsObservable, REQ-6).
			if a.errorsObservable {
				counter(descCalls, float64(s.calls[1]), lbl, outcomeError)
				for _, cl := range Classes {
					counter(descErrors, float64(s.errors[cl]), lbl, string(cl))
				}
				counter(descUnclassified, float64(s.unclassified), lbl)
			}
			// Omitted, not zeroed, until the first success (REQ-3): a zero
			// reads as 1970 and puts 56 years of staleness on every panel.
			if !s.lastSuccess.IsZero() {
				gauge(descLastSuccess, unix(s.lastSuccess), lbl)
			}
			counter(descSessionsStarted, float64(s.sessionsStarted), lbl)
			active := 0.0
			if a.up && !s.lastItem.IsZero() && now.Sub(s.lastItem) <= m.opts.SessionIdle {
				active = 1
			}
			gauge(descSessionActive, active, lbl)
		}

		if a.scheduled {
			counter(descScheduledRuns, float64(s.runs[0]), lbl, outcomeSuccess)
			counter(descScheduledRuns, float64(s.runs[1]), lbl, outcomeFailure)
			if !a.next.IsZero() {
				gauge(descNextRun, unix(a.next), lbl)
			}
		}

		// Both reasons, zero until one happens, for every harness whose
		// argv is a template — and for any other harness a failure was
		// somehow counted against, so a count is never hidden.
		// Governing: SPEC-0017 REQ-11 "Rendering".
		if a.templated || len(s.renderFailures) > 0 {
			for _, r := range renderFailureReasons {
				counter(descRenderFailures, float64(s.renderFailures[r]), lbl, r)
			}
		}
	}

	for _, n := range collectorNames {
		counter(descCollectionErrors, float64(collErrs[n]), n)
	}
	if snapsOK {
		gauge(descOverflowed, float64(overflowed))
	}

	if stOK {
		counter(descObsDelivered, float64(st.Delivered))
		for sub, n := range st.Dropped {
			counter(descObsDropped, float64(n), sub)
		}
		counter(descObsAmbiguous, float64(st.Ambiguous))
		counter(descObsUnattributed, float64(st.Unattributed))
		for adapter, n := range st.ParseErrors {
			counter(descObsParseErrors, float64(n), adapter)
		}
		counter(descObsScanErrors, float64(st.ScanErrors))
		gauge(descObsSessions, float64(st.Sessions))
		gauge(descObsContested, float64(st.Contested))
		if !st.LastScan.IsZero() {
			gauge(descObsLastScan, unix(st.LastScan))
		}
	}
}

package metrics

// Trigger Source Metrics
//
// "The listener has been down for an hour" should be an alert rule, not a
// surprise (ADR-0021). These four families put the trigger sources of
// SPEC-0014 on the same scrape as the harnesses they fire:
//
//	harness_trigger_source_up{source,kind}          1 while connected or listening
//	harness_trigger_events_total{source,outcome}    how each delivery or doorbell ended
//	harness_trigger_last_event_timestamp{source}    when the source last fired
//	harness_trigger_reconnects_total{source}        channel sources only
//
// Same mechanism as the harness families: read at scrape time, never mirrored
// into a gauge. The state comes from the source manager's own status (the one
// `harness triggers` reports), and the counts from the trigger.OutcomeCounters
// the manager shares with the webhook listener. So `source_up` flips on the
// transition the channel session itself observed, and there is no second
// copy of it to forget to update.
//
// Cardinality: a `source` label value is only ever a source the config
// declares right now — the manager's status list — never a key read out of
// the counters, and `outcome` only ever one of trigger.Outcomes. A delivery to
// an unknown route is a 404 before anything counts it, and even a counter
// entry for a name the config does not declare (a delivery in flight across
// a reload) is never emitted. So no request can mint a series, and no
// delivery ID or event text can reach a label: neither is an input to one.
// Source names are bounded by the config, which is why they take no
// __other__ cap of their own (SPEC-0014 REQ "Trigger Metrics").
//
// Every declared source reports every outcome, zeros included, from the first
// scrape: absence and zero are indistinguishable to an alert, and increase()
// needs a zero sample to see a first increment. A source removed or renamed
// by a reload disappears from the next scrape, like a removed harness. The
// last-event timestamp is omitted until the source first fires rather than
// reported as 1970 (SPEC-0013 REQ-6).
//
// Governing: ADR-0021; SPEC-0014 REQ "Trigger Metrics"; SPEC-0013 REQ-5,
// REQ-6.
//
// @joestump 09/24/2026 - Introduced with the SPEC-0014 trigger metrics (#480).

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/trigger"
	"github.com/stump-wtf/harness/internal/trigger/source"
)

// TriggerSource is the trigger source manager. *source.Manager satisfies it.
type TriggerSource interface {
	// Status is every declared source's state.
	Status() []source.Status
	// Counters are the per-source counts shared with the webhook listener.
	Counters() *trigger.OutcomeCounters
}

var (
	descTriggerUp = prometheus.NewDesc("harness_trigger_source_up",
		"1 while the trigger source is connected (channel) or listening (webhook), else 0.",
		[]string{"source", "kind"}, nil)
	descTriggerEvents = prometheus.NewDesc("harness_trigger_events_total",
		"Webhook deliveries and channel doorbells by how they ended (fired|ignored|duplicate|unauthorized|too_large|rate_limited|invalid).",
		[]string{"source", "outcome"}, nil)
	descTriggerLastEvent = prometheus.NewDesc("harness_trigger_last_event_timestamp",
		"Unix time the trigger source last fired. Absent until it first fires in this daemon's lifetime.",
		[]string{"source"}, nil)
	descTriggerReconnects = prometheus.NewDesc("harness_trigger_reconnects_total",
		"Times a channel source's stream came back to connected after its first connection.",
		[]string{"source"}, nil)
)

var triggerDescs = []*prometheus.Desc{descTriggerUp, descTriggerEvents, descTriggerLastEvent, descTriggerReconnects}

// triggerRow is one declared source as read for this scrape.
type triggerRow struct {
	status source.Status
	counts trigger.SourceCounts
}

// AttachTriggers supplies the trigger source manager after New, for a daemon
// that builds it after the collector. A nil argument, or one after Close,
// does nothing; one already attached is kept.
func (m *Metrics) AttachTriggers(t TriggerSource) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || t == nil || m.opts.Triggers != nil {
		return
	}
	m.opts.Triggers = t
}

// triggerRows reads every declared source and its counts, turning a panic
// into a collection error rather than a dead scrape. ok is false when there
// is no trigger source, or reading it failed: the families are then omitted,
// not zeroed.
func (m *Metrics) triggerRows() (rows []triggerRow, ok bool) {
	m.mu.Lock()
	t := m.opts.Triggers // AttachTriggers may set it after New
	m.mu.Unlock()
	if t == nil {
		return nil, false
	}
	defer func() {
		if r := recover(); r != nil {
			m.countError(collectorTriggers)
			rows, ok = nil, false
		}
	}()
	statuses := t.Status()
	counters := t.Counters()
	rows = make([]triggerRow, 0, len(statuses))
	for _, st := range statuses {
		rw := triggerRow{status: st}
		if counters != nil {
			rw.counts = counters.Counts(st.Source)
		}
		rows = append(rows, rw)
	}
	return rows, true
}

// sourceUp is REQ "Trigger Metrics"'s definition: 1 only while connected or
// listening. Connecting, backoff, error, no_listener, disabled and unbound
// are all 0 — none of them can hear an event.
func sourceUp(s trigger.SourceState) float64 {
	if s == trigger.StateConnected || s == trigger.StateListening {
		return 1
	}
	return 0
}

// collectTriggers emits the trigger families for rows.
func collectTriggers(ch chan<- prometheus.Metric, rows []triggerRow) {
	for _, rw := range rows {
		ref := rw.status.Source
		ch <- prometheus.MustNewConstMetric(descTriggerUp, prometheus.GaugeValue, sourceUp(rw.status.State), ref, rw.status.Kind)
		for _, o := range trigger.Outcomes {
			ch <- prometheus.MustNewConstMetric(descTriggerEvents, prometheus.CounterValue, float64(rw.counts.Outcomes[o]), ref, string(o))
		}
		if !rw.counts.LastEvent.IsZero() {
			ch <- prometheus.MustNewConstMetric(descTriggerLastEvent, prometheus.GaugeValue, float64(rw.counts.LastEvent.UnixNano())/1e9, ref)
		}
		if rw.status.Kind == core.SourceKindChannel {
			ch <- prometheus.MustNewConstMetric(descTriggerReconnects, prometheus.CounterValue, float64(rw.counts.Reconnects), ref)
		}
	}
}

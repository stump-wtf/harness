package telemetry

// Trace Accumulation
//
// The traces signal exports one trace per agent session (SPEC-0014 REQ-7).
// Items arrive one at a time and a session never "ends", so the accumulator
// keeps each session's unsent items and exports them as spans on a trigger:
// the session going idle for idle_flush, 2048 unsent items, its harness
// leaving `running`, being evicted as the least recently active of 256
// tracked sessions, or daemon shutdown.
//
// An export runs agent-trace's otel.BuildTrace over the unsent items, plus the
// current turn's user-message mark when that was sent earlier — kept as
// parenting context, so a tool call arriving after an idle export still hangs
// under its turn. BuildTrace names spans by a per-build counter; every span is
// then re-keyed to its item's ID (record.go), which depends on BuildTrace
// producing exactly one span per mapped item, in timeline order. That is
// checked on every build — a mismatch is counted failed and logged, never
// exported mis-keyed — and pinned by a test against the vendored version.
// Spans whose IDs were already sent (the retained turn) are filtered out, so a
// span is never re-sent and its fields freeze at first export.
//
// Missing timestamps are filled from the observer's Time before the build, so
// a span starts when its log record says it happened. Error marks get no span:
// BuildTrace maps none, and the logs signal carries them.
//
// The accumulator is owned by the traces intake goroutine; nothing here locks.
//
// Governing: ADR-0021; SPEC-0014 REQ-4, REQ-7.
//
// @joestump-agent 09/21/2026 - Added for harness#391.
//
// @joestump-agent 09/21/2026 - review: span source text is redacted before
// BuildTrace truncates it, not only after.

import (
	"sort"
	"time"

	"charm.land/log/v2"
	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/otel"
	"github.com/stump-wtf/agent-trace/tail"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/observe"
	"github.com/stump-wtf/harness/internal/redact"
)

// Accumulator bounds (SPEC-0014 REQ-7).
const (
	traceMaxUnsent   = 2048
	traceMaxSessions = 256
	traceForget      = 24 * time.Hour
)

type traceItem struct {
	ev observe.Event // timestamp filled, prompt applied
	id string
}

type traceSession struct {
	key     string
	meta    tail.SessionMeta
	harness string
	adapter string

	unsent []traceItem
	// turn is the current turn's user-message mark; turnSent says whether it
	// already went out, in which case it rides along only as context.
	turn     *traceItem
	turnSent bool
	// sent holds span IDs already exported that could be built again — the
	// retained turn is the only one; bounded by construction.
	sent map[string]struct{}
	last time.Time
}

type traceAccumulator struct {
	conv        *converter
	omitPrompts bool
	idle        time.Duration
	now         func() time.Time
	running     func(harness string) bool
	emit        func([]otel.Span)
	stats       *signalStats
	log         *log.Logger

	sessions map[string]*traceSession
}

// add takes one contributing item.
func (a *traceAccumulator) add(ev observe.Event) {
	now := a.now()
	s := a.sessions[ev.Session.Key]
	if s == nil {
		if len(a.sessions) >= traceMaxSessions {
			a.evictOldest()
		}
		s = &traceSession{key: ev.Session.Key, sent: map[string]struct{}{}}
		a.sessions[ev.Session.Key] = s
	}
	s.meta, s.harness, s.adapter, s.last = ev.Session, ev.Harness, ev.Adapter, now

	_, id := a.conv.itemIDs(ev)
	ev = a.prepare(ev)
	it := traceItem{ev: ev, id: id}
	if ev.Kind == observe.KindMark && ev.Mark.Type == "user-message" {
		s.turn, s.turnSent = &it, false
		// A new turn: nothing older can be rebuilt any more.
		s.sent = map[string]struct{}{}
	}
	s.unsent = append(s.unsent, it)
	if len(s.unsent) >= traceMaxUnsent {
		a.flush(s)
	}
}

// prepare fills a missing timestamp from the observer's Time, applies
// omit_prompts and redacts the text a span is named from, before BuildTrace
// sees the item. Redaction has to come first: BuildTrace truncates a
// user-message span name to 128 runes, and a credential cut short there no
// longer matches its pattern, so cleanSpan alone would export its head.
func (a *traceAccumulator) prepare(ev observe.Event) observe.Event {
	fill := ev.Time.UTC().Format(time.RFC3339Nano)
	switch ev.Kind {
	case observe.KindTool:
		if !parseableTime(ev.Tool.Timestamp) {
			ev.Tool.Timestamp = fill
		}
		ev.Tool.Summary = redact.String(ev.Tool.Summary)
	case observe.KindMark:
		if !parseableTime(ev.Mark.Timestamp) {
			ev.Mark.Timestamp = fill
		}
		ev.Mark.Note = redact.String(ev.Mark.Note)
		if a.omitPrompts && ev.Mark.Type == "user-message" {
			ev.Mark.Note = PromptOmitted
		}
	}
	return ev
}

// parseableTime mirrors agent-trace's own timestamp parse.
func parseableTime(ts string) bool {
	if ts == "" {
		return false
	}
	if _, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return true
	}
	_, err := time.Parse(time.RFC3339, ts)
	return err == nil
}

// tick applies the time- and state-based triggers.
func (a *traceAccumulator) tick() {
	now := a.now()
	for key, s := range a.sessions {
		idle := now.Sub(s.last)
		if len(s.unsent) > 0 && (idle >= a.idle || (a.running != nil && !a.running(s.harness))) {
			a.flush(s)
		}
		if idle >= traceForget {
			a.flush(s)
			delete(a.sessions, key)
		}
	}
}

// flushAll exports every session's unsent spans (shutdown).
func (a *traceAccumulator) flushAll() {
	for _, s := range a.sessions {
		a.flush(s)
	}
}

func (a *traceAccumulator) evictOldest() {
	var oldest *traceSession
	for _, s := range a.sessions {
		if oldest == nil || s.last.Before(oldest.last) {
			oldest = s
		}
	}
	if oldest != nil {
		a.flush(oldest)
		delete(a.sessions, oldest.key)
	}
}

// flush builds and emits s's unsent spans.
func (a *traceAccumulator) flush(s *traceSession) {
	if len(s.unsent) == 0 {
		return
	}
	items := s.unsent
	s.unsent = nil
	if s.turn != nil && s.turnSent {
		items = append([]traceItem{*s.turn}, items...)
	}
	spans, ok := buildSpans(s.meta, items)
	if !ok {
		a.stats.failed.Add(uint64(len(items)))
		a.log.Error("telemetry: agent-trace built a different number of spans than mapped items; not exporting this batch rather than mis-keying it",
			"session", s.meta.ID, "harness", s.harness)
		return
	}
	out := spans[:0]
	for _, sp := range spans {
		if _, dup := s.sent[sp.SpanID]; dup {
			continue
		}
		sp.Attributes["harness.name"] = s.harness
		sp.Attributes["agent.adapter"] = s.adapter
		sp.Attributes["agent.item.id"] = sp.SpanID
		out = append(out, cleanSpan(sp))
	}
	if s.turn != nil {
		s.turnSent = true
		s.sent = map[string]struct{}{s.turn.id: {}}
	}
	if len(out) > 0 {
		a.emit(out)
	}
}

// buildSpans runs otel.BuildTrace over items and re-keys its spans to the
// items' IDs. ok is false when the one-span-per-mapped-item correspondence
// does not hold.
func buildSpans(meta tail.SessionMeta, items []traceItem) ([]otel.Span, bool) {
	// Hand BuildTrace marks then events, each in item order, and replay its
	// own stable sort to learn which item each span came from.
	type entry struct {
		seq    int
		isMark bool
		item   traceItem
	}
	var marks []classify.Mark
	var events []classify.Event
	var timeline []entry
	for _, it := range items {
		if it.ev.Kind == observe.KindMark {
			marks = append(marks, it.ev.Mark)
		}
	}
	for _, it := range items {
		if it.ev.Kind == observe.KindMark {
			timeline = append(timeline, entry{it.ev.Mark.Seq, true, it})
		}
	}
	for _, it := range items {
		if it.ev.Kind == observe.KindTool {
			events = append(events, it.ev.Tool)
			timeline = append(timeline, entry{it.ev.Tool.Seq, false, it})
		}
	}
	sort.SliceStable(timeline, func(i, j int) bool {
		if timeline[i].seq != timeline[j].seq {
			return timeline[i].seq < timeline[j].seq
		}
		return timeline[i].isMark && !timeline[j].isMark
	})
	var ids []string
	for _, e := range timeline {
		if mapsToSpan(e.item.ev) {
			ids = append(ids, e.item.id)
		}
	}

	trace := otel.BuildTrace(meta, events, marks)
	if len(trace.Spans) != len(ids) {
		return nil, false
	}
	remap := make(map[string]string, len(ids))
	for i, sp := range trace.Spans {
		remap[sp.SpanID] = ids[i]
	}
	for i := range trace.Spans {
		sp := &trace.Spans[i]
		sp.SpanID = remap[sp.SpanID]
		if sp.ParentSpanID != "" {
			sp.ParentSpanID = remap[sp.ParentSpanID]
		}
		if sp.Attributes == nil {
			sp.Attributes = map[string]any{}
		}
	}
	return trace.Spans, true
}

// cleanSpan redacts and caps every string a span carries (REQ-4).
func cleanSpan(sp otel.Span) otel.Span {
	sp.Name = clean(sp.Name, spanNameCap)
	sp.StatusMsg = clean(sp.StatusMsg, attrCap)
	for k, v := range sp.Attributes {
		if s, ok := v.(string); ok {
			sp.Attributes[k] = clean(s, attrCap)
		}
	}
	return sp
}

// runningFunc adapts a Source to "is this harness still running".
func runningFunc(src Source) func(string) bool {
	return func(name string) bool {
		snap, ok := src.Snapshot(name)
		return ok && snap.State == core.StateRunning
	}
}

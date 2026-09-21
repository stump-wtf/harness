package telemetry

// Trace Accumulation Tests
//
// Governing tests: SPEC-0014 REQ-7 — trace ID equal to BuildTrace's, item-keyed
// span IDs pinned against the vendored agent-trace, no span ever re-sent across
// idle exports or a daemon restart, the export triggers and bounds, and no
// invented span for an error mark.
//
// @joestump-agent 09/21/2026 - Added for harness#391.

import (
	"strconv"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/otel"

	"github.com/stump-wtf/harness/internal/observe"
)

type spanSink struct{ batches [][]otel.Span }

func (s *spanSink) emit(sp []otel.Span) {
	s.batches = append(s.batches, append([]otel.Span(nil), sp...))
}

func (s *spanSink) all() []otel.Span {
	var out []otel.Span
	for _, b := range s.batches {
		out = append(out, b...)
	}
	return out
}

func newAcc(now *time.Time, running func(string) bool) (*traceAccumulator, *spanSink) {
	sink := &spanSink{}
	clock := func() time.Time { return *now }
	return &traceAccumulator{
		conv: &converter{ids: newIDRegistry(clock)}, idle: 5 * time.Minute, now: clock,
		running: running, emit: sink.emit, stats: &signalStats{}, log: quietLogger(),
		sessions: map[string]*traceSession{},
	}, sink
}

func TestTraceIDMatchesBuildTrace(t *testing.T) {
	for _, key := range []string{"", "k", "/home/u/.claude/projects/x/abc.jsonl", "crush:1234"} {
		if got, want := TraceID(key), otel.BuildTrace(session(key), nil, nil).TraceID; got != want {
			t.Fatalf("TraceID(%q) = %s, BuildTrace says %s", key, got, want)
		}
	}
}

// Re-keying depends on BuildTrace making exactly one span per mapped item, in
// timeline order. This pins that against the vendored version with every mark
// type, including two BuildTrace maps to nothing.
func TestMapsToSpanMatchesBuildTrace(t *testing.T) {
	conv := &converter{ids: newIDRegistry(time.Now)}
	var items []traceItem
	add := func(ev observe.Event) {
		_, id := conv.itemIDs(ev)
		items = append(items, traceItem{ev: ev, id: id})
	}
	add(markEv("w", "k", 0, "user-message", "go", t0))
	add(toolEv("w", "k", 0, "a", false, t0))
	add(markEv("w", "k", 1, "error", "boom", t0))
	add(markEv("w", "k", 1, "compaction", "", t0))
	add(toolEv("w", "k", 1, "b", true, t0))
	add(markEv("w", "k", 2, "subagent", "helper", t0))
	add(markEv("w", "k", 2, "some-future-type", "x", t0))
	add(toolEv("w", "k", 2, "c", false, t0))
	add(markEv("w", "k", 3, "user-message", "again", t0))

	mapped := 0
	for _, it := range items {
		if mapsToSpan(it.ev) {
			mapped++
		}
	}
	spans, ok := buildSpans(session("k"), items)
	if !ok || len(spans) != mapped {
		t.Fatalf("BuildTrace made %d spans for %d mapped items (ok=%v): agent-trace's mapping changed; update mapsToSpan", len(spans), mapped, ok)
	}
	// The IDs are the items' own, in timeline order, and parents point at
	// re-keyed turn IDs.
	wantOrder := []string{items[0].id, items[1].id, items[3].id, items[4].id, items[5].id, items[7].id, items[8].id}
	for i, sp := range spans {
		if sp.SpanID != wantOrder[i] {
			t.Fatalf("span %d (%s) id %s, want %s", i, sp.Name, sp.SpanID, wantOrder[i])
		}
	}
	if spans[1].ParentSpanID != items[0].id || spans[5].ParentSpanID != items[0].id {
		t.Fatalf("children not parented under the re-keyed turn: %+v", spans)
	}
}

func TestResumedSessionSendsOnlyNewSpans(t *testing.T) {
	now := t0
	acc, sink := newAcc(&now, nil)
	acc.add(markEv("w", "k", 0, "user-message", "turn", t0))
	for i := range 5 {
		acc.add(toolEv("w", "k", i, "tool"+strconv.Itoa(i), false, t0.Add(time.Duration(i+1)*time.Second)))
	}
	now = now.Add(5 * time.Minute)
	acc.tick()
	if len(sink.batches) != 1 || len(sink.batches[0]) != 6 {
		t.Fatalf("idle export: %d batches, want one of 6 spans", len(sink.batches))
	}
	first := sink.batches[0]
	turnID := first[0].SpanID
	turnEnd := first[0].EndTime

	acc.add(toolEv("w", "k", 5, "late tool", false, t0.Add(time.Hour)))
	now = now.Add(5 * time.Minute)
	acc.tick()
	if len(sink.batches) != 2 || len(sink.batches[1]) != 1 {
		t.Fatalf("resume export: %v, want one batch of exactly one span", sink.batches)
	}
	late := sink.batches[1][0]
	if late.ParentSpanID != turnID {
		t.Fatalf("late tool parent %s, want the turn sent earlier %s", late.ParentSpanID, turnID)
	}
	if late.SpanID == turnID {
		t.Fatal("the turn span was re-sent")
	}
	if first[0].EndTime != turnEnd {
		t.Fatal("the already-sent turn span changed")
	}
}

func TestDaemonRestartProducesNoOverlappingSpanIDs(t *testing.T) {
	now := t0
	life1, sink1 := newAcc(&now, nil)
	life1.add(markEv("w", "k", 0, "user-message", "turn", t0))
	for i := range 3 {
		life1.add(toolEv("w", "k", i, "t", false, t0))
	}
	life1.flushAll()

	// A new daemon: fresh registry and accumulator, no replay; the session
	// continues at later seqs.
	life2, sink2 := newAcc(&now, nil)
	for i := 3; i < 6; i++ {
		life2.add(toolEv("w", "k", i, "t", false, t0))
	}
	life2.flushAll()

	seen := map[string]bool{}
	for _, sp := range sink1.all() {
		seen[sp.SpanID] = true
	}
	for _, sp := range sink2.all() {
		if seen[sp.SpanID] {
			t.Fatalf("span %s reused across a restart", sp.SpanID)
		}
		if sp.TraceID != sink1.all()[0].TraceID {
			t.Fatal("trace ID changed across a restart")
		}
		if sp.ParentSpanID != "" {
			t.Fatalf("post-restart span parented to %s; its turn began before the restart", sp.ParentSpanID)
		}
	}
	// An item both daemons delivered (the slack window) keeps one identity.
	a, _ := life1.conv.itemIDs(toolEv("w", "k", 2, "t", false, t0))
	b, _ := life2.conv.itemIDs(toolEv("w", "k", 2, "t", false, t0))
	if a != b {
		t.Fatal("trace IDs of the same item differ between lifetimes")
	}
	_, ida := life1.conv.itemIDs(toolEv("w", "k", 2, "t", false, t0))
	_, idb := life2.conv.itemIDs(toolEv("w", "k", 2, "t", false, t0))
	if ida != idb {
		t.Fatal("the same item got different IDs in two lifetimes")
	}
}

func TestHarnessExitFlushesPromptly(t *testing.T) {
	now := t0
	up := true
	acc, sink := newAcc(&now, func(string) bool { return up })
	acc.add(toolEv("w", "k", 0, "t", false, t0))
	acc.tick()
	if len(sink.batches) != 0 {
		t.Fatal("flushed a live, recently active session")
	}
	up = false
	acc.tick()
	if len(sink.batches) != 1 {
		t.Fatal("harness exit did not flush")
	}
}

func TestUnsentCapExportsEarly(t *testing.T) {
	now := t0
	acc, sink := newAcc(&now, nil)
	for i := range traceMaxUnsent {
		acc.add(toolEv("w", "k", i, "t", false, t0))
	}
	if len(sink.batches) != 1 || len(sink.batches[0]) != traceMaxUnsent {
		t.Fatalf("batches %d, want one early export of %d", len(sink.batches), traceMaxUnsent)
	}
}

func TestSessionCapEvictsTheLeastRecentlyActive(t *testing.T) {
	now := t0
	acc, sink := newAcc(&now, nil)
	for i := range traceMaxSessions + 1 {
		now = t0.Add(time.Duration(i) * time.Second)
		acc.add(toolEv("w", "k"+strconv.Itoa(i), 0, "t", false, now))
	}
	if len(acc.sessions) != traceMaxSessions {
		t.Fatalf("tracking %d sessions", len(acc.sessions))
	}
	if _, ok := acc.sessions["k0"]; ok || len(sink.batches) != 1 || sink.batches[0][0].TraceID != TraceID("k0") {
		t.Fatal("the oldest session was not exported and forgotten")
	}
}

func TestForgetAfterADay(t *testing.T) {
	now := t0
	acc, _ := newAcc(&now, nil)
	acc.add(toolEv("w", "k", 0, "t", false, t0))
	now = now.Add(traceForget)
	acc.tick()
	if len(acc.sessions) != 0 {
		t.Fatal("session context kept past 24h")
	}
}

func TestErrorOnlySessionHasNoInventedSpans(t *testing.T) {
	now := t0
	acc, sink := newAcc(&now, nil)
	acc.add(markEv("w", "k", 0, "user-message", "go", t0))
	acc.add(markEv("w", "k", 0, "error", "quota", t0))
	acc.add(markEv("w", "k", 0, "error", "quota", t0.Add(time.Second)))
	acc.flushAll()
	spans := sink.all()
	if len(spans) != 1 || spans[0].Attributes["agent.turn.type"] != "user-message" {
		t.Fatalf("spans %+v, want the turn span alone", spans)
	}
}

func TestMissingTimestampIsFilledFromTheObserver(t *testing.T) {
	now := t0
	acc, sink := newAcc(&now, nil)
	at := t0.Add(42 * time.Second)
	ev := toolEv("w", "k", 0, "t", false, at)
	ev.Tool.Timestamp = ""
	acc.add(ev)
	acc.flushAll()
	if got := sink.all()[0].StartTime; !got.Equal(at) {
		t.Fatalf("span start %s, want the observer's %s", got, at)
	}
}

// Marks sharing a seq get distinct ordinals, and the same item gets the same
// ID however many sinks ask, in any order.
func TestOrdinalsAreSharedAndStable(t *testing.T) {
	reg := newIDRegistry(time.Now)
	conv := &converter{ids: reg}
	a := markEv("w", "k", 4, "user-message", "one", t0)
	b := markEv("w", "k", 4, "error", "two", t0)
	_, ida := conv.itemIDs(a)
	_, idb := conv.itemIDs(b)
	_, ida2 := conv.itemIDs(a)
	if ida == idb || ida != ida2 {
		t.Fatalf("ids %s %s %s", ida, idb, ida2)
	}
	if want := ItemID(TraceID("k"), observe.KindMark, 4, 1); idb != want {
		t.Fatalf("second mark at a seq id %s, want ordinal 1's %s", idb, want)
	}
}

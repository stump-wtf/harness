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
	"strings"
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
		conv: &converter{}, idle: 5 * time.Minute, now: clock,
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
	conv := &converter{}
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

// Items sharing a seq get distinct IDs, the same item gets the same ID from
// any converter (so every sink and every lifetime agree), and under
// omit_prompts the prompt text does not feed the exported ID.
func TestItemIDsAreContentKeyed(t *testing.T) {
	a := markEv("w", "k", 4, "user-message", "one", t0)
	b := markEv("w", "k", 4, "error", "two", t0)
	_, ida := (&converter{}).itemIDs(a)
	_, idb := (&converter{}).itemIDs(b)
	_, ida2 := (&converter{}).itemIDs(a)
	if ida == idb || ida != ida2 {
		t.Fatalf("ids %s %s %s", ida, idb, ida2)
	}
	if want := ItemID(TraceID("k"), observe.KindMark, 4, ContentKey(b, false)); idb != want {
		t.Fatalf("id %s, want the REQ-7 derivation %s", idb, want)
	}
	// omit_prompts: two prompts differing only in text share a key, so the
	// ID reveals nothing about the text; a timestamp still separates them.
	p1 := markEv("w", "k", 4, "user-message", "customer 1234", t0)
	p2 := markEv("w", "k", 4, "user-message", "customer 5678", t0)
	if ContentKey(p1, true) != ContentKey(p2, true) {
		t.Fatal("omit_prompts: the prompt text still feeds the item ID")
	}
	if ContentKey(p1, false) == ContentKey(p2, false) {
		t.Fatal("without omit_prompts the note must separate same-seq prompts")
	}
}

// A provider outage is exactly when many marks share one seq: every failed
// turn's user-message and error mark carry the seq of the tool call that never
// comes. A daemon restarted mid-outage does not replay, so it first sees the
// NEXT retry's marks. Their IDs must differ from every ID the previous daemon
// gave the earlier retries — otherwise a consumer deduplicating on
// agent.item.id (REQ-5) silently drops real error records, and the trace gets
// two different user-message spans with one span ID. The same item delivered
// by both lifetimes (the slack window) must still keep one ID.
func TestRestartDuringAnOutageKeepsItemIDsDistinct(t *testing.T) {
	now := t0
	life1, sink1 := newAcc(&now, nil)
	life1Items := []observe.Event{
		markEv("w", "k", 7, "user-message", "retry one", t0),
		markEv("w", "k", 7, "error", "429 quota exhausted", t0.Add(time.Second)),
		markEv("w", "k", 7, "user-message", "retry two", t0.Add(2*time.Second)),
		markEv("w", "k", 7, "error", "429 quota exhausted", t0.Add(3*time.Second)),
	}
	ids1 := map[string]string{}
	for _, ev := range life1Items {
		_, id := life1.conv.itemIDs(ev)
		ids1[id] = ev.Mark.Note + "@" + ev.Mark.Timestamp
		life1.add(ev)
	}
	life1.flushAll()
	if len(ids1) != len(life1Items) {
		t.Fatalf("one lifetime gave %d distinct IDs to %d distinct items", len(ids1), len(life1Items))
	}

	life2, sink2 := newAcc(&now, nil)
	life2Items := []observe.Event{
		markEv("w", "k", 7, "user-message", "retry three", t0.Add(4*time.Second)),
		markEv("w", "k", 7, "error", "429 quota exhausted", t0.Add(5*time.Second)),
	}
	for _, ev := range life2Items {
		_, id := life2.conv.itemIDs(ev)
		if was, dup := ids1[id]; dup {
			t.Fatalf("%q in the second lifetime got the ID the first gave %q", ev.Mark.Note, was)
		}
		life2.add(ev)
	}
	life2.flushAll()
	sent := map[string]bool{}
	for _, sp := range sink1.all() {
		sent[sp.SpanID] = true
	}
	for _, sp := range sink2.all() {
		if sent[sp.SpanID] {
			t.Fatalf("span %q reuses span ID %s from the previous daemon", sp.Name, sp.SpanID)
		}
	}

	// The slack window: an item both daemons delivered keeps its identity.
	_, a := life1.conv.itemIDs(life1Items[3])
	_, b := life2.conv.itemIDs(life1Items[3])
	if a != b {
		t.Fatalf("the same item got %s and %s in two lifetimes", a, b)
	}
}

// BuildTrace truncates a user-message span name to 128 runes. Redacting only
// its output, after the cut, would miss a credential the cut shortened below
// the pattern's minimum length and export the head of it. Every source string
// must be redacted before the build (REQ-4).
func TestSpanSourcesAreRedactedBeforeBuildTraceTruncates(t *testing.T) {
	now := t0
	acc, sink := newAcc(&now, nil)
	// 110 runes of padding leave "ghp_" plus 13 characters inside the cut.
	note := strings.Repeat("a ", 55) + plantedToken
	ev := markEv("w", "k", 0, "user-message", note, t0)
	acc.add(ev)
	tool := toolEv("w", "k", 0, "curl -H 'Authorization: token "+plantedToken+"'", true, t0)
	acc.add(tool)
	acc.flushAll()
	head := plantedToken[:12]
	for _, sp := range sink.all() {
		if strings.Contains(sp.Name, head) || strings.Contains(sp.StatusMsg, head) {
			t.Fatalf("span %q (status %q) carries the head of a credential", sp.Name, sp.StatusMsg)
		}
		for k, v := range sp.Attributes {
			if s, ok := v.(string); ok && strings.Contains(s, head) {
				t.Fatalf("span attribute %s = %q carries the head of a credential", k, s)
			}
		}
	}
	if len(sink.all()) != 2 {
		t.Fatalf("spans %d, want the turn and the tool", len(sink.all()))
	}
}

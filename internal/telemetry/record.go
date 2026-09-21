package telemetry

// Records and Identity
//
// Every observed item is described once, as a signal-neutral Record: the
// SPEC-0014 REQ-5 field set, redacted and capped, plus the item's trace ID,
// item ID and — when the item maps to a span — its span ID. The logs sink turns
// a Record into an OTLP LogRecord and the events file sink marshals the same
// Record to a line, which is what keeps the JSONL keys and the log attributes
// from drifting apart. Redaction and omit_prompts happen here, at conversion,
// so no unredacted string ever sits in a queue, a retry buffer or a file.
//
// Identity. The trace ID is agent-trace's own, sha256("trace:"+key)[:16], so
// logs can compute it without building a trace. The item ID is keyed to the
// item rather than to agent-trace's per-build span counter:
//
//	sha256("harness-item:" + traceID + ":" + kind + ":" + seq + ":" + ordinal)[:8]
//
// because the observer does not replay history, and a counter restarted by a
// daemon restart would hand the first new item the ID of a span the previous
// daemon already sent (REQ-7). The ordinal separates items of one kind that
// share a seq (marks carry the seq of the tool call that follows them). It is
// assigned by one registry shared by all three sinks and keyed by the item's
// content, so a sink that lost an item to a full buffer still agrees with the
// others about every item it did get.
//
// Governing: ADR-0021; SPEC-0014 REQ-4, REQ-5, REQ-6, REQ-7, REQ-8; ADR-0008.
//
// @joestump-agent 09/21/2026 - Added for harness#391.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/stump-wtf/harness/internal/observe"
	"github.com/stump-wtf/harness/internal/otlpexport"
	"github.com/stump-wtf/harness/internal/redact"
)

// Wire constants (SPEC-0014 REQ-4, REQ-6, REQ-8).
const (
	// ScopeName is the instrumentation scope of every log record and span.
	ScopeName = "github.com/stump-wtf/harness/telemetry"
	// Schema is the events file's schema value; renaming or removing a field
	// changes it.
	Schema = "harness.telemetry/v1"
	// PromptOmitted replaces prompt text under omit_prompts.
	PromptOmitted = "[prompt omitted]"

	bodyCap     = 4 << 10
	spanNameCap = 256
	attrCap     = 1 << 10
	targetsMax  = 32
	targetCap   = 512
)

// Severity is a log record's severity.
type Severity struct {
	Text   string
	Number int
}

// The three severities REQ-6 uses. An error mark is a failed turn; a tool call
// that returned an error is the agent working, so it is WARN and an alert on
// ERROR means a turn failed.
var (
	SeverityInfo  = Severity{"INFO", 9}
	SeverityWarn  = Severity{"WARN", 13}
	SeverityError = Severity{"ERROR", 17}
)

// TraceID is a session's trace ID: agent-trace's derivation, reproduced so a
// log record can carry it without building a trace. A test pins it equal to
// otel.BuildTrace's.
func TraceID(sessionKey string) string {
	sum := sha256.Sum256([]byte("trace:" + sessionKey))
	return hex.EncodeToString(sum[:16])
}

// ItemID is an item's ID, which is also its span ID when it maps to a span.
func ItemID(traceID string, kind observe.Kind, seq, ordinal int) string {
	sum := sha256.Sum256([]byte("harness-item:" + traceID + ":" + string(kind) + ":" + strconv.Itoa(seq) + ":" + strconv.Itoa(ordinal)))
	return hex.EncodeToString(sum[:8])
}

// mapsToSpan reports whether otel.BuildTrace produces a span for ev at the
// pinned agent-trace version: every tool call, and user-message, compaction
// and subagent marks. TestMapsToSpanMatchesBuildTrace fails if that changes.
func mapsToSpan(ev observe.Event) bool {
	if ev.Kind == observe.KindTool {
		return true
	}
	switch ev.Mark.Type {
	case "user-message", "compaction", "subagent":
		return true
	}
	return false
}

func itemSeq(ev observe.Event) int {
	if ev.Kind == observe.KindTool {
		return ev.Tool.Seq
	}
	return ev.Mark.Seq
}

// Record is one item, ready for any sink. Every string in it is redacted and
// capped.
type Record struct {
	Time       time.Time
	ObservedAt time.Time
	Severity   Severity
	Body       string
	TraceID    string
	// SpanID is set only when the item maps to a span.
	SpanID string
	ItemID string
	// Harness is the attributed harness, kept for the gate's bookkeeping.
	Harness string
	// Attrs are the REQ-5 item attributes, in a fixed order.
	Attrs []otlpexport.KeyValue
}

// LogRecord is r as an OTLP LogRecord (REQ-6).
func (r Record) LogRecord() otlpexport.LogRecord {
	return otlpexport.LogRecord{
		Time: r.Time, ObservedTime: r.ObservedAt,
		SeverityNumber: r.Severity.Number, SeverityText: r.Severity.Text,
		Body: r.Body, Attributes: r.Attrs,
		TraceID: r.TraceID, SpanID: r.SpanID,
	}
}

// JSONLine is r as one events file line (REQ-8): flat dotted keys, the
// resource fields, and a trailing newline. encoding/json escapes every
// control character, so the line holds no raw newline of its own.
func (r Record) JSONLine(resource []otlpexport.KeyValue) ([]byte, error) {
	m := make(map[string]any, len(r.Attrs)+len(resource)+8)
	for _, kv := range resource {
		m[kv.Key] = kv.Value
	}
	for _, kv := range r.Attrs {
		m[kv.Key] = kv.Value
	}
	m["schema"] = Schema
	m["time"] = r.Time.UTC().Format(time.RFC3339Nano)
	m["observed_time"] = r.ObservedAt.UTC().Format(time.RFC3339Nano)
	m["severity"] = r.Severity.Text
	m["body"] = r.Body
	m["trace_id"] = r.TraceID
	if r.SpanID != "" {
		m["span_id"] = r.SpanID
	}
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// converter turns observer events into Records.
type converter struct {
	omitPrompts bool
	ids         *idRegistry
}

// ids returns ev's trace and item IDs.
func (c *converter) itemIDs(ev observe.Event) (traceID, itemID string) {
	traceID = TraceID(ev.Session.Key)
	return traceID, ItemID(traceID, ev.Kind, itemSeq(ev), c.ids.ordinal(ev))
}

// isPrompt reports whether ev's note is prompt text.
func isPrompt(ev observe.Event) bool {
	return ev.Kind == observe.KindMark && ev.Mark.Type == "user-message"
}

// record builds ev's Record. Every string passes through redact.String here,
// even though the observer already redacted it: redaction is idempotent, and
// this is the one place REQ-4 can be checked for every sink at once.
func (c *converter) record(ev observe.Event) Record {
	traceID, itemID := c.itemIDs(ev)
	r := Record{
		Time: ev.Time, ObservedAt: ev.ObservedAt,
		TraceID: traceID, ItemID: itemID, Harness: ev.Harness,
		Severity: SeverityInfo,
	}
	if mapsToSpan(ev) {
		r.SpanID = itemID
	}

	attrs := []otlpexport.KeyValue{
		{Key: "harness.name", Value: clean(ev.Harness, attrCap)},
		{Key: "agent.adapter", Value: clean(ev.Adapter, attrCap)},
		{Key: "agent.session.id", Value: clean(ev.Session.ID, attrCap)},
	}
	if ev.Session.Title != "" && !c.omitPrompts {
		attrs = append(attrs, otlpexport.KeyValue{Key: "agent.session.title", Value: clean(ev.Session.Title, attrCap)})
	}
	if ev.Session.Cwd != "" {
		attrs = append(attrs, otlpexport.KeyValue{Key: "agent.session.cwd", Value: clean(ev.Session.Cwd, attrCap)})
	}
	if ev.Session.Model != "" {
		attrs = append(attrs, otlpexport.KeyValue{Key: "agent.model", Value: clean(ev.Session.Model, attrCap)})
	}
	attrs = append(attrs,
		otlpexport.KeyValue{Key: "agent.item.kind", Value: string(ev.Kind)},
		otlpexport.KeyValue{Key: "agent.item.seq", Value: int64(itemSeq(ev))},
		otlpexport.KeyValue{Key: "agent.item.id", Value: itemID},
	)

	switch ev.Kind {
	case observe.KindTool:
		t := ev.Tool
		r.Body = clean(t.Summary, bodyCap)
		if t.IsError {
			r.Severity = SeverityWarn
		}
		attrs = append(attrs,
			otlpexport.KeyValue{Key: "agent.tool.name", Value: clean(t.Tool, attrCap)},
			otlpexport.KeyValue{Key: "agent.tool.action", Value: clean(t.Action, attrCap)},
			otlpexport.KeyValue{Key: "agent.tool.is_error", Value: t.IsError},
			otlpexport.KeyValue{Key: "agent.result.bytes", Value: int64(t.ResultBytes)},
		)
		if len(t.Targets) > 0 {
			n := min(len(t.Targets), targetsMax)
			paths := make([]string, 0, n)
			for _, tg := range t.Targets[:n] {
				paths = append(paths, clean(tg.Path, targetCap))
			}
			attrs = append(attrs, otlpexport.KeyValue{Key: "agent.targets", Value: paths})
		}
		if len(t.Outside) > 0 {
			attrs = append(attrs, otlpexport.KeyValue{Key: "agent.outside_count", Value: int64(len(t.Outside))})
		}
	case observe.KindMark:
		m := ev.Mark
		switch {
		case isPrompt(ev) && c.omitPrompts:
			r.Body = PromptOmitted
		case m.Note != "":
			r.Body = clean(m.Note, bodyCap)
		default:
			r.Body = clean(m.Type, bodyCap)
		}
		if m.Type == "error" {
			r.Severity = SeverityError
		}
		attrs = append(attrs, otlpexport.KeyValue{Key: "agent.mark.type", Value: clean(m.Type, attrCap)})
	}
	r.Attrs = attrs
	return r
}

// clean redacts s and caps it at max bytes.
func clean(s string, max int) string {
	return capString(redact.String(s), max)
}

// capString truncates s to at most max bytes on a UTF-8 boundary, ending in
// "…" when anything was cut.
func capString(s string, max int) string {
	if len(s) <= max {
		return s
	}
	const ell = "…"
	cut := max - len(ell)
	if cut < 0 {
		cut = 0
	}
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + ell
}

// idRegistry assigns ordinals: the index of an item among items of the same
// kind and seq in its session, in first-seen order, keyed by the item's
// content so every sink computes the same answer for the same item.
type idRegistry struct {
	mu       sync.Mutex
	now      func() time.Time
	sessions map[string]*idSession
	calls    int
}

type idSession struct {
	last   time.Time
	maxSeq int
	slots  map[idSlot][]string
}

type idSlot struct {
	kind observe.Kind
	seq  int
}

// Registry bounds: sessions idle this long are forgotten, and at most this
// many are kept; within a session, slots far behind the newest seq go.
const (
	idForgetAfter  = 24 * time.Hour
	idMaxSessions  = 4096
	idSlotsKeep    = 1024
	idSweepEveryN  = 1024
	idSlotsCeiling = 2 * idSlotsKeep
)

func newIDRegistry(now func() time.Time) *idRegistry {
	return &idRegistry{now: now, sessions: map[string]*idSession{}}
}

func (r *idRegistry) ordinal(ev observe.Event) int {
	seq := itemSeq(ev)
	fp := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%s\x00%s", ev.Mark.Type, ev.Mark.Note, ev.Mark.Timestamp, ev.Tool.Tool, ev.Tool.Summary, ev.Tool.Timestamp)
	now := r.now()

	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	if r.calls%idSweepEveryN == 0 {
		r.sweepLocked(now)
	}
	s := r.sessions[ev.Session.Key]
	if s == nil {
		if len(r.sessions) >= idMaxSessions {
			r.evictOldestLocked()
		}
		s = &idSession{slots: map[idSlot][]string{}}
		r.sessions[ev.Session.Key] = s
	}
	s.last = now
	if seq > s.maxSeq {
		s.maxSeq = seq
	}
	key := idSlot{ev.Kind, seq}
	list := s.slots[key]
	for i, have := range list {
		if have == fp {
			return i
		}
	}
	s.slots[key] = append(list, fp)
	if len(s.slots) > idSlotsCeiling {
		for k := range s.slots {
			if k.seq < s.maxSeq-idSlotsKeep {
				delete(s.slots, k)
			}
		}
	}
	return len(list)
}

func (r *idRegistry) sweepLocked(now time.Time) {
	for k, s := range r.sessions {
		if now.Sub(s.last) > idForgetAfter {
			delete(r.sessions, k)
		}
	}
}

func (r *idRegistry) evictOldestLocked() {
	var oldest string
	var at time.Time
	for k, s := range r.sessions {
		if oldest == "" || s.last.Before(at) {
			oldest, at = k, s.last
		}
	}
	delete(r.sessions, oldest)
}

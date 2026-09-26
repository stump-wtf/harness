package telemetry

// Records and Identity
//
// Every observed item is described once, as a signal-neutral Record: the
// SPEC-0015 REQ-5 field set, redacted and capped, plus the item's trace ID,
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
//	sha256("harness-item:" + traceID + ":" + kind + ":" + seq + ":" + content)[:8]
//
// because the observer does not replay history, and a counter restarted by a
// daemon restart would hand the first new item the ID of a span the previous
// daemon already sent (REQ-7). The content key separates items of one kind
// that share a seq — marks carry the seq of the tool call that follows them,
// so a provider outage piles every retry's user-message and error mark onto
// one seq. It is a pure function of what the transcript recorded, so every
// sink, and every daemon lifetime, computes the same ID for the same item
// without sharing any state.
//
// Governing: ADR-0022; SPEC-0015 REQ-4, REQ-5, REQ-6, REQ-7, REQ-8; ADR-0008.
//
// @joestump-agent 09/21/2026 - Added for harness#391.
//
// @joestump-agent 09/21/2026 - review: item IDs keyed by content, not by a
// first-seen ordinal. The ordinal restarted at 0 with the daemon, so a restart
// mid-outage handed the next retry's marks the IDs of earlier ones.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/stump-wtf/harness/internal/observe"
	"github.com/stump-wtf/harness/internal/otlpexport"
	"github.com/stump-wtf/harness/internal/redact"
)

// Wire constants (SPEC-0015 REQ-4, REQ-6, REQ-8).
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
// content is the item's ContentKey.
func ItemID(traceID string, kind observe.Kind, seq int, content string) string {
	sum := sha256.Sum256([]byte("harness-item:" + traceID + ":" + string(kind) + ":" + strconv.Itoa(seq) + ":" + content))
	return hex.EncodeToString(sum[:8])
}

// ContentKey is what tells apart items of one kind sharing a seq (SPEC-0015
// REQ-7): the fields the transcript recorded for the item, joined with NUL.
// A tool event is keyed by its tool, summary and timestamp; a mark by its
// type, timestamp and note. Under omit_prompts a user-message's note is left
// out: the ID is exported, and a hash over a short prompt would let anyone
// holding the telemetry confirm a guess of the text the operator chose to
// hide. Two items identical in every keyed field get one ID; that is the
// price of an ID no restart can reassign.
func ContentKey(ev observe.Event, omitPrompts bool) string {
	if ev.Kind == observe.KindTool {
		return "tool\x00" + ev.Tool.Tool + "\x00" + ev.Tool.Summary + "\x00" + ev.Tool.Timestamp
	}
	note := ev.Mark.Note
	if omitPrompts && isPrompt(ev) {
		note = ""
	}
	return "mark\x00" + ev.Mark.Type + "\x00" + ev.Mark.Timestamp + "\x00" + note
}

// mapsToSpan reports whether otel.BuildTrace produces a span for ev at the
// pinned agent-trace version: every tool call, and user-message, compaction
// and subagent marks. A turn-end mark (v0.6.0) is not one: BuildTrace uses it
// only to end its turn's last tool span. TestMapsToSpanMatchesBuildTrace fails
// if that changes.
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

// converter turns observer events into Records. It holds no state, so every
// sink computes the same IDs for the same item.
type converter struct {
	omitPrompts bool
}

// itemIDs returns ev's trace and item IDs.
func (c *converter) itemIDs(ev observe.Event) (traceID, itemID string) {
	traceID = TraceID(ev.Session.Key)
	return traceID, ItemID(traceID, ev.Kind, itemSeq(ev), ContentKey(ev, c.omitPrompts))
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

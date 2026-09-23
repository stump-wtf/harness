package telemetry

// Strict OTLP/HTTP JSON Reader
//
// The receiver's own decode is deliberately forgiving (maps of any), which is
// right for reading attributes back and wrong for proving the encoding: a
// lenient reader accepts a number where the proto JSON mapping requires a
// decimal string, an empty AnyValue, or an uppercase trace ID, and a real
// collector does not. strictOTLP decodes a request body the way the mapping
// defines it — unknown fields rejected, 64-bit integers as decimal strings,
// IDs as lowercase hex of the right length and not all zero, enums as
// integers in range, every AnyValue carrying exactly one field, attribute
// keys unique — and the receiver runs it on every request any test makes.
// TestStrictOTLPRejectsWhatACollectorWould shows each check can fire.
//
// Governing tests: SPEC-0015 REQ-6, REQ-7; the OTLP specification's JSON
// Protobuf Encoding.
//
// @joestump-agent 09/21/2026 - review: added so the pipeline's wire format is
// checked against the mapping, not against substrings.
//
// @joestump-agent 09/23/2026 - Span events, which agent-trace v0.4.0 emits
// for an error mark inside a turn.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var (
	hex32 = regexp.MustCompile(`^[0-9a-f]{32}$`)
	hex16 = regexp.MustCompile(`^[0-9a-f]{16}$`)
)

type strictKV struct {
	Key   string          `json:"key"`
	Value json.RawMessage `json:"value"`
}

type strictResource struct {
	Attributes []strictKV `json:"attributes"`
}

type strictScope struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type strictLog struct {
	TimeUnixNano         json.RawMessage `json:"timeUnixNano"`
	ObservedTimeUnixNano json.RawMessage `json:"observedTimeUnixNano"`
	SeverityNumber       json.RawMessage `json:"severityNumber"`
	SeverityText         string          `json:"severityText"`
	Body                 json.RawMessage `json:"body"`
	Attributes           []strictKV      `json:"attributes"`
	TraceID              *string         `json:"traceId"`
	SpanID               *string         `json:"spanId"`
}

type strictSpan struct {
	TraceID           string          `json:"traceId"`
	SpanID            string          `json:"spanId"`
	ParentSpanID      *string         `json:"parentSpanId"`
	Name              string          `json:"name"`
	Kind              json.RawMessage `json:"kind"`
	StartTimeUnixNano json.RawMessage `json:"startTimeUnixNano"`
	EndTimeUnixNano   json.RawMessage `json:"endTimeUnixNano"`
	Attributes        []strictKV      `json:"attributes"`
	Events            []strictEvent   `json:"events"`
	Status            struct {
		Code    json.RawMessage `json:"code"`
		Message string          `json:"message"`
	} `json:"status"`
}

// strictEvent is a Span.Event. agent-trace v0.4.0 records an error mark
// inside a turn as an "exception" event on the turn's span.
type strictEvent struct {
	TimeUnixNano json.RawMessage `json:"timeUnixNano"`
	Name         string          `json:"name"`
	Attributes   []strictKV      `json:"attributes"`
}

type strictBody struct {
	ResourceLogs []struct {
		Resource  strictResource `json:"resource"`
		ScopeLogs []struct {
			Scope      strictScope `json:"scope"`
			LogRecords []strictLog `json:"logRecords"`
		} `json:"scopeLogs"`
	} `json:"resourceLogs"`
	ResourceSpans []struct {
		Resource   strictResource `json:"resource"`
		ScopeSpans []struct {
			Scope strictScope  `json:"scope"`
			Spans []strictSpan `json:"spans"`
		} `json:"scopeSpans"`
	} `json:"resourceSpans"`
}

// strictOTLP checks raw, POSTed to path, against the OTLP JSON mapping.
func strictOTLP(path string, raw []byte) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var b strictBody
	if err := dec.Decode(&b); err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	switch {
	case strings.HasSuffix(path, "/v1/logs"):
		if len(b.ResourceLogs) == 0 || b.ResourceSpans != nil {
			return fmt.Errorf("%s must carry resourceLogs only", path)
		}
	case strings.HasSuffix(path, "/v1/traces"):
		if len(b.ResourceSpans) == 0 || b.ResourceLogs != nil {
			return fmt.Errorf("%s must carry resourceSpans only", path)
		}
	default:
		return fmt.Errorf("unexpected path %s", path)
	}
	for _, rl := range b.ResourceLogs {
		if err := strictAttrs("resource", rl.Resource.Attributes); err != nil {
			return err
		}
		for _, sl := range rl.ScopeLogs {
			if sl.Scope.Name == "" {
				return fmt.Errorf("scope has no name")
			}
			for i, l := range sl.LogRecords {
				if err := strictLogRecord(l); err != nil {
					return fmt.Errorf("logRecords[%d]: %w", i, err)
				}
			}
		}
	}
	for _, rs := range b.ResourceSpans {
		if err := strictAttrs("resource", rs.Resource.Attributes); err != nil {
			return err
		}
		for _, ss := range rs.ScopeSpans {
			if ss.Scope.Name == "" {
				return fmt.Errorf("scope has no name")
			}
			for i, sp := range ss.Spans {
				if err := strictSpanRecord(sp); err != nil {
					return fmt.Errorf("spans[%d] %q: %w", i, sp.Name, err)
				}
			}
		}
	}
	return nil
}

func strictLogRecord(l strictLog) error {
	if _, err := strictUint64(l.TimeUnixNano); err != nil {
		return fmt.Errorf("timeUnixNano: %w", err)
	}
	if _, err := strictUint64(l.ObservedTimeUnixNano); err != nil {
		return fmt.Errorf("observedTimeUnixNano: %w", err)
	}
	sev, err := strictEnum(l.SeverityNumber, 1, 24)
	if err != nil {
		return fmt.Errorf("severityNumber: %w", err)
	}
	if want := map[int]string{9: "INFO", 13: "WARN", 17: "ERROR"}[sev]; want == "" || l.SeverityText != want {
		return fmt.Errorf("severity %d/%q is not one of REQ-6's pairs", sev, l.SeverityText)
	}
	if err := strictAnyValue(l.Body); err != nil {
		return fmt.Errorf("body: %w", err)
	}
	if err := strictAttrs("attributes", l.Attributes); err != nil {
		return err
	}
	if l.TraceID != nil && !strictID(*l.TraceID, hex32) {
		return fmt.Errorf("traceId %q is not 32 lowercase hex, non-zero", *l.TraceID)
	}
	if l.SpanID != nil {
		if !strictID(*l.SpanID, hex16) {
			return fmt.Errorf("spanId %q is not 16 lowercase hex, non-zero", *l.SpanID)
		}
		if l.TraceID == nil {
			return fmt.Errorf("spanId without traceId")
		}
	}
	return nil
}

func strictSpanRecord(sp strictSpan) error {
	if !strictID(sp.TraceID, hex32) {
		return fmt.Errorf("traceId %q is not 32 lowercase hex, non-zero", sp.TraceID)
	}
	if !strictID(sp.SpanID, hex16) {
		return fmt.Errorf("spanId %q is not 16 lowercase hex, non-zero", sp.SpanID)
	}
	if sp.ParentSpanID != nil && !strictID(*sp.ParentSpanID, hex16) {
		return fmt.Errorf("parentSpanId %q is not 16 lowercase hex, non-zero", *sp.ParentSpanID)
	}
	if sp.ParentSpanID != nil && *sp.ParentSpanID == sp.SpanID {
		return fmt.Errorf("span is its own parent")
	}
	if _, err := strictEnum(sp.Kind, 0, 5); err != nil {
		return fmt.Errorf("kind: %w", err)
	}
	start, err := strictUint64(sp.StartTimeUnixNano)
	if err != nil {
		return fmt.Errorf("startTimeUnixNano: %w", err)
	}
	end, err := strictUint64(sp.EndTimeUnixNano)
	if err != nil {
		return fmt.Errorf("endTimeUnixNano: %w", err)
	}
	if end < start {
		return fmt.Errorf("ends (%d) before it starts (%d)", end, start)
	}
	if _, err := strictEnum(sp.Status.Code, 0, 2); err != nil {
		return fmt.Errorf("status.code: %w", err)
	}
	for i, ev := range sp.Events {
		if _, err := strictUint64(ev.TimeUnixNano); err != nil {
			return fmt.Errorf("events[%d].timeUnixNano: %w", i, err)
		}
		if ev.Name == "" {
			return fmt.Errorf("events[%d] has no name", i)
		}
		if err := strictAttrs(fmt.Sprintf("events[%d].attributes", i), ev.Attributes); err != nil {
			return err
		}
	}
	return strictAttrs("attributes", sp.Attributes)
}

func strictID(s string, re *regexp.Regexp) bool {
	return re.MatchString(s) && strings.Trim(s, "0") != ""
}

// strictUint64 requires a fixed64 the canonical way: a decimal string.
func strictUint64(raw json.RawMessage) (uint64, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, fmt.Errorf("%s is not a decimal string", raw)
	}
	return strconv.ParseUint(s, 10, 64)
}

// strictEnum requires a JSON integer in [lo, hi] (enums are sent as numbers).
func strictEnum(raw json.RawMessage, lo, hi int) (int, error) {
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, fmt.Errorf("%s is not an integer", raw)
	}
	if n < lo || n > hi {
		return 0, fmt.Errorf("%d out of range [%d, %d]", n, lo, hi)
	}
	return n, nil
}

func strictAttrs(where string, kvs []strictKV) error {
	seen := map[string]bool{}
	for _, kv := range kvs {
		if kv.Key == "" {
			return fmt.Errorf("%s: empty key", where)
		}
		if seen[kv.Key] {
			return fmt.Errorf("%s: duplicate key %q", where, kv.Key)
		}
		seen[kv.Key] = true
		if err := strictAnyValue(kv.Value); err != nil {
			return fmt.Errorf("%s %q: %w", where, kv.Key, err)
		}
	}
	return nil
}

// strictAnyValue requires exactly one AnyValue field, of the right JSON type.
func strictAnyValue(raw json.RawMessage) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("AnyValue %s is not an object", raw)
	}
	if len(m) != 1 {
		return fmt.Errorf("AnyValue %s must carry exactly one field", raw)
	}
	for k, v := range m {
		switch k {
		case "stringValue":
			var s string
			if json.Unmarshal(v, &s) != nil {
				return fmt.Errorf("stringValue %s is not a string", v)
			}
		case "boolValue":
			var b bool
			if json.Unmarshal(v, &b) != nil {
				return fmt.Errorf("boolValue %s is not a bool", v)
			}
		case "intValue":
			var s string
			if json.Unmarshal(v, &s) != nil {
				return fmt.Errorf("intValue %s is not a decimal string", v)
			}
			if _, err := strconv.ParseInt(s, 10, 64); err != nil {
				return fmt.Errorf("intValue %q: %w", s, err)
			}
		case "doubleValue":
			var f float64
			if json.Unmarshal(v, &f) != nil {
				return fmt.Errorf("doubleValue %s is not a number", v)
			}
		case "arrayValue":
			dec := json.NewDecoder(bytes.NewReader(v))
			dec.DisallowUnknownFields()
			var arr struct {
				Values []json.RawMessage `json:"values"`
			}
			if err := dec.Decode(&arr); err != nil {
				return fmt.Errorf("arrayValue: %w", err)
			}
			for _, item := range arr.Values {
				if err := strictAnyValue(item); err != nil {
					return fmt.Errorf("arrayValue: %w", err)
				}
			}
		default:
			return fmt.Errorf("AnyValue field %q is not one Harness sends", k)
		}
	}
	return nil
}

// Each strict check fires on the mistake it exists for, so its silence on the
// pipeline's real requests means something.
func TestStrictOTLPRejectsWhatACollectorWould(t *testing.T) {
	const tid, sid = `"0123456789abcdef0123456789abcdef"`, `"0123456789abcdef"`
	log := func(rec string) string {
		return `{"resourceLogs":[{"resource":{"attributes":[]},"scopeLogs":[{"scope":{"name":"s","version":"v"},"logRecords":[` + rec + `]}]}]}`
	}
	span := func(sp string) string {
		return `{"resourceSpans":[{"resource":{"attributes":[]},"scopeSpans":[{"scope":{"name":"s","version":"v"},"spans":[` + sp + `]}]}]}`
	}
	goodLog := `{"timeUnixNano":"1","observedTimeUnixNano":"2","severityNumber":9,"severityText":"INFO","body":{"stringValue":"b"},"traceId":` + tid + `,"spanId":` + sid + `}`
	goodSpan := `{"traceId":` + tid + `,"spanId":` + sid + `,"name":"n","kind":1,"startTimeUnixNano":"1","endTimeUnixNano":"2","status":{"code":0}}`
	if err := strictOTLP("/v1/logs", []byte(log(goodLog))); err != nil {
		t.Fatalf("a valid log request was rejected: %v", err)
	}
	if err := strictOTLP("/v1/traces", []byte(span(goodSpan))); err != nil {
		t.Fatalf("a valid span request was rejected: %v", err)
	}
	goodEvent := `"events":[{"timeUnixNano":"2","name":"exception","attributes":[{"key":"exception.message","value":{"stringValue":"m"}}]}],`
	withEvent := strings.Replace(goodSpan, `"status"`, goodEvent+`"status"`, 1)
	if err := strictOTLP("/v1/traces", []byte(span(withEvent))); err != nil {
		t.Fatalf("a valid span event was rejected: %v", err)
	}
	for name, body := range map[string]struct{ path, raw string }{
		"numeric timestamp":  {"/v1/logs", log(strings.Replace(goodLog, `"timeUnixNano":"1"`, `"timeUnixNano":1`, 1))},
		"severity as name":   {"/v1/logs", log(strings.Replace(goodLog, `"severityNumber":9`, `"severityNumber":"SEVERITY_NUMBER_INFO"`, 1))},
		"empty AnyValue":     {"/v1/logs", log(strings.Replace(goodLog, `{"stringValue":"b"}`, `{}`, 1))},
		"two-field AnyValue": {"/v1/logs", log(strings.Replace(goodLog, `{"stringValue":"b"}`, `{"stringValue":"b","boolValue":false}`, 1))},
		"numeric intValue":   {"/v1/logs", log(strings.Replace(goodLog, `"body":{"stringValue":"b"}`, `"body":{"stringValue":"b"},"attributes":[{"key":"n","value":{"intValue":7}}]`, 1))},
		"duplicate key":      {"/v1/logs", log(strings.Replace(goodLog, `"body":{"stringValue":"b"}`, `"body":{"stringValue":"b"},"attributes":[{"key":"k","value":{"stringValue":"a"}},{"key":"k","value":{"stringValue":"b"}}]`, 1))},
		"uppercase traceId":  {"/v1/logs", log(strings.Replace(goodLog, tid, strings.ToUpper(tid), 1))},
		"unknown field":      {"/v1/logs", log(strings.Replace(goodLog, `"severityText"`, `"severity_text":"x","severityText"`, 1))},
		"kind as name":       {"/v1/traces", span(strings.Replace(goodSpan, `"kind":1`, `"kind":"SPAN_KIND_INTERNAL"`, 1))},
		"zero spanId":        {"/v1/traces", span(strings.Replace(goodSpan, sid, `"0000000000000000"`, 1))},
		"short spanId":       {"/v1/traces", span(strings.Replace(goodSpan, sid, `"0123"`, 1))},
		"ends before start":  {"/v1/traces", span(strings.Replace(goodSpan, `"endTimeUnixNano":"2"`, `"endTimeUnixNano":"0"`, 1))},
		"logs to traces":     {"/v1/traces", log(goodLog)},
		"numeric event time": {"/v1/traces", span(strings.Replace(withEvent, `"timeUnixNano":"2"`, `"timeUnixNano":2`, 1))},
		"unnamed event":      {"/v1/traces", span(strings.Replace(withEvent, `"name":"exception"`, `"name":""`, 1))},
		"event bad AnyValue": {"/v1/traces", span(strings.Replace(withEvent, `{"stringValue":"m"}`, `{}`, 1))},
		"event unknown key":  {"/v1/traces", span(strings.Replace(withEvent, `"name":"exception"`, `"name":"exception","nme":"x"`, 1))},
	} {
		if err := strictOTLP(body.path, []byte(body.raw)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

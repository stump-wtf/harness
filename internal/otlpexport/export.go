// Package otlpexport speaks OTLP/HTTP JSON: it encodes agent-trace spans and
// Harness log records into the OTLP JSON wire format and POSTs them to any
// OTLP-compatible endpoint.
//
// # OTLP Export
//
// The exporter is endpoint-agnostic: Honeycomb, Tempo, Loki, Jaeger, Grafana
// Cloud, an OpenTelemetry Collector, or a Cairn instance that exposes an OTLP
// endpoint all accept the same request. Harness does not know or care what is
// on the other end.
//
// It is deliberately a pure "send this request, tell me what happened"
// function. Retry, backoff, batching and the queue belong to
// internal/telemetry, where the clock is injected; here SendLogs and SendSpans
// make exactly one request and classify the answer (Result): how many units the
// collector rejected through partialSuccess, whether a failure is worth
// retrying (429/502/503/504 and network errors, per the OTLP specification),
// and any Retry-After it asked for.
//
// The encoding follows the OTLP specification's JSON mapping of the protobuf:
// trace and span IDs as lowercase hex, 64-bit integers (timestamps, intValue)
// as decimal strings, enum fields (span kind, status code, severity number) as
// integers, and every attribute value as an AnyValue carrying exactly one of
// its fields — including a false bool or an empty string, which a plain
// omitempty encoding would turn into an empty, invalid AnyValue.
//
// Governing: ADR-0021; SPEC-0014 REQ-6, REQ-7, REQ-10; ADR-0008 (header values
// are never put into an error or a log line).
//
// @joestump-agent 09/21/2026 - Rewritten for harness#391: logs alongside
// traces, a full-URL endpoint, gzip, a per-request timeout, a typed Result,
// integer enums and single-field AnyValues. Export keeps its old contract.
//
// @joestump-agent 09/21/2026 - review: redirects are no longer followed, so a
// collector credential never reaches a URL the operator did not configure.
package otlpexport

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stump-wtf/agent-trace/otel"
)

// DefaultTimeout caps a single export HTTP request when the caller names none.
const DefaultTimeout = 30 * time.Second

// maxResponseBody bounds how much of a collector's reply is read.
const maxResponseBody = 1 << 20

// defaultClient is http.DefaultClient's transport without redirects. Go
// follows a 307/308 by re-sending the body with every custom header — an
// api-key header included — so a redirecting endpoint would get the batch
// and the collector credential delivered to a URL nobody configured. A 3xx
// is instead returned as it is, a permanent failure the operator sees in the
// daemon log.
var defaultClient = &http.Client{
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

// Endpoint is where, and how, one request is sent.
type Endpoint struct {
	// URL is the complete signal URL (".../v1/logs" or ".../v1/traces").
	URL string
	// Headers are added to the request. Values are credentials as often as
	// not; nothing in this package ever prints one.
	Headers map[string]string
	// Gzip compresses the body and sets Content-Encoding: gzip.
	Gzip bool
	// Timeout bounds the request (default DefaultTimeout).
	Timeout time.Duration
	// Client overrides the default *http.Client (tests inject one).
	Client *http.Client
}

// KeyValue is one attribute. Value is a string, bool, int, int64, float64,
// []string or []any; anything else is sent as its fmt string.
type KeyValue struct {
	Key   string
	Value any
}

// Resource is the OTLP resource every unit in a request belongs to.
type Resource struct {
	Attributes []KeyValue
}

// Scope is the instrumentation scope.
type Scope struct {
	Name    string
	Version string
}

// LogRecord is one OTLP LogRecord.
type LogRecord struct {
	Time           time.Time
	ObservedTime   time.Time
	SeverityNumber int
	SeverityText   string
	Body           string
	Attributes     []KeyValue
	// TraceID (32 hex) and SpanID (16 hex) correlate the record with a
	// trace; either may be empty.
	TraceID string
	SpanID  string
}

// Result is what one request came to.
type Result struct {
	// StatusCode is the HTTP status, or 0 when no response arrived.
	StatusCode int
	// Rejected is partialSuccess's rejected count on a 2xx.
	Rejected int64
	// ErrorMessage is partialSuccess's errorMessage on a 2xx, verbatim from
	// the collector; callers redact before logging it.
	ErrorMessage string
	// Err is nil when the collector accepted the request (2xx).
	Err error
	// Retryable reports a failure the OTLP specification says to retry: 429,
	// 502, 503, 504, or no response at all (network error, timeout).
	Retryable bool
	// RetryAfter is the collector's Retry-After, when it sent one.
	RetryAfter time.Duration
}

// OK reports whether the collector accepted the request.
func (r Result) OK() bool { return r.Err == nil }

// Class is a short, value-free name for the failure kind ("http_503",
// "network"), fit for a rate-limit key and a log field. Empty on success.
func (r Result) Class() string {
	switch {
	case r.Err == nil:
		return ""
	case r.StatusCode != 0:
		return "http_" + strconv.Itoa(r.StatusCode)
	default:
		return "network"
	}
}

// SendLogs POSTs records as one ExportLogsServiceRequest.
func SendLogs(ctx context.Context, ep Endpoint, res Resource, scope Scope, records []LogRecord) Result {
	out := make([]otlpLogRecord, 0, len(records))
	for _, r := range records {
		out = append(out, convertLogRecord(r))
	}
	body, err := json.Marshal(otlpLogs{ResourceLogs: []otlpResourceLogs{{
		Resource:  convertResource(res),
		ScopeLogs: []otlpScopeLogs{{Scope: otlpScope(scope), LogRecords: out}},
	}}})
	if err != nil {
		return Result{Err: fmt.Errorf("otlpexport: encode logs: %w", err)}
	}
	return post(ctx, ep, body, "rejectedLogRecords")
}

// SendSpans POSTs spans as one ExportTraceServiceRequest.
func SendSpans(ctx context.Context, ep Endpoint, res Resource, scope Scope, spans []otel.Span) Result {
	body, err := encodeSpans(res, scope, spans)
	if err != nil {
		return Result{Err: fmt.Errorf("otlpexport: encode spans: %w", err)}
	}
	return post(ctx, ep, body, "rejectedSpans")
}

func encodeSpans(res Resource, scope Scope, spans []otel.Span) ([]byte, error) {
	out := make([]otlpSpan, 0, len(spans))
	for _, sp := range spans {
		out = append(out, convertSpan(sp))
	}
	return json.Marshal(otlpTrace{ResourceSpans: []otlpResourceSpans{{
		Resource:   convertResource(res),
		ScopeSpans: []otlpScopeSpans{{Scope: otlpScope(scope), Spans: out}},
	}}})
}

// ExportConfig holds the connection parameters for Export.
type ExportConfig struct {
	// Endpoint is the OTLP/HTTP base URL; Export appends /v1/traces.
	Endpoint string
	// Headers are added to the request.
	Headers map[string]string
	// Client overrides the default *http.Client.
	Client *http.Client
}

// Export submits one whole agent-trace trace to <Endpoint>/v1/traces, with the
// session described in the resource. It predates SendSpans and keeps its
// contract for one-shot callers; the telemetry pipeline uses SendSpans.
func Export(ctx context.Context, trace otel.Trace, cfg ExportConfig) error {
	if cfg.Endpoint == "" {
		return fmt.Errorf("otlpexport: endpoint is required")
	}
	res := Resource{Attributes: []KeyValue{
		{"service.name", "harness"},
		{"agent.session.id", trace.Session.ID},
		{"agent.session.harness", string(trace.Session.Harness)},
	}}
	r := SendSpans(ctx, Endpoint{URL: strings.TrimRight(cfg.Endpoint, "/") + "/v1/traces", Headers: cfg.Headers, Client: cfg.Client},
		res, Scope{Name: "github.com/stump-wtf/agent-trace", Version: "1"}, trace.Spans)
	return r.Err
}

// post sends one request and classifies the response. rejectedField names the
// partialSuccess counter for the signal.
func post(ctx context.Context, ep Endpoint, body []byte, rejectedField string) Result {
	if ep.URL == "" {
		return Result{Err: errors.New("otlpexport: endpoint URL is required")}
	}
	if ep.Gzip {
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		if _, err := zw.Write(body); err != nil {
			return Result{Err: fmt.Errorf("otlpexport: gzip: %w", err)}
		}
		if err := zw.Close(); err != nil {
			return Result{Err: fmt.Errorf("otlpexport: gzip: %w", err)}
		}
		body = buf.Bytes()
	}
	timeout := ep.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ep.URL, bytes.NewReader(body))
	if err != nil {
		// The URL is operator-supplied and may carry a secret; say only what
		// kind of failure this was.
		return Result{Err: errors.New("otlpexport: invalid endpoint URL")}
	}
	for k, v := range ep.Headers {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "application/json")
	if ep.Gzip {
		req.Header.Set("Content-Encoding", "gzip")
	}

	client := ep.Client
	if client == nil {
		client = defaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return Result{Err: fmt.Errorf("otlpexport: %s", networkClass(err)), Retryable: true}
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBody))

	r := Result{StatusCode: resp.StatusCode}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		r.Rejected, r.ErrorMessage = parsePartialSuccess(raw, rejectedField)
		return r
	}
	r.Err = fmt.Errorf("otlpexport: collector returned %d", resp.StatusCode)
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		r.Retryable = true
		r.RetryAfter = ParseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
	}
	return r
}

// networkClass names a transport failure without echoing the URL (which
// url.Error would, credentials in the query string included).
func networkClass(err error) string {
	var ne net.Error
	switch {
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return "request timed out"
	case errors.Is(err, context.Canceled):
		return "request cancelled"
	default:
		var op *net.OpError
		if errors.As(err, &op) {
			return "network error (" + op.Op + ")"
		}
		return "network error"
	}
}

// ParseRetryAfter reads a Retry-After header: delay-seconds or an HTTP date.
// Zero means absent, unparseable or already past.
func ParseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// parsePartialSuccess reads {"partialSuccess":{"<field>":N,"errorMessage":…}}.
// The count is an int64, which the JSON mapping allows as a string or a number.
func parsePartialSuccess(raw []byte, field string) (int64, string) {
	var resp struct {
		PartialSuccess map[string]json.RawMessage `json:"partialSuccess"`
	}
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &resp) != nil || resp.PartialSuccess == nil {
		return 0, ""
	}
	var n int64
	if v, ok := resp.PartialSuccess[field]; ok {
		s := strings.Trim(string(v), `"`)
		n, _ = strconv.ParseInt(s, 10, 64)
	}
	var msg string
	if v, ok := resp.PartialSuccess["errorMessage"]; ok {
		_ = json.Unmarshal(v, &msg)
	}
	return n, msg
}

// --- OTLP JSON format ---
//
// https://opentelemetry.io/docs/specs/otlp/#json-protobuf-encoding

type otlpTrace struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpLogs struct {
	ResourceLogs []otlpResourceLogs `json:"resourceLogs"`
}

type otlpResourceLogs struct {
	Resource  otlpResource    `json:"resource"`
	ScopeLogs []otlpScopeLogs `json:"scopeLogs"`
}

type otlpScopeLogs struct {
	Scope      otlpScope       `json:"scope"`
	LogRecords []otlpLogRecord `json:"logRecords"`
}

type otlpLogRecord struct {
	TimeUnixNano         string    `json:"timeUnixNano"`
	ObservedTimeUnixNano string    `json:"observedTimeUnixNano"`
	SeverityNumber       int       `json:"severityNumber"`
	SeverityText         string    `json:"severityText"`
	Body                 otlpValue `json:"body"`
	Attributes           []otlpKV  `json:"attributes,omitempty"`
	TraceID              string    `json:"traceId,omitempty"`
	SpanID               string    `json:"spanId,omitempty"`
}

type otlpResource struct {
	Attributes []otlpKV `json:"attributes"`
}

type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpScope struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type otlpSpan struct {
	TraceID           string     `json:"traceId"`
	SpanID            string     `json:"spanId"`
	ParentSpanID      string     `json:"parentSpanId,omitempty"`
	Name              string     `json:"name"`
	Kind              int        `json:"kind"`
	StartTimeUnixNano string     `json:"startTimeUnixNano"`
	EndTimeUnixNano   string     `json:"endTimeUnixNano"`
	Attributes        []otlpKV   `json:"attributes,omitempty"`
	Status            otlpStatus `json:"status"`
}

type otlpStatus struct {
	Code    int    `json:"code"`
	Message string `json:"message,omitempty"`
}

type otlpKV struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

// otlpValue is an AnyValue. kind records which field is set, so MarshalJSON
// emits exactly that one even when it holds its zero value.
type otlpValue struct {
	StringValue string          `json:"stringValue,omitempty"`
	IntValue    string          `json:"intValue,omitempty"`
	DoubleValue float64         `json:"doubleValue,omitempty"`
	BoolValue   bool            `json:"boolValue,omitempty"`
	ArrayValue  *otlpArrayValue `json:"arrayValue,omitempty"`

	kind string
}

type otlpArrayValue struct {
	Values []otlpValue `json:"values"`
}

// MarshalJSON emits the one field kind names.
func (v otlpValue) MarshalJSON() ([]byte, error) {
	switch v.kind {
	case "int":
		return json.Marshal(map[string]string{"intValue": v.IntValue})
	case "double":
		return json.Marshal(map[string]float64{"doubleValue": v.DoubleValue})
	case "bool":
		return json.Marshal(map[string]bool{"boolValue": v.BoolValue})
	case "array":
		arr := v.ArrayValue
		if arr == nil || arr.Values == nil {
			arr = &otlpArrayValue{Values: []otlpValue{}}
		}
		return json.Marshal(map[string]*otlpArrayValue{"arrayValue": arr})
	default:
		return json.Marshal(map[string]string{"stringValue": v.StringValue})
	}
}

func stringValue(s string) otlpValue { return otlpValue{StringValue: s, kind: "string"} }

func convertResource(res Resource) otlpResource {
	return otlpResource{Attributes: convertKVs(res.Attributes)}
}

func convertKVs(kvs []KeyValue) []otlpKV {
	out := make([]otlpKV, 0, len(kvs))
	for _, kv := range kvs {
		out = append(out, otlpKV{Key: kv.Key, Value: convertValue(kv.Value)})
	}
	return out
}

func convertLogRecord(r LogRecord) otlpLogRecord {
	var attrs []otlpKV
	if len(r.Attributes) > 0 {
		attrs = convertKVs(r.Attributes)
	}
	return otlpLogRecord{
		TimeUnixNano:         unixNano(r.Time),
		ObservedTimeUnixNano: unixNano(r.ObservedTime),
		SeverityNumber:       r.SeverityNumber,
		SeverityText:         r.SeverityText,
		Body:                 stringValue(r.Body),
		Attributes:           attrs,
		TraceID:              r.TraceID,
		SpanID:               r.SpanID,
	}
}

func convertSpan(sp otel.Span) otlpSpan {
	return otlpSpan{
		TraceID:           sp.TraceID,
		SpanID:            sp.SpanID,
		ParentSpanID:      sp.ParentSpanID,
		Name:              sp.Name,
		Kind:              kindNumber(sp.Kind),
		StartTimeUnixNano: unixNano(sp.StartTime),
		EndTimeUnixNano:   unixNano(sp.EndTime),
		Attributes:        convertAttrs(sp.Attributes),
		Status:            convertStatus(sp.Status, sp.StatusMsg),
	}
}

// OTLP SpanKind enum: 1 INTERNAL, 2 SERVER, 3 CLIENT. agent-trace's own values
// start at 0, so they are translated, never passed through.
func kindNumber(k otel.SpanKind) int {
	switch k {
	case otel.SpanKindServer:
		return 2
	case otel.SpanKindClient:
		return 3
	default:
		return 1
	}
}

// OTLP StatusCode enum: 0 UNSET, 1 OK, 2 ERROR.
func convertStatus(code otel.StatusCode, msg string) otlpStatus {
	switch code {
	case otel.StatusOK:
		return otlpStatus{Code: 1}
	case otel.StatusError:
		return otlpStatus{Code: 2, Message: msg}
	default:
		return otlpStatus{Code: 0}
	}
}

// convertAttrs sorts by key so a request is byte-stable for its input.
func convertAttrs(attrs map[string]any) []otlpKV {
	if len(attrs) == 0 {
		return nil
	}
	keys := make([]string, 0, len(attrs))
	for k := range attrs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]otlpKV, 0, len(attrs))
	for _, k := range keys {
		out = append(out, otlpKV{Key: k, Value: convertValue(attrs[k])})
	}
	return out
}

func convertValue(v any) otlpValue {
	switch val := v.(type) {
	case string:
		return stringValue(val)
	case bool:
		return otlpValue{BoolValue: val, kind: "bool"}
	case int:
		return otlpValue{IntValue: strconv.FormatInt(int64(val), 10), kind: "int"}
	case int64:
		return otlpValue{IntValue: strconv.FormatInt(val, 10), kind: "int"}
	case float64:
		if math.IsNaN(val) || math.IsInf(val, 0) {
			// JSON cannot carry these; one bad attribute must not make the
			// whole request unencodable.
			return stringValue(strconv.FormatFloat(val, 'g', -1, 64))
		}
		return otlpValue{DoubleValue: val, kind: "double"}
	case []string:
		items := make([]otlpValue, len(val))
		for i, s := range val {
			items[i] = stringValue(s)
		}
		return otlpValue{ArrayValue: &otlpArrayValue{Values: items}, kind: "array"}
	case []any:
		items := make([]otlpValue, 0, len(val))
		for _, item := range val {
			items = append(items, convertValue(item))
		}
		return otlpValue{ArrayValue: &otlpArrayValue{Values: items}, kind: "array"}
	default:
		return stringValue(fmt.Sprintf("%v", v))
	}
}

func unixNano(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return strconv.FormatInt(t.UnixNano(), 10)
}

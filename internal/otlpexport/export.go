// Package otlpexport converts agent-trace's OTel trace model into standard
// OTLP/HTTP JSON and POSTs it to any OTLP-compatible endpoint.
//
// The exporter is endpoint-agnostic: it speaks standard OTLP JSON to whatever
// URL the daemon's otel_endpoint config names — Honeycomb, Tempo, Jaeger,
// Grafana, or a Cairn instance that exposes an OTLP endpoint. Harness does not
// know or care what is on the other end.
//
// Governing: ADR-0008 (secrets — only harvested trajectories are exported, and
// the endpoint is config-truth), SPEC-0006 REQ "Trajectory Discovery".
package otlpexport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"gitea.stump.rocks/stump.wtf/agent-trace/otel"
)

// DefaultTimeout caps a single export HTTP request.
const DefaultTimeout = 30 * time.Second

// ExportConfig holds the connection parameters for an OTLP export.
type ExportConfig struct {
	// Endpoint is the OTLP/HTTP base URL (e.g. "https://api.honeycomb.io" or
	// "https://cairn.stump.wtf"). The exporter appends /v1/traces.
	Endpoint string

	// Headers are optional HTTP headers added to the request (e.g.
	// {"x-honeycomb-team": "..."}). Secret values should be sourced from
	// the harness env_file, not hardcoded in config (ADR-0008).
	Headers map[string]string

	// Client overrides the default *http.Client (tests inject a test server).
	Client *http.Client
}

// Export submits an agent-trace OTel trace to any OTLP-compatible endpoint as
// standard OTLP/HTTP JSON. The trace is POSTed to <Endpoint>/v1/traces with
// Content-Type: application/json.
//
// The conversion from agent-trace's Span model to OTLP JSON is lossless:
// trace/span IDs, parent relationships, timing, attributes, status, and span
// kind are all mapped to their OTLP equivalents.
func Export(ctx context.Context, trace otel.Trace, cfg ExportConfig) error {
	if cfg.Endpoint == "" {
		return fmt.Errorf("otlpexport: endpoint is required")
	}

	body, err := buildOTLPJSON(trace)
	if err != nil {
		return fmt.Errorf("otlpexport: build OTLP JSON: %w", err)
	}

	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: DefaultTimeout}
	}

	url := cfg.Endpoint + "/v1/traces"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("otlpexport: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range cfg.Headers {
		req.Header.Set(k, v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("otlpexport: post to %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	// OTLP collectors return 200 on success. Read and discard the body to
	// reuse the connection.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("otlpexport: %s returned %d", url, resp.StatusCode)
	}

	return nil
}

// --- OTLP JSON format ---
//
// The OTLP/HTTP JSON format nests spans under resourceSpans → scopeSpans →
// spans. Each span carries traceId, spanId, parentSpanId, name, kind,
// startTimeUnixNano, endTimeUnixNano, attributes (as typed key-value pairs),
// and status. The format is defined by the OTLP spec:
// https://opentelemetry.io/docs/specs/otlp/#json-protobuf-encoding

type otlpTrace struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
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
	Kind              string     `json:"kind"`
	StartTimeUnixNano string     `json:"startTimeUnixNano"`
	EndTimeUnixNano   string     `json:"endTimeUnixNano"`
	Attributes        []otlpKV   `json:"attributes,omitempty"`
	Status            otlpStatus `json:"status"`
}

type otlpStatus struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

type otlpKV struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

type otlpValue struct {
	StringValue string          `json:"stringValue,omitempty"`
	IntValue    string          `json:"intValue,omitempty"`
	DoubleValue float64         `json:"doubleValue,omitempty"`
	BoolValue   bool            `json:"boolValue,omitempty"`
	ArrayValue  *otlpArrayValue `json:"arrayValue,omitempty"`
}

type otlpArrayValue struct {
	Values []otlpValue `json:"values"`
}

func buildOTLPJSON(trace otel.Trace) ([]byte, error) {
	spans := make([]otlpSpan, 0, len(trace.Spans))
	for _, sp := range trace.Spans {
		spans = append(spans, convertSpan(sp))
	}

	otlp := otlpTrace{
		ResourceSpans: []otlpResourceSpans{{
			Resource: otlpResource{
				Attributes: []otlpKV{
					{Key: "service.name", Value: otlpValue{StringValue: "harness"}},
					{Key: "agent.session.id", Value: otlpValue{StringValue: trace.Session.ID}},
					{Key: "agent.session.harness", Value: otlpValue{StringValue: string(trace.Session.Harness)}},
				},
			},
			ScopeSpans: []otlpScopeSpans{{
				Scope: otlpScope{
					Name:    "gitea.stump.rocks/stump.wtf/agent-trace",
					Version: "1",
				},
				Spans: spans,
			}},
		}},
	}

	return json.Marshal(otlp)
}

func convertSpan(sp otel.Span) otlpSpan {
	return otlpSpan{
		TraceID:           sp.TraceID,
		SpanID:            sp.SpanID,
		ParentSpanID:      sp.ParentSpanID,
		Name:              sp.Name,
		Kind:              kindString(sp.Kind),
		StartTimeUnixNano: unixNano(sp.StartTime),
		EndTimeUnixNano:   unixNano(sp.EndTime),
		Attributes:        convertAttrs(sp.Attributes),
		Status:            convertStatus(sp.Status, sp.StatusMsg),
	}
}

func kindString(k otel.SpanKind) string {
	switch k {
	case otel.SpanKindServer:
		return "SPAN_KIND_SERVER"
	case otel.SpanKindClient:
		return "SPAN_KIND_CLIENT"
	default:
		return "SPAN_KIND_INTERNAL"
	}
}

func convertStatus(code otel.StatusCode, msg string) otlpStatus {
	switch code {
	case otel.StatusOK:
		return otlpStatus{Code: "STATUS_CODE_OK"}
	case otel.StatusError:
		return otlpStatus{Code: "STATUS_CODE_ERROR", Message: msg}
	default:
		return otlpStatus{Code: "STATUS_CODE_UNSET"}
	}
}

func convertAttrs(attrs map[string]any) []otlpKV {
	if len(attrs) == 0 {
		return nil
	}
	out := make([]otlpKV, 0, len(attrs))
	for k, v := range attrs {
		out = append(out, otlpKV{Key: k, Value: convertValue(v)})
	}
	return out
}

func convertValue(v any) otlpValue {
	switch val := v.(type) {
	case string:
		return otlpValue{StringValue: val}
	case bool:
		return otlpValue{BoolValue: val}
	case int:
		return otlpValue{IntValue: fmt.Sprintf("%d", val)}
	case int64:
		return otlpValue{IntValue: fmt.Sprintf("%d", val)}
	case float64:
		return otlpValue{DoubleValue: val}
	case []string:
		items := make([]otlpValue, len(val))
		for i, s := range val {
			items[i] = otlpValue{StringValue: s}
		}
		return otlpValue{ArrayValue: &otlpArrayValue{Values: items}}
	case []any:
		items := make([]otlpValue, 0, len(val))
		for _, item := range val {
			items = append(items, convertValue(item))
		}
		return otlpValue{ArrayValue: &otlpArrayValue{Values: items}}
	default:
		return otlpValue{StringValue: fmt.Sprintf("%v", v)}
	}
}

func unixNano(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return fmt.Sprintf("%d", t.UnixNano())
}

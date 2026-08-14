package otlpexport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/otel"
	"github.com/stump-wtf/agent-trace/tail"
)

// testTrace returns a minimal deterministic trace for testing.
func testTrace() otel.Trace {
	return otel.Trace{
		TraceID: "0123456789abcdef0123456789abcdef",
		Session: tail.SessionMeta{
			ID:      "test-session",
			Harness: tail.HarnessClaudeCode,
		},
		Spans: []otel.Span{
			{
				TraceID:   "0123456789abcdef0123456789abcdef",
				SpanID:    "aaaaaaaaaaaaaaaa",
				Name:      "user message",
				Kind:      otel.SpanKindInternal,
				StartTime: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC),
				EndTime:   time.Date(2026, 1, 1, 10, 0, 5, 0, time.UTC),
				Attributes: map[string]any{
					"agent.turn.type":    "user-message",
					"agent.session.id":   "test-session",
					"agent.event.type":   "tool",
					"agent.tool.name":    "Bash",
					"agent.tool.action":  classify.ActionExec,
					"agent.result.bytes": int64(1024),
					"agent.targets":      "main.go,util.go",
				},
				Status: otel.StatusOK,
			},
			{
				TraceID:      "0123456789abcdef0123456789abcdef",
				SpanID:       "bbbbbbbbbbbbbbbb",
				ParentSpanID: "aaaaaaaaaaaaaaaa",
				Name:         "bash: go test",
				Kind:         otel.SpanKindInternal,
				StartTime:    time.Date(2026, 1, 1, 10, 0, 1, 0, time.UTC),
				EndTime:      time.Date(2026, 1, 1, 10, 0, 3, 0, time.UTC),
				Attributes: map[string]any{
					"agent.tool.name":   "Bash",
					"agent.tool.action": classify.ActionExec,
				},
				Status:    otel.StatusError,
				StatusMsg: "exit code 1",
			},
		},
	}
}

func TestExportSuccess(t *testing.T) {
	var receivedBody []byte
	var receivedPath string
	var receivedCT string
	var receivedAuth string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedPath = r.URL.Path
		receivedCT = r.Header.Get("Content-Type")
		receivedAuth = r.Header.Get("Authorization")
		receivedBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	trace := testTrace()
	cfg := ExportConfig{
		Endpoint: srv.URL,
		Headers:  map[string]string{"Authorization": "Bearer test-token"},
	}

	err := Export(context.Background(), trace, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if receivedPath != "/v1/traces" {
		t.Fatalf("path = %q, want /v1/traces", receivedPath)
	}
	if receivedCT != "application/json" {
		t.Fatalf("content-type = %q, want application/json", receivedCT)
	}
	if receivedAuth != "Bearer test-token" {
		t.Fatalf("auth = %q, want Bearer test-token", receivedAuth)
	}

	// Verify the body is valid OTLP JSON.
	var otlp otlpTrace
	if err := json.Unmarshal(receivedBody, &otlp); err != nil {
		t.Fatalf("response is not valid OTLP JSON: %v", err)
	}

	if len(otlp.ResourceSpans) != 1 {
		t.Fatalf("expected 1 resourceSpans, got %d", len(otlp.ResourceSpans))
	}
	rs := otlp.ResourceSpans[0]
	if len(rs.ScopeSpans) != 1 {
		t.Fatalf("expected 1 scopeSpans, got %d", len(rs.ScopeSpans))
	}
	spans := rs.ScopeSpans[0].Spans
	if len(spans) != 2 {
		t.Fatalf("expected 2 spans, got %d", len(spans))
	}

	// First span: user message.
	s0 := spans[0]
	if s0.TraceID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("traceId = %q", s0.TraceID)
	}
	if s0.SpanID != "aaaaaaaaaaaaaaaa" {
		t.Fatalf("spanId = %q", s0.SpanID)
	}
	if s0.Name != "user message" {
		t.Fatalf("name = %q, want 'user message'", s0.Name)
	}
	if s0.Kind != "SPAN_KIND_INTERNAL" {
		t.Fatalf("kind = %q, want SPAN_KIND_INTERNAL", s0.Kind)
	}
	if s0.StartTimeUnixNano != "1767261600000000000" {
		t.Fatalf("startTimeUnixNano = %q, want 1767261600000000000", s0.StartTimeUnixNano)
	}
	if s0.Status.Code != "STATUS_CODE_OK" {
		t.Fatalf("status code = %q, want STATUS_CODE_OK", s0.Status.Code)
	}

	// Second span: tool call with error.
	s1 := spans[1]
	if s1.ParentSpanID != "aaaaaaaaaaaaaaaa" {
		t.Fatalf("parentSpanId = %q, want aaaaaaaaaaaaaaaa", s1.ParentSpanID)
	}
	if s1.Status.Code != "STATUS_CODE_ERROR" {
		t.Fatalf("status code = %q, want STATUS_CODE_ERROR", s1.Status.Code)
	}
	if s1.Status.Message != "exit code 1" {
		t.Fatalf("status message = %q, want 'exit code 1'", s1.Status.Message)
	}
}

func TestExportServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	trace := testTrace()
	cfg := ExportConfig{Endpoint: srv.URL}

	err := Export(context.Background(), trace, cfg)
	if err == nil {
		t.Fatal("expected error for 500 response, got nil")
	}
}

func TestExportEmptyEndpoint(t *testing.T) {
	err := Export(context.Background(), testTrace(), ExportConfig{})
	if err == nil {
		t.Fatal("expected error for empty endpoint")
	}
}

func TestExportHeadersPassed(t *testing.T) {
	var gotHeaders http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = r.Header.Clone()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := ExportConfig{
		Endpoint: srv.URL,
		Headers: map[string]string{
			"X-Honeycomb-Team": "abc123",
			"X-Custom-Header":  "value",
		},
	}

	err := Export(context.Background(), testTrace(), cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if gotHeaders.Get("X-Honeycomb-Team") != "abc123" {
		t.Fatalf("missing X-Honeycomb-Team header")
	}
	if gotHeaders.Get("X-Custom-Header") != "value" {
		t.Fatalf("missing X-Custom-Header header")
	}
}

func TestConvertValueTypes(t *testing.T) {
	tests := []struct {
		name  string
		val   any
		check func(v otlpValue)
	}{
		{"string", "hello", func(v otlpValue) {
			if v.StringValue != "hello" {
				t.Errorf("string: got %v", v)
			}
		}},
		{"bool", true, func(v otlpValue) {
			if !v.BoolValue {
				t.Errorf("bool: got %v", v)
			}
		}},
		{"int", int(42), func(v otlpValue) {
			if v.IntValue != "42" {
				t.Errorf("int: got %v", v)
			}
		}},
		{"int64", int64(99), func(v otlpValue) {
			if v.IntValue != "99" {
				t.Errorf("int64: got %v", v)
			}
		}},
		{"float64", float64(3.14), func(v otlpValue) {
			if v.DoubleValue != 3.14 {
				t.Errorf("float: got %v", v)
			}
		}},
		{"[]string", []string{"a", "b"}, func(v otlpValue) {
			if v.ArrayValue == nil || len(v.ArrayValue.Values) != 2 {
				t.Errorf("array: got %v", v)
			}
		}},
		{"unknown", struct{}{}, func(v otlpValue) {
			if v.StringValue == "" {
				t.Errorf("unknown: got %v", v)
			}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := convertValue(tt.val)
			tt.check(v)
		})
	}
}

func TestResourceAttributes(t *testing.T) {
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	err := Export(context.Background(), testTrace(), ExportConfig{Endpoint: srv.URL})
	if err != nil {
		t.Fatal(err)
	}

	var otlp otlpTrace
	json.Unmarshal(body, &otlp)

	attrs := otlp.ResourceSpans[0].Resource.Attributes
	found := map[string]string{}
	for _, kv := range attrs {
		found[kv.Key] = kv.Value.StringValue
	}

	if found["service.name"] != "harness" {
		t.Errorf("service.name = %q, want harness", found["service.name"])
	}
	if found["agent.session.id"] != "test-session" {
		t.Errorf("agent.session.id = %q", found["agent.session.id"])
	}
	if found["agent.session.harness"] != "claude-code" {
		t.Errorf("agent.session.harness = %q", found["agent.session.harness"])
	}
}

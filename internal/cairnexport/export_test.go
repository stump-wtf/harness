package cairnexport

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/agent-trace/classify"
	"gitea.stump.rocks/stump.wtf/agent-trace/otel"
	"gitea.stump.rocks/stump.wtf/agent-trace/tail"
)

// helper: build a trace from fixed inputs for deterministic testing.
func testTrace() otel.Trace {
	session := tail.SessionMeta{
		Key:       "test-session-key",
		ID:        "sess-123",
		Harness:   tail.HarnessClaudeCode,
		StartedAt: "2026-08-10T10:00:00Z",
	}
	events := []classify.Event{
		{Seq: 0, Timestamp: "2026-08-10T10:00:05Z", Tool: "Read", Action: classify.ActionRead, Summary: "read main.go"},
		{Seq: 1, Timestamp: "2026-08-10T10:00:10Z", Tool: "Edit", Action: classify.ActionEdit, Summary: "edit main.go"},
		{Seq: 2, Timestamp: "2026-08-10T10:00:15Z", Tool: "Bash", Action: classify.ActionVerify, IsError: true, Summary: "go test failed"},
	}
	marks := []classify.Mark{
		{Seq: 0, Timestamp: "2026-08-10T10:00:00Z", Type: "user-message", Note: "fix the login bug"},
	}
	return otel.BuildTrace(session, events, marks)
}

func TestExportDisabledByDefault(t *testing.T) {
	_, err := Export(context.Background(), testTrace(), ExportConfig{})
	if err != ErrExportDisabled {
		t.Errorf("expected ErrExportDisabled, got %v", err)
	}
}

func TestExportRequiresEndpoint(t *testing.T) {
	_, err := Export(context.Background(), testTrace(), ExportConfig{
		ExportEnabled: true,
		Token:         "tok",
	})
	if err == nil || err.Error() != "cairnexport: endpoint is required" {
		t.Errorf("expected endpoint-required error, got %v", err)
	}
}

func TestExportRequiresToken(t *testing.T) {
	_, err := Export(context.Background(), testTrace(), ExportConfig{
		ExportEnabled: true,
		Endpoint:      "https://cairn.example",
	})
	if err == nil || err.Error() != "cairnexport: token is required" {
		t.Errorf("expected token-required error, got %v", err)
	}
}

func TestExportSuccess(t *testing.T) {
	var receivedBody runRequest
	var receivedAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/runs" {
			t.Errorf("expected POST /v1/runs, got %s", r.URL.Path)
		}
		receivedAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &receivedBody)

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"abc123","url":"https://cairn.example/run/abc123"}`))
	}))
	defer srv.Close()

	result, err := Export(context.Background(), testTrace(), ExportConfig{
		ExportEnabled: true,
		Endpoint:      srv.URL,
		Token:         "test-token",
		Model:         "claude-opus-5",
		OnBehalfOf:    "joestump",
		Client:        srv.Client(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.RunID != "abc123" {
		t.Errorf("expected RunID abc123, got %s", result.RunID)
	}
	if result.URL != "https://cairn.example/run/abc123" {
		t.Errorf("unexpected URL: %s", result.URL)
	}
	if receivedAuth != "Bearer test-token" {
		t.Errorf("expected Bearer auth, got %s", receivedAuth)
	}
	if receivedBody.Mode != "batch" {
		t.Errorf("expected batch mode, got %s", receivedBody.Mode)
	}
	if receivedBody.Model != "claude-opus-5" {
		t.Errorf("expected model, got %s", receivedBody.Model)
	}
	if receivedBody.StartedAt.IsZero() {
		t.Error("expected non-zero StartedAt")
	}
}

func TestExportServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"internal"}`))
	}))
	defer srv.Close()

	_, err := Export(context.Background(), testTrace(), ExportConfig{
		ExportEnabled: true,
		Endpoint:      srv.URL,
		Token:         "tok",
		Client:        srv.Client(),
	})
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
}

// TestCategoryMapping verifies the otel.Span → Cairn category mapping.
func TestCategoryMapping(t *testing.T) {
	trace := testTrace()

	categories := make(map[string]string) // spanID → category
	req := buildRunRequest(trace, ExportConfig{Model: "test"})
	for _, sp := range req.Spans {
		categories[sp.SpanID] = sp.Category
	}

	// The trace should contain: 1 prompt (user message), at least 1 read,
	// 1 write (edit), 1 test (verify).
	hasPrompt, hasRead, hasWrite, hasTest := false, false, false, false
	for _, cat := range categories {
		switch cat {
		case "prompt":
			hasPrompt = true
		case "read":
			hasRead = true
		case "write":
			hasWrite = true
		case "test":
			hasTest = true
		}
	}
	if !hasPrompt {
		t.Error("expected a 'prompt' category span for the user message")
	}
	if !hasRead {
		t.Error("expected a 'read' category span for the Read tool")
	}
	if !hasWrite {
		t.Error("expected a 'write' category span for the Edit tool")
	}
	if !hasTest {
		t.Error("expected a 'test' category span for the Verify tool")
	}
}

// TestTimingNonNegative asserts all start offsets and durations are
// non-negative — Cairn rejects negative timing.
func TestTimingNonNegative(t *testing.T) {
	trace := testTrace()
	req := buildRunRequest(trace, ExportConfig{})

	for _, sp := range req.Spans {
		if sp.StartOffsetMS < 0 {
			t.Errorf("span %s has negative start_offset_ms: %d", sp.SpanID, sp.StartOffsetMS)
		}
		if sp.DurationMS < 0 {
			t.Errorf("span %s has negative duration_ms: %d", sp.SpanID, sp.DurationMS)
		}
	}
}

// TestIdempotentSpanIDs verifies that re-building the same trace produces the
// same span IDs, which is what makes re-export idempotent on the Cairn side.
func TestIdempotentSpanIDs(t *testing.T) {
	trace1 := testTrace()
	trace2 := testTrace()

	if trace1.TraceID != trace2.TraceID {
		t.Fatal("trace IDs differ for identical inputs")
	}
	if len(trace1.Spans) != len(trace2.Spans) {
		t.Fatalf("span count differs: %d vs %d", len(trace1.Spans), len(trace2.Spans))
	}
	for i := range trace1.Spans {
		if trace1.Spans[i].SpanID != trace2.Spans[i].SpanID {
			t.Errorf("span %d ID differs: %s vs %s", i, trace1.Spans[i].SpanID, trace2.Spans[i].SpanID)
		}
	}
}

// TestParentChildStructure verifies tool spans are children of the turn span.
func TestParentChildStructure(t *testing.T) {
	trace := testTrace()

	// The first span should be the turn (user message), with no parent.
	if len(trace.Spans) == 0 {
		t.Fatal("no spans")
	}
	turnSpan := trace.Spans[0]
	if turnSpan.ParentSpanID != "" {
		t.Errorf("first span (turn) should have no parent, got %s", turnSpan.ParentSpanID)
	}
	if _, isTurn := turnSpan.Attributes["agent.turn.type"]; !isTurn {
		t.Error("first span should be a turn (user message)")
	}

	// Subsequent spans should be children of the turn span.
	for _, sp := range trace.Spans[1:] {
		if sp.ParentSpanID == "" {
			t.Errorf("span %s should have a parent", sp.SpanID)
		}
		// Verify the parent exists in the trace.
		found := false
		for _, other := range trace.Spans {
			if other.SpanID == sp.ParentSpanID {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("span %s references unknown parent %s", sp.SpanID, sp.ParentSpanID)
		}
	}
}

// TestToolSpanHasToolName verifies tool-call spans carry the tool name.
func TestToolSpanHasToolName(t *testing.T) {
	trace := testTrace()
	req := buildRunRequest(trace, ExportConfig{})

	foundToolName := false
	for _, sp := range req.Spans {
		if sp.Tool != "" && sp.Category != "prompt" {
			foundToolName = true
			break
		}
	}
	if !foundToolName {
		t.Error("expected at least one tool span with a non-empty tool name")
	}
}

// TestPromptSpanHasNoTool verifies prompt/turn spans do not carry a tool name,
// per Cairn's constraint that reason spans take no tool.
func TestPromptSpanHasNoTool(t *testing.T) {
	trace := testTrace()
	req := buildRunRequest(trace, ExportConfig{})

	for _, sp := range req.Spans {
		if sp.Category == "prompt" && sp.Tool != "" {
			t.Errorf("prompt span %s should not carry tool %q", sp.SpanID, sp.Tool)
		}
	}
}

// TestExportFromEvents verifies the convenience entry point builds a trace
// and exports it.
func TestExportFromEvents(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"xyz","url":"https://cairn.example/run/xyz"}`))
	}))
	defer srv.Close()

	session := tail.SessionMeta{
		Key:       "k",
		ID:        "s",
		Harness:   tail.HarnessClaudeCode,
		StartedAt: "2026-08-10T10:00:00Z",
	}
	events := []classify.Event{
		{Seq: 0, Timestamp: "2026-08-10T10:00:01Z", Tool: "Bash", Action: classify.ActionExec, Summary: "ls"},
	}
	marks := []classify.Mark{
		{Seq: 0, Timestamp: "2026-08-10T10:00:00Z", Type: "user-message", Note: "list files"},
	}

	result, err := ExportFromEvents(context.Background(), session, events, marks, ExportConfig{
		ExportEnabled: true,
		Endpoint:      srv.URL,
		Token:         "tok",
		Client:        srv.Client(),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.RunID != "xyz" {
		t.Errorf("expected RunID xyz, got %s", result.RunID)
	}
}

// TestDeriveTitle verifies the run title is derived from the first user message.
func TestDeriveTitle(t *testing.T) {
	trace := testTrace()
	req := buildRunRequest(trace, ExportConfig{})
	if req.Title != "fix the login bug" {
		t.Errorf("expected title from first user message, got %q", req.Title)
	}
}

// TestEmptyTrace verifies the exporter handles an empty trace without panicking.
func TestEmptyTrace(t *testing.T) {
	emptyTrace := otel.BuildTrace(
		tail.SessionMeta{Key: "empty", ID: "e", Harness: tail.HarnessClaudeCode, StartedAt: "2026-08-10T10:00:00Z"},
		nil, nil,
	)
	req := buildRunRequest(emptyTrace, ExportConfig{})
	if len(req.Spans) != 0 {
		t.Errorf("expected 0 spans for empty trace, got %d", len(req.Spans))
	}
	if req.Title != "e" {
		t.Errorf("expected session ID as title for empty trace, got %q", req.Title)
	}
}

// TestCompactionCategory verifies compaction spans map to "meta".
func TestCompactionCategory(t *testing.T) {
	session := tail.SessionMeta{Key: "c", ID: "c1", Harness: tail.HarnessClaudeCode, StartedAt: "2026-08-10T10:00:00Z"}
	events := []classify.Event{}
	marks := []classify.Mark{
		{Seq: 0, Timestamp: "2026-08-10T10:00:00Z", Type: "user-message", Note: "do work"},
		{Seq: 1, Timestamp: "2026-08-10T10:00:05Z", Type: "compaction", Note: ""},
	}
	trace := otel.BuildTrace(session, events, marks)
	req := buildRunRequest(trace, ExportConfig{})

	foundMeta := false
	for _, sp := range req.Spans {
		if sp.Category == "meta" {
			foundMeta = true
		}
	}
	if !foundMeta {
		t.Error("expected a 'meta' category span for compaction")
	}
}

// TestSubagentCategory verifies subagent spans map to "net" with tool "sub-agent".
func TestSubagentCategory(t *testing.T) {
	session := tail.SessionMeta{Key: "s", ID: "s1", Harness: tail.HarnessClaudeCode, StartedAt: "2026-08-10T10:00:00Z"}
	events := []classify.Event{}
	marks := []classify.Mark{
		{Seq: 0, Timestamp: "2026-08-10T10:00:00Z", Type: "user-message", Note: "delegate"},
		{Seq: 1, Timestamp: "2026-08-10T10:00:02Z", Type: "subagent", Note: "researcher"},
	}
	trace := otel.BuildTrace(session, events, marks)
	req := buildRunRequest(trace, ExportConfig{})

	foundSubagent := false
	for _, sp := range req.Spans {
		if sp.Category == "net" && sp.Tool == "sub-agent" {
			foundSubagent = true
		}
	}
	if !foundSubagent {
		t.Error("expected a 'net' category span with tool 'sub-agent' for subagent")
	}
}

func TestOffsetMS(t *testing.T) {
	base := time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)
	later := base.Add(5 * time.Second)
	if got := offsetMS(base, later); got != 5000 {
		t.Errorf("expected 5000, got %d", got)
	}
	// Before base → clamped to 0.
	if got := offsetMS(base, base.Add(-1*time.Second)); got != 0 {
		t.Errorf("expected 0 for negative offset, got %d", got)
	}
	// Zero times → 0.
	if got := offsetMS(time.Time{}, time.Time{}); got != 0 {
		t.Errorf("expected 0 for zero times, got %d", got)
	}
}

func TestDurationMS(t *testing.T) {
	start := time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)
	end := start.Add(3 * time.Second)
	if got := durationMS(start, end); got != 3000 {
		t.Errorf("expected 3000, got %d", got)
	}
	// End before start → clamped to 0.
	if got := durationMS(end, start); got != 0 {
		t.Errorf("expected 0 for negative duration, got %d", got)
	}
}

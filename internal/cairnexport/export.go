// Package cairnexport converts an agent-trace OTel trace into a Cairn
// trajectory share and submits it via POST /v1/runs.
//
// It bridges agent-trace's deterministic span model (otel.Trace) to Cairn's
// OTel-inspired ingest format, mapping categories, timing, and parent
// relationships so the resulting Cairn waterfall renders a faithful view of
// the agent session.
//
// Export is always opt-in: the caller must pass an ExportConfig with ExportEnabled
// set to true. This is a distinct consent gate from the trajectory harvest
// opt-in (issue #91) — consenting to local reads is not the same as consenting
// to publication as a shareable URL (ADR-0008, issue #94).
//
// Governing: SPEC-0006 (agent-adapters), ADR-0008 (secrets), issue #94.
package cairnexport

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"gitea.stump.rocks/stump.wtf/agent-trace/classify"
	"gitea.stump.rocks/stump.wtf/agent-trace/otel"
	"gitea.stump.rocks/stump.wtf/agent-trace/tail"
)

// DefaultTimeout caps a single export HTTP request.
const DefaultTimeout = 30 * time.Second

// ExportConfig holds the consent and connection parameters for exporting a
// trajectory to Cairn. All fields are required for a valid export.
type ExportConfig struct {
	// ExportEnabled is the publication opt-in. This is distinct from the
	// harvest opt-in: harvest allows the daemon to READ a trajectory locally;
	// this allows PUBLISHING it as a shareable URL. Must be true to export.
	ExportEnabled bool

	// Endpoint is the Cairn base URL (e.g. "https://cairn.stump.wtf").
	Endpoint string

	// Token is a Cairn PAT used for authentication.
	Token string

	// Model identifies the agent model that produced the trace, recorded in
	// the run's provenance.
	Model string

	// OnBehalfOf records the human the agent is acting for, if known.
	OnBehalfOf string

	// Client overrides the default *http.Client (tests inject a test server).
	Client *http.Client
}

// Result is the outcome of a successful export: the Cairn run's public id and
// its shareable web URL.
type Result struct {
	RunID string
	URL   string
}

// ErrExportDisabled is returned when ExportConfig.ExportEnabled is false. It is
// a sentinel so callers can distinguish "not allowed" from a network failure.
const ErrExportDisabled = cairnError("cairnexport: export is not enabled for this harness (publication opt-in required, issue #94)")

type cairnError string

func (e cairnError) Error() string { return string(e) }

// spanRequest is the POST /v1/runs body shape for a single span. It mirrors
// Cairn's internal/httpapi.spanRequest without importing the server package.
type spanRequest struct {
	SpanID       string          `json:"span_id"`
	ParentSpanID string          `json:"parent_span_id,omitempty"`
	Category     string          `json:"category"`
	Name         string          `json:"name,omitempty"`
	Tool         string          `json:"tool,omitempty"`
	Args         json.RawMessage `json:"args,omitempty"`
	StartOffsetMS int            `json:"start_offset_ms"`
	DurationMS   int             `json:"duration_ms"`
}

type runRequest struct {
	Mode       string        `json:"mode"`
	Title      string        `json:"title,omitempty"`
	Prompt     string        `json:"prompt,omitempty"`
	Model      string        `json:"model,omitempty"`
	StartedAt  time.Time     `json:"started_at"`
	OnBehalfOf string        `json:"on_behalf_of,omitempty"`
	Spans      []spanRequest `json:"spans"`
}

type runResponse struct {
	ID  string `json:"id"`
	URL string `json:"url"`
}

// Export submits an agent-trace OTel trace to Cairn as a batch trajectory run.
// The trace's deterministic IDs (derived from the session key by otel.BuildTrace)
// make re-export idempotent: the same input always produces the same span IDs,
// and a re-submission to the same endpoint updates rather than duplicates.
//
// Returns ErrExportDisabled if ExportConfig.ExportEnabled is false — this is
// the publication consent gate, separate from any local harvest opt-in.
func Export(ctx context.Context, trace otel.Trace, cfg ExportConfig) (*Result, error) {
	if !cfg.ExportEnabled {
		return nil, ErrExportDisabled
	}
	if cfg.Endpoint == "" {
		return nil, fmt.Errorf("cairnexport: endpoint is required")
	}
	if cfg.Token == "" {
		return nil, fmt.Errorf("cairnexport: token is required")
	}

	body, err := json.Marshal(buildRunRequest(trace, cfg))
	if err != nil {
		return nil, fmt.Errorf("cairnexport: marshal request: %w", err)
	}

	client := cfg.Client
	if client == nil {
		client = &http.Client{Timeout: DefaultTimeout}
	}

	url := cfg.Endpoint + "/v1/runs"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("cairnexport: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+cfg.Token)

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cairnexport: post to %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("cairnexport: read response: %w", err)
	}

	if resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("cairnexport: Cairn returned %d: %s", resp.StatusCode, string(respBody))
	}

	var rr runResponse
	if err := json.Unmarshal(respBody, &rr); err != nil {
		return nil, fmt.Errorf("cairnexport: decode response: %w", err)
	}

	return &Result{RunID: rr.ID, URL: rr.URL}, nil
}

// ExportFromEvents is a convenience that builds the OTel trace from session
// metadata, events, and marks — then calls Export. This is the primary entry
// point for the daemon: it has the classified events and session metadata, and
// the trace-building is delegated to agent-trace's otel.BuildTrace.
func ExportFromEvents(ctx context.Context, session tail.SessionMeta, events []classify.Event, marks []classify.Mark, cfg ExportConfig) (*Result, error) {
	trace := otel.BuildTrace(session, events, marks)
	return Export(ctx, trace, cfg)
}

// buildRunRequest converts an agent-trace OTel trace into a Cairn POST /v1/runs
// request body. The mapping is:
//
//   - otel.Span.SpanID → span_id (deterministic, so re-export is idempotent)
//   - otel.Span.ParentSpanID → parent_span_id (preserves the turn→tool tree)
//   - otel.Span timing → start_offset_ms/duration_ms relative to session start
//   - Span kind + attributes → Cairn category and tool name
//   - Turn spans (user-message) → category "prompt"
//   - Tool-call spans → category derived from the tool action attribute
//   - Compaction spans → category "meta"
//   - Subagent spans → category "net", tool "sub-agent"
//
// The run title is derived from the first user-message mark (or the session ID),
// and the model is recorded in both the run header and provenance.
func buildRunRequest(trace otel.Trace, cfg ExportConfig) runRequest {
	sessionStart := parseTime(trace.Session.StartedAt)
	spans := make([]spanRequest, 0, len(trace.Spans))

	for _, sp := range trace.Spans {
		sr := spanRequest{
			SpanID:       sp.SpanID,
			ParentSpanID: sp.ParentSpanID,
			Category:     mapCategory(sp),
			Name:         sp.Name,
			Tool:         mapTool(sp),
			StartOffsetMS: offsetMS(sessionStart, sp.StartTime),
			DurationMS:   durationMS(sp.StartTime, sp.EndTime),
		}
		if args := mapArgs(sp); args != nil {
			sr.Args = args
		}
		spans = append(spans, sr)
	}

	return runRequest{
		Mode:       "batch",
		Title:      deriveTitle(trace),
		Model:      cfg.Model,
		StartedAt:  sessionStart,
		OnBehalfOf: cfg.OnBehalfOf,
		Spans:      spans,
	}
}

// mapCategory translates an otel.Span's kind and attributes into a Cairn
// category. Cairn's category set is open, but using the recommended vocabulary
// ensures the waterfall renders with semantic colors.
func mapCategory(sp otel.Span) string {
	// Turn spans (user messages) are prompt markers.
	if _, isTurn := sp.Attributes["agent.turn.type"]; isTurn {
		return "prompt"
	}

	// Compaction and subagent have explicit event types.
	switch sp.Attributes["agent.event.type"] {
	case "compaction":
		return "meta"
	case "subagent":
		return "net"
	}

	// Tool-call spans: derive a category from the tool action attribute.
	action := sp.Attributes["agent.tool.action"]
	switch action {
	case classify.ActionRead:
		return "read"
	case classify.ActionEdit, classify.ActionOther:
		return "write"
	case classify.ActionExec:
		return "exec"
	case classify.ActionSearch:
		return "search"
	case classify.ActionVerify:
		return "test"
	default:
		// A tool call with no recognized action is still a tool span.
		if _, hasTool := sp.Attributes["agent.tool.name"]; hasTool {
			return "tool"
		}
		return "meta"
	}
}

// mapTool extracts the tool name from a span's attributes. Reason/prompt spans
// carry no tool — only tool-call and subagent spans do.
func mapTool(sp otel.Span) string {
	if tool := sp.Attributes["agent.tool.name"]; tool != "" {
		return tool
	}
	if sp.Attributes["agent.event.type"] == "subagent" {
		return "sub-agent"
	}
	return ""
}

// mapArgs serializes a span's attributes as JSON args, so the Cairn viewer can
// display the tool action, targets, and session context in the span detail.
func mapArgs(sp otel.Span) json.RawMessage {
	if len(sp.Attributes) == 0 {
		return nil
	}
	b, err := json.Marshal(sp.Attributes)
	if err != nil {
		return nil
	}
	return b
}

// deriveTitle returns the first span's name (which, for a trace built by
// otel.BuildTrace, is the first user message), or falls back to the session ID.
func deriveTitle(trace otel.Trace) string {
	for _, sp := range trace.Spans {
		if _, isTurn := sp.Attributes["agent.turn.type"]; isTurn && sp.Name != "" {
			return sp.Name
		}
	}
	return trace.Session.ID
}

// offsetMS returns the milliseconds between base and t. Negative or zero
// offsets are clamped to 0 (Cairn requires non-negative start_offset_ms).
func offsetMS(base, t time.Time) int {
	if t.IsZero() || base.IsZero() {
		return 0
	}
	d := int(t.Sub(base).Milliseconds())
	if d < 0 {
		return 0
	}
	return d
}

// durationMS returns the duration between start and end in milliseconds,
// clamped to a minimum of 0.
func durationMS(start, end time.Time) int {
	if end.IsZero() || start.IsZero() {
		return 0
	}
	d := int(end.Sub(start).Milliseconds())
	if d < 0 {
		return 0
	}
	return d
}

func parseTime(ts string) time.Time {
	if ts == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t
		}
	}
	return time.Time{}
}

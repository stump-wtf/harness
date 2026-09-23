package metrics

// Error-Side Observability Tests
//
// Governing tests: SPEC-0013 REQ-3 and REQ-6 — a harness whose adapter's
// provider errors never reach the observer has no error-side series, rather
// than a class="quota" zero that no outage could ever move.
//
// @joestump-agent 09/23/2026 - Added in review (harness#589).

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stump-wtf/agent-trace/tail"

	"github.com/stump-wtf/harness/internal/core"
)

// The error outcome, every class and the unclassified control exist only for
// an adapter whose errors the observer can see; the success side (and the
// session series) stay for every observable harness.
func TestErrorSeriesOnlyWhereErrorsAreObservable(t *testing.T) {
	src := newFakeSource()
	src.add(crushHarness("crush-worker"), runningSnap())
	src.add(core.Harness{Name: "cc-worker", Adapter: "claude-code", Workdir: "/w/cc"}, runningSnap())
	src.add(core.Harness{Name: "codex-worker", Adapter: "codex", Workdir: "/w/codex"}, runningSnap())
	fams := scrape(t, newTestMetrics(t, src, Options{Observer: newFakeFeed()}))

	for _, h := range []string{"crush-worker", "cc-worker", "codex-worker"} {
		if _, ok := fams.get("harness_model_calls_total", lbls("harness", h, "outcome", "success")); !ok {
			t.Errorf("%s: success calls missing; the success side is observable for every transcript adapter", h)
		}
		if _, ok := fams.get("harness_session_active", lbls("harness", h)); !ok {
			t.Errorf("%s: session_active missing", h)
		}
	}
	if _, ok := fams.get("harness_model_call_errors_total", lbls("harness", "crush-worker", "class", "quota")); !ok {
		t.Error("crush-worker: class=quota missing; crush errors are observable")
	}
	for _, h := range []string{"cc-worker", "codex-worker"} {
		if v, ok := fams.get("harness_model_calls_total", lbls("harness", h, "outcome", "error")); ok {
			t.Errorf("%s: outcome=error reported as %v; its errors never reach the observer, so it must be absent", h, v)
		}
		if v, ok := fams.get("harness_model_call_errors_total", lbls("harness", h, "class", "quota")); ok {
			t.Errorf("%s: class=quota reported as %v; a zero here would read as no quota errors during an outage", h, v)
		}
		if v, ok := fams.get("harness_model_call_errors_unclassified_total", lbls("harness", h)); ok {
			t.Errorf("%s: unclassified reported as %v", h, v)
		}
	}
}

// ErrorsObservable is a claim about agent-trace, so it is checked against
// agent-trace: a Claude Code transcript holding a failed API call (the
// isApiErrorMessage record, shape from agent-trace's own testdata) is parsed
// by the pinned parser, and whether an error mark comes out must match what
// ErrorsObservable says. When the dependency bump that carries
// stump.wtf/agent-trace#104 lands, this fails until ErrorsObservable is
// flipped for claude-code — the collector cannot go on omitting errors that
// have started arriving, nor report zeros for ones that have not.
func TestErrorsObservableMatchesAgentTrace(t *testing.T) {
	const transcript = `{"parentUuid":null,"isSidechain":false,"userType":"external","cwd":"/w/cc","sessionId":"cc-err","version":"2.0.0","type":"user","uuid":"u1","timestamp":"2026-09-14T03:00:00.000Z","message":{"role":"user","content":"run the sweep"}}
{"parentUuid":"u1","isSidechain":false,"userType":"external","cwd":"/w/cc","sessionId":"cc-err","version":"2.0.0","type":"assistant","uuid":"a1","timestamp":"2026-09-14T03:00:01.000Z","message":{"id":"msg_a1","type":"message","role":"assistant","model":"claude-test-model","content":[{"type":"tool_use","id":"call-1","name":"Read","input":{"file_path":"/w/cc/main.go"}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}}
{"parentUuid":"a1","isSidechain":false,"userType":"external","cwd":"/w/cc","sessionId":"cc-err","version":"2.0.0","type":"user","uuid":"u2","timestamp":"2026-09-14T03:00:02.000Z","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"package main"}]}}
{"parentUuid":"u2","isSidechain":false,"userType":"external","cwd":"/w/cc","sessionId":"cc-err","version":"2.0.0","type":"assistant","uuid":"a2","timestamp":"2026-09-14T03:00:03.000Z","message":{"id":"msg_a2","type":"message","role":"assistant","model":"<synthetic>","content":[{"type":"text","text":"You've hit your usage limit"}],"stop_reason":"stop_sequence","usage":{"input_tokens":0,"output_tokens":0}},"error":"rate_limit","apiErrorStatus":429,"isApiErrorMessage":true}
`
	path := filepath.Join(t.TempDir(), "cc-err.jsonl")
	if err := os.WriteFile(path, []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}
	events, marks, _, err := tail.ClaudeCodeAdapter{}.Parse(context.Background(), path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	// The control: the parser read the file, so "no error mark" below is a
	// finding about error marks, not about an unreadable fixture.
	if len(events) == 0 {
		t.Fatal("agent-trace parsed no tool call from the fixture; the canary proves nothing")
	}
	sawError := false
	for _, mk := range marks {
		if mk.Type == "error" {
			sawError = true
		}
	}
	h := core.Harness{Name: "cc", Adapter: "claude-code", Workdir: "/w/cc"}
	if got := ErrorsObservable(h); got != sawError {
		t.Errorf("ErrorsObservable(claude-code) = %v, but the pinned agent-trace emits an error mark for a failed API call: %v — update ErrorsObservable and docs/usage/metrics.md together", got, sawError)
	}
}

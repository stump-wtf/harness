package supervisor

// A pipe run's own stream, folded into its ledger record at run end
// (issue #994): tests.
//
// A claude-code one-shot whose transcript never flushed records its usage
// from its own stream: the run's end reads the sealed <id>.stream.jsonl
// once, and the closed line carries the tokens and the agent's recorded
// cost. The fake claude writes only the stream, so the transcript path has
// nothing to record and the stream's result is the run's usage.

import (
	"context"
	"os"
	"testing"

	"github.com/stump-wtf/agent-trace/tail"

	"github.com/stump-wtf/harness/internal/ledger"
)

// sinkFrom is the test stand-in for the daemon's sink: it parses the
// run's stream file with the tail parser for the run's kind and folds the
// final result into acc, the way the daemon's does (internal/runusage).
func sinkFrom(t *testing.T, acc *ledger.Accumulator) StreamUsageSink {
	t.Helper()
	return func(name, kind string, runID int, streamPath string) {
		var sp tail.StreamParser
		if kind == "claude-code" {
			sp = tail.ClaudeCodeAdapter{}
		}
		if sp == nil {
			return
		}
		f, err := os.Open(streamPath)
		if err != nil {
			return
		}
		defer f.Close()
		_, res, err := sp.ParseStream(context.Background(), f, tail.StreamHandler{})
		if err != nil || res == nil {
			return
		}
		var cost *float64
		if res.CostUSD > 0 {
			c := res.CostUSD
			cost = &c
		}
		_ = acc.FoldResult(name, runID, ledger.UsageReport{
			Tokens: ledger.Tokens{
				Input:      res.Usage.InputTokens,
				Output:     res.Usage.OutputTokens,
				CacheRead:  res.Usage.CacheReadInputTokens,
				CacheWrite: res.Usage.CacheCreationInputTokens,
			},
			CostUSD: cost,
		})
	}
}

// The run's transcript is never written, so the transcript path records
// nothing: the closed line carries the stream's final result — tokens and
// the agent's own cost, cost_source=recorded (issue #994).
func TestPipeRunFoldsItsStreamUsageWhenTheTranscriptIsAbsent(t *testing.T) {
	e := newRunsEnv(t)
	fakeAgent(t, "claude", `printf '%s\n' '{"type":"system","subtype":"init","session_id":"s-994"}'
printf '%s\n' '{"type":"result","subtype":"success","num_turns":2,"total_cost_usd":0.37,"usage":{"input_tokens":900,"output_tokens":210,"cache_read_input_tokens":10000,"cache_creation_input_tokens":35}}'
`)
	m, _ := e.manager(t, sweepCfg(pipeSweep("oneshot")), fastPolicy())
	acc := ledger.NewAccumulator(m.Ledger(), ledger.AccumulatorOptions{})
	m.SetStreamUsageSink(sinkFrom(t, acc))

	m.StartRun("oneshot", RunRequest{Trigger: TriggerManual})
	waitRuns(t, m, "oneshot", "run ends", outcomesAre(OutcomeSuccess))

	var c map[string]any
	for _, ln := range ledgerLines(t, e) {
		if ln["type"] == "closed" && ln["harness"] == "oneshot" {
			c = ln
		}
	}
	if c == nil {
		t.Fatal("no closed line on disk")
	}
	tok, _ := c["tokens"].(map[string]any)
	if tok["input"] != float64(900) || tok["output"] != float64(210) ||
		tok["cache_read"] != float64(10_000) || tok["cache_write"] != float64(35) {
		t.Errorf("tokens = %v, want the stream's 900/210/10000/35", tok)
	}
	if c["cost_usd"] != 0.37 || c["cost_source"] != "recorded" {
		t.Errorf("cost = %v (%v), want 0.37 recorded", c["cost_usd"], c["cost_source"])
	}
}

// A run whose stream ends without a result record — a killed one-shot —
// folds nothing: the closed line carries no usage fields at all.
func TestPipeRunKilledBeforeItsResultFoldsNothing(t *testing.T) {
	e := newRunsEnv(t)
	fakeAgent(t, "claude", `printf '%s\n' '{"type":"system","subtype":"init","session_id":"s-994"}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"text","text":"half"}]}}'
kill -TERM $$
`)
	m, _ := e.manager(t, sweepCfg(pipeSweep("oneshot")), fastPolicy())
	acc := ledger.NewAccumulator(m.Ledger(), ledger.AccumulatorOptions{})
	m.SetStreamUsageSink(sinkFrom(t, acc))

	m.StartRun("oneshot", RunRequest{Trigger: TriggerManual})
	waitRuns(t, m, "oneshot", "run ends", outcomesAre(OutcomeFailed))

	var c map[string]any
	for _, ln := range ledgerLines(t, e) {
		if ln["type"] == "closed" && ln["harness"] == "oneshot" {
			c = ln
		}
	}
	if c == nil {
		t.Fatal("no closed line on disk")
	}
	for _, k := range []string{"tokens", "cost_usd", "cost_source"} {
		if _, has := c[k]; has {
			t.Errorf("a run with no result record carries %q: %v", k, c[k])
		}
	}
}

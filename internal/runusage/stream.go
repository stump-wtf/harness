package runusage

// The run's own stream
//
// A structured one-shot's usage, when its transcript never flushed, comes
// from the run's own stream file instead (harness#994): ADR-0033's
// <id>.stream.jsonl, sealed before the run's record is closed. Its final
// result record is the agent's own account of the run — turns, duration,
// tokens, the recorded cost — and it folds into the accumulator as a usage
// report with cost_source=recorded, when the transcript path recorded
// nothing for the run. The two sources must never count the same run twice;
// the accumulator's FoldResult is what keeps that true.
//
// Governing: SPEC-0021 REQ-8; SPEC-0022 REQ-8; harness#994.

import (
	"context"
	"errors"
	"io/fs"
	"os"

	"github.com/stump-wtf/agent-trace/tail"

	"github.com/stump-wtf/harness/internal/ledger"
)

// StreamParserFor returns the agent-trace adapter that reads a run's own
// stream file for kind — the trajectory kind the run's process writes — or
// false when no adapter for kind parses a structured stream.
func StreamParserFor(kind string) (tail.StreamParser, bool) {
	for _, a := range tail.DefaultAdapters() {
		if a.Harness() != tail.Harness(kind) {
			continue
		}
		sp, ok := a.(tail.StreamParser)
		return sp, ok
	}
	return nil, false
}

// FoldStream reads the run's own stream file (path) once and folds its final
// result record into acc as the run's usage. It reports whether it folded.
// A file that ends without a result record — a killed or crashed run — and
// a run with no stream file fold nothing, and neither is an error.
func FoldStream(ctx context.Context, acc *ledger.Accumulator, harness string, runID int, path string, parser tail.StreamParser) (bool, error) {
	if parser == nil {
		return false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		// A run with no stream file (a PTY run, or a spawn that never
		// opened one) has nothing to fold; that is not an error.
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	defer f.Close()
	_, res, err := parser.ParseStream(ctx, f, tail.StreamHandler{})
	if err != nil {
		return false, err
	}
	if res == nil {
		return false, nil
	}
	return acc.FoldResult(harness, runID, *resultReport(res)), nil
}

// resultReport is a run's usage as its final result record says it is: the
// four token counts as the ledger keeps them, and the cost the agent
// recorded, nil when the record carries none. The record names no model or
// provider, and those stay "" — the ledger treats them as not recorded.
func resultReport(res *tail.StreamResult) *ledger.UsageReport {
	var cost *float64
	if res.CostUSD > 0 {
		c := res.CostUSD
		cost = &c
	}
	return &ledger.UsageReport{
		Tokens: ledger.Tokens{
			Input:      res.Usage.InputTokens,
			Output:     res.Usage.OutputTokens,
			CacheRead:  res.Usage.CacheReadInputTokens,
			CacheWrite: res.Usage.CacheCreationInputTokens,
		},
		CostUSD: cost,
	}
}

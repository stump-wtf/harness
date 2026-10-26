package runusage

// The run's own stream: tests
//
// FoldStream reads a run's sealed <id>.stream.jsonl once and folds its final
// result into the ledger (harness#994). These tests write real stream files
// — the shape Claude Code's stream-json takes — and a real ledger, and check
// what the run's closed line ends up carrying.

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/tail"

	"github.com/stump-wtf/harness/internal/ledger"
)

func ptime(t time.Time) *time.Time  { return &t }
func pfloat64(f float64) *float64   { return &f }

// openTestLedger opens a ledger under its own day directory.
func openTestLedger(t *testing.T) (string, *ledger.Ledger) {
	t.Helper()
	dir := t.TempDir()
	l, err := ledger.Open(dir, ledger.Options{})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = l.Close(5 * time.Second) })
	return dir, l
}

func openTestRun(t *testing.T, l *ledger.Ledger, h string, id int, at time.Time) {
	t.Helper()
	ln := ledger.Line{
		Type: ledger.TypeOpened, At: at, Harness: h, RunID: id,
		Record: ledger.Record{Kind: "oneshot", Trigger: "schedule", Outcome: "running", StartedAt: ptime(at)},
	}
	if _, err := l.Append(ln, true); err != nil {
		t.Fatalf("open run: %v", err)
	}
}

func closeTestRun(t *testing.T, l *ledger.Ledger, h string, id int, at time.Time, outcome string) {
	t.Helper()
	if _, err := l.Append(ledger.Line{
		Type: ledger.TypeClosed, At: at, Harness: h, RunID: id,
		Record: ledger.Record{Outcome: outcome, EndedAt: ptime(at)},
	}, true); err != nil {
		t.Fatalf("close run: %v", err)
	}
}

// closedRecordOf returns the last closed line of h's run id, as it sits on
// disk (the record's fields are flattened onto the line).
func closedRecordOf(t *testing.T, dir string, h string, id int) map[string]any {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			var ln map[string]any
			if err := json.Unmarshal(sc.Bytes(), &ln); err != nil {
				continue
			}
			if ln["type"] == "closed" && ln["harness"] == h && ln["run_id"] == float64(id) {
				out = ln
			}
		}
		_ = fh.Close()
	}
	if out == nil {
		t.Fatalf("no closed line on disk for %s run %d", h, id)
	}
	return out
}

// writeStreamFile writes lines to dir as <runID>.stream.jsonl.
func writeStreamFile(t *testing.T, dir string, runID int, lines ...string) string {
	t.Helper()
	path := filepath.Join(dir, strconv.Itoa(runID)+".stream.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// streamParser resolves the tail parser the way the daemon's sink does.
func streamParser(t *testing.T, kind string) tail.StreamParser {
	t.Helper()
	sp, ok := StreamParserFor(kind)
	if !ok {
		t.Fatalf("no stream parser for %s", kind)
	}
	return sp
}

// A run whose transcript never flushed: its stream has a final result, and
// that is what the ledger records — tokens and the agent's own cost, with
// cost_source=recorded (harness#994).
func TestFoldStreamFillsTheTranscriptHole(t *testing.T) {
	dir, l := openTestLedger(t)
	a := ledger.NewAccumulator(l, ledger.AccumulatorOptions{})
	now := time.Now()
	openTestRun(t, l, "cc", 1, now.Add(-time.Minute))

	stream := writeStreamFile(t, t.TempDir(), 1,
		`{"type":"system","subtype":"init","session_id":"s-1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"done"}]}}`,
		`{"type":"result","subtype":"success","num_turns":3,"duration_ms":1234,"total_cost_usd":0.25,"usage":{"input_tokens":1200,"output_tokens":300,"cache_creation_input_tokens":500,"cache_read_input_tokens":40000}}`,
	)
	folded, err := FoldStream(context.Background(), a, "cc", 1, stream, streamParser(t, "claude-code"))
	if err != nil || !folded {
		t.Fatalf("FoldStream = %v, %v; want the result folded", folded, err)
	}
	closeTestRun(t, l, "cc", 1, now, "success")

	c := closedRecordOf(t, dir, "cc", 1)
	tok := c["tokens"].(map[string]any)
	if tok["input"] != float64(1200) || tok["output"] != float64(300) ||
		tok["cache_read"] != float64(40_000) || tok["cache_write"] != float64(500) {
		t.Errorf("tokens = %v", tok)
	}
	if c["cost_usd"] != 0.25 || c["cost_source"] != "recorded" {
		t.Errorf("cost = %v (%v), want 0.25 recorded", c["cost_usd"], c["cost_source"])
	}
}

// Both sources present: the transcript path already recorded the run's usage,
// so the stream's result folds nothing on top of it — the run's totals are
// the transcript's and only the transcript's (harness#994).
func TestFoldStreamSkipsWhenTheTranscriptAlreadyRecorded(t *testing.T) {
	_, l := openTestLedger(t)
	a := ledger.NewAccumulator(l, ledger.AccumulatorOptions{})
	now := time.Now()
	openTestRun(t, l, "cc", 1, now.Add(-time.Minute))
	a.Fold(ledger.Item{
		Harness: "cc", At: now,
		Session: ledger.Session{ID: "cc:s1", Adapter: "claude-code"},
		Usage:   &ledger.UsageReport{Tokens: ledger.Tokens{Input: 600, Output: 120}, CostUSD: pfloat64(0.12)},
	})

	stream := writeStreamFile(t, t.TempDir(), 1,
		`{"type":"system","subtype":"init","session_id":"s-1"}`,
		`{"type":"result","subtype":"success","num_turns":3,"total_cost_usd":0.25,"usage":{"input_tokens":1200,"output_tokens":300}}`,
	)
	folded, err := FoldStream(context.Background(), a, "cc", 1, stream, streamParser(t, "claude-code"))
	if err != nil || folded {
		t.Fatalf("FoldStream = %v, %v; want nothing folded on top of the transcript", folded, err)
	}
}

// A killed or crashed run ends without a result record: FoldStream folds
// nothing, and that is not an error.
func TestFoldStreamFoldsNothingWithoutAResult(t *testing.T) {
	_, l := openTestLedger(t)
	a := ledger.NewAccumulator(l, ledger.AccumulatorOptions{})
	now := time.Now()
	openTestRun(t, l, "cc", 1, now.Add(-time.Minute))

	stream := writeStreamFile(t, t.TempDir(), 1,
		`{"type":"system","subtype":"init","session_id":"s-1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"half done"}]}}`,
	)
	folded, err := FoldStream(context.Background(), a, "cc", 1, stream, streamParser(t, "claude-code"))
	if err != nil || folded {
		t.Fatalf("FoldStream = %v, %v; want nothing folded", folded, err)
	}
}

// A run with no stream file at all (a PTY run's path) folds nothing, and the
// missing file is not an error.
func TestFoldStreamMissingFileIsNothing(t *testing.T) {
	_, l := openTestLedger(t)
	a := ledger.NewAccumulator(l, ledger.AccumulatorOptions{})
	now := time.Now()
	openTestRun(t, l, "cc", 1, now.Add(-time.Minute))

	folded, err := FoldStream(context.Background(), a, "cc", 1, filepath.Join(t.TempDir(), "1.stream.jsonl"), streamParser(t, "claude-code"))
	if err != nil || folded {
		t.Fatalf("FoldStream = %v, %v; want nothing folded", folded, err)
	}
}

// StreamParserFor: the adapters that write a structured prompt stream, and
// only those.
func TestStreamParserFor(t *testing.T) {
	if _, ok := StreamParserFor("claude-code"); !ok {
		t.Error("claude-code has no stream parser")
	}
	for _, kind := range []string{"crush", "codex", "pi", "omp", "generic", ""} {
		if _, ok := StreamParserFor(kind); ok {
			t.Errorf("%s has a stream parser, want none", kind)
		}
	}
}

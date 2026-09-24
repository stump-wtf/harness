package ledger

import (
	"io/fs"
	"os"
	"sync"
	"testing"
	"time"
)

// usageClock is a settable clock for checkpoint timing.
type usageClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *usageClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *usageClock) Add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// lastLineOf returns the last line on disk of run id of h with the given type.
func lastLineOf(t *testing.T, dir, h string, id int, typ Type) map[string]any {
	t.Helper()
	var out map[string]any
	for _, ln := range fileLines(t, dir) {
		if ln["harness"] == h && ln["run_id"] == float64(id) && ln["type"] == string(typ) {
			out = ln
		}
	}
	return out
}

// REQ-8: tool calls, error classes and sessions fold into the open run; the
// closed line on disk carries the final totals.
func TestUsageFoldsIntoTheOpenRunAndItsClose(t *testing.T) {
	dir := t.TempDir()
	l := openT(t, dir, Options{})
	start := time.Now().Add(-time.Minute)
	mustAppend(t, l, opened("pr-review", 7, start), true)
	a := NewAccumulator(l, AccumulatorOptions{})
	a.SetTraceURL("https://grafana.example.com/explore?traceId={trace_id}")

	s := Session{ID: "crush:s1", Adapter: "crush", TraceID: "4bf92f3577b34da6a3ce929d0e0e4736"}
	at := start.Add(time.Second)
	for range 3 {
		a.Fold(Item{Harness: "pr-review", At: at, Tool: true, Session: s})
	}
	a.Fold(Item{Harness: "pr-review", At: at, ErrorClass: "quota", Session: s})
	a.Fold(Item{Harness: "pr-review", At: at, ErrorClass: "quota", Session: s})
	a.Fold(Item{Harness: "pr-review", At: at, ErrorClass: "auth", Session: Session{ID: "crush:s2", Adapter: "crush"}})
	// No open run for this harness, and an item from before this run began.
	a.Fold(Item{Harness: "other", At: at, Tool: true})
	a.Fold(Item{Harness: "pr-review", At: start.Add(-time.Hour), Tool: true})

	mustAppend(t, l, closed("pr-review", 7, time.Now(), "success"), true)
	c := lastLineOf(t, dir, "pr-review", 7, TypeClosed)
	if c["model_calls"] != float64(3) {
		t.Errorf("model_calls on disk = %v, want 3", c["model_calls"])
	}
	errs, _ := c["errors"].(map[string]any)
	if errs["quota"] != float64(2) || errs["auth"] != float64(1) {
		t.Errorf("errors on disk = %v, want quota 2, auth 1", c["errors"])
	}
	if ss, _ := c["sessions"].([]any); len(ss) != 2 {
		t.Errorf("sessions on disk = %v, want 2", c["sessions"])
	}
	// REQ-9 "A Grafana link".
	if c["trace_url"] != "https://grafana.example.com/explore?traceId=4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("trace_url = %v", c["trace_url"])
	}
	if c["usage_complete"] != true || c["outcome"] != "success" {
		t.Errorf("close = %v, want usage_complete true and the closing path's own outcome", c)
	}
	if st := a.Stats(); st.Folded != 6 || st.NoRun != 2 {
		t.Errorf("stats = %+v, want 6 folded, 2 with no run", st)
	}
}

// REQ-8 "The observer drops items": a drop seen while run 9 is open leaves it
// usage_complete: false.
func TestObserverDropsMarkTheOpenRunIncomplete(t *testing.T) {
	dir := t.TempDir()
	l := openT(t, dir, Options{})
	mustAppend(t, l, opened("sweep", 9, time.Now()), true)
	a := NewAccumulator(l, AccumulatorOptions{})
	a.Dropped(0) // nothing lost yet
	a.Fold(Item{Harness: "sweep", At: time.Now(), Tool: true})
	a.Dropped(12)
	a.Dropped(12) // the same total again is not a new drop
	mustAppend(t, l, closed("sweep", 9, time.Now(), "success"), true)

	if c := lastLineOf(t, dir, "sweep", 9, TypeClosed); c["usage_complete"] != false {
		t.Errorf("usage_complete on disk = %v, want false", c["usage_complete"])
	}
	if st := a.Stats(); st.Dropped != 12 || st.Incomplete != 1 {
		t.Errorf("stats = %+v, want 12 dropped and 1 run incomplete", st)
	}
}

// Checkpoints: at most one updated line per CheckpointEvery per run, and none
// when nothing changed.
func TestCheckpointsAreRateLimited(t *testing.T) {
	dir := t.TempDir()
	clk := &usageClock{t: time.Now()}
	l := openT(t, dir, Options{})
	mustAppend(t, l, opened("svc", 1, clk.Now()), true)
	a := NewAccumulator(l, AccumulatorOptions{Now: clk.Now})

	a.Fold(Item{Harness: "svc", At: clk.Now(), Tool: true})
	a.Tick() // first change: written
	a.Fold(Item{Harness: "svc", At: clk.Now(), Tool: true})
	clk.Add(10 * time.Second)
	a.Tick() // too soon
	clk.Add(25 * time.Second)
	a.Tick() // 35s after the last: written
	clk.Add(time.Minute)
	a.Tick() // nothing changed: not written
	mustAppend(t, l, closed("svc", 1, clk.Now(), "success"), true)

	var updates []float64
	for _, ln := range fileLines(t, dir) {
		if ln["type"] == "updated" {
			updates = append(updates, ln["model_calls"].(float64))
		}
	}
	if len(updates) != 2 || updates[0] != 1 || updates[1] != 2 {
		t.Errorf("updated lines carry model_calls %v, want [1 2]", updates)
	}
}

// REQ-19: a reload's trace_url template applies to records closed after it.
func TestTraceURLTemplateAppliesAtClose(t *testing.T) {
	dir := t.TempDir()
	l := openT(t, dir, Options{})
	mustAppend(t, l, opened("a", 1, time.Now()), true)
	a := NewAccumulator(l, AccumulatorOptions{})
	a.SetTraceURL("https://old.example/{trace_id}")
	a.Fold(Item{Harness: "a", At: time.Now(), Tool: true, Session: Session{ID: "s", TraceID: "abc"}})
	a.SetTraceURL("https://new.example/t/{trace_id}")
	mustAppend(t, l, closed("a", 1, time.Now(), "success"), true)
	if c := lastLineOf(t, dir, "a", 1, TypeClosed); c["trace_url"] != "https://new.example/t/abc" {
		t.Errorf("trace_url = %v, want the template in force at the close", c["trace_url"])
	}
}

// The accumulator never waits on the writer: with the disk failing, folding a
// thousand items and checkpointing returns at once. Its caller reads the
// observer's subscription, so a stall here would be an observer drop, never an
// observer stall; this is the half that proves there is no stall to cause one.
func TestAccumulatorDoesNotWaitOnAFailingWriter(t *testing.T) {
	var mu sync.Mutex
	failing := false
	realWrite := writeFile
	writeFile = func(f *os.File, b []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		if failing {
			return 0, fs.ErrPermission
		}
		return realWrite(f, b)
	}
	t.Cleanup(func() { writeFile = realWrite })

	dir := t.TempDir()
	l, err := Open(dir, Options{RetryMin: time.Millisecond, RetryMax: 5 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = l.Close(time.Second) }()
	mustAppend(t, l, opened("busy", 1, time.Now()), true)
	a := NewAccumulator(l, AccumulatorOptions{CheckpointEvery: time.Nanosecond})
	mu.Lock()
	failing = true
	mu.Unlock()

	start := time.Now()
	for range 1000 {
		a.Fold(Item{Harness: "busy", At: time.Now(), Tool: true})
		a.Tick()
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("1000 folds and checkpoints against a failing disk took %s", took)
	}
	mu.Lock()
	failing = false
	mu.Unlock()
}

// usageItem is a usage report for harness h's session s.
func usageItem(h, s string, at time.Time, rep UsageReport) Item {
	return Item{Harness: h, At: at, Session: Session{ID: s, Adapter: "crush"}, Usage: &rep}
}

func pfloat(f float64) *float64 { return &f }

// REQ-8 "A crush resident resumed across restarts": totals of 10,000 when run
// 5 starts and 14,000 when it ends give run 5 tokens.input 4,000. The 10,000
// was seen during run 4, so it is run 5's baseline.
func TestCumulativeTotalsAreDifferencedAcrossRuns(t *testing.T) {
	dir := t.TempDir()
	l := openT(t, dir, Options{})
	a := NewAccumulator(l, AccumulatorOptions{})
	t0 := time.Now().Add(-time.Hour)
	crush := func(in, out int64, cost float64) UsageReport {
		return UsageReport{Model: "gpt-5", Provider: "openrouter", Tokens: Tokens{Input: in, Output: out}, CostUSD: pfloat(cost), Cumulative: true}
	}

	mustAppend(t, l, opened("res", 4, t0), true)
	a.Fold(usageItem("res", "s1", t0.Add(time.Minute), crush(6_000, 600, 0.06)))
	a.Fold(usageItem("res", "s1", t0.Add(2*time.Minute), crush(10_000, 1_000, 0.10)))
	mustAppend(t, l, closed("res", 4, t0.Add(3*time.Minute), "failed"), true)

	mustAppend(t, l, opened("res", 5, t0.Add(4*time.Minute)), true)
	a.Fold(usageItem("res", "s1", t0.Add(5*time.Minute), crush(12_000, 1_200, 0.12)))
	a.Fold(usageItem("res", "s1", t0.Add(6*time.Minute), crush(14_000, 1_400, 0.14)))
	mustAppend(t, l, closed("res", 5, t0.Add(7*time.Minute), "success"), true)

	c := lastLineOf(t, dir, "res", 5, TypeClosed)
	tok := c["tokens"].(map[string]any)
	if tok["input"] != float64(4_000) || tok["output"] != float64(400) {
		t.Errorf("run 5 tokens = %v, want input 4000, output 400", tok)
	}
	if cost := c["cost_usd"].(float64); cost < 0.0399 || cost > 0.0401 || c["cost_source"] != "recorded" {
		t.Errorf("run 5 cost = %v (%v), want 0.04 recorded", c["cost_usd"], c["cost_source"])
	}
	if c["model"] != "gpt-5" {
		t.Errorf("model = %v", c["model"])
	}
}

// A lower cumulative total is a new baseline: no negative tokens, ever.
func TestLowerCumulativeTotalRebaselines(t *testing.T) {
	dir := t.TempDir()
	l := openT(t, dir, Options{})
	a := NewAccumulator(l, AccumulatorOptions{})
	now := time.Now()
	mustAppend(t, l, opened("res", 1, now.Add(-time.Minute)), true)
	for _, in := range []int64{5_000, 7_000, 300, 900} {
		a.Fold(usageItem("res", "s1", now, UsageReport{Tokens: Tokens{Input: in}, Cumulative: true}))
	}
	mustAppend(t, l, closed("res", 1, now, "success"), true)
	tok := lastLineOf(t, dir, "res", 1, TypeClosed)["tokens"].(map[string]any)
	// 5,000 is the baseline, +2,000, 300 re-baselines, +600.
	if tok["input"] != float64(2_600) {
		t.Errorf("tokens.input = %v, want 2600 with no negative delta", tok["input"])
	}
}

// Per-message usage sums into the run; `model` is the served model with the
// most output tokens; with no price and no recorded cost the source is
// unknown and there is no cost figure to mistake for zero.
func TestPerMessageUsageSumsAndCostIsUnknownWithoutAPrice(t *testing.T) {
	dir := t.TempDir()
	l := openT(t, dir, Options{})
	var live []LiveTotals
	a := NewAccumulator(l, AccumulatorOptions{OnFold: func(lt LiveTotals) { live = append(live, lt) }})
	now := time.Now()
	mustAppend(t, l, opened("cc", 1, now.Add(-time.Minute)), true)
	msg := func(model string, in, out int64) UsageReport {
		return UsageReport{Model: model, Tokens: Tokens{Input: in, Output: out, CacheRead: 10}}
	}
	a.Fold(usageItem("cc", "s", now, msg("claude-haiku-4-5", 100, 10)))
	a.Fold(usageItem("cc", "s", now, msg("claude-sonnet-5", 200, 50)))
	a.Fold(usageItem("cc", "s", now, msg("claude-sonnet-5", 300, 60)))
	mustAppend(t, l, closed("cc", 1, now, "success"), true)

	c := lastLineOf(t, dir, "cc", 1, TypeClosed)
	tok := c["tokens"].(map[string]any)
	if tok["input"] != float64(600) || tok["output"] != float64(120) || tok["cache_read"] != float64(30) {
		t.Errorf("tokens = %v", tok)
	}
	if c["model"] != "claude-sonnet-5" || len(c["models"].([]any)) != 2 {
		t.Errorf("model = %v, models = %v", c["model"], c["models"])
	}
	if c["cost_source"] != "unknown" || c["cost_usd"] != nil {
		t.Errorf("cost = %v (%v), want no figure and source unknown", c["cost_usd"], c["cost_source"])
	}
	if len(live) != 3 || live[2].Tokens.Input != 600 {
		t.Errorf("live totals = %+v, want one per fold ending at 600 input", live)
	}
}

// SPEC-0021 REQ-8: priced when a price exists; the run's source is the weakest
// of its items'.
func TestCostSourceIsTheWeakestItem(t *testing.T) {
	dir := t.TempDir()
	l := openT(t, dir, Options{})
	price := func(model, _ string, tk Tokens) (float64, bool) {
		if model != "priced-model" {
			return 0, false
		}
		return float64(tk.Input) / 1e6, true
	}
	a := NewAccumulator(l, AccumulatorOptions{Price: price})
	now := time.Now()
	mustAppend(t, l, opened("x", 1, now.Add(-time.Minute)), true)
	a.Fold(usageItem("x", "s", now, UsageReport{Model: "recorded-model", Tokens: Tokens{Input: 1}, CostUSD: pfloat(0.5)}))
	a.Fold(usageItem("x", "s", now, UsageReport{Model: "priced-model", Tokens: Tokens{Input: 1_000_000}}))
	mustAppend(t, l, closed("x", 1, now, "success"), true)
	c := lastLineOf(t, dir, "x", 1, TypeClosed)
	if c["cost_source"] != "priced" || c["cost_usd"] != float64(1.5) {
		t.Errorf("cost = %v (%v), want 1.5 priced", c["cost_usd"], c["cost_source"])
	}
}

// Before agent-trace#105 no item carries usage, and a run's record must not
// grow tokens, cost or models fields: absent, not zero.
func TestNoUsageItemsMeansNoUsageFields(t *testing.T) {
	dir := t.TempDir()
	l := openT(t, dir, Options{})
	a := NewAccumulator(l, AccumulatorOptions{})
	now := time.Now()
	mustAppend(t, l, opened("x", 1, now.Add(-time.Minute)), true)
	a.Fold(Item{Harness: "x", At: now, Tool: true})
	mustAppend(t, l, closed("x", 1, now, "success"), true)
	c := lastLineOf(t, dir, "x", 1, TypeClosed)
	for _, k := range []string{"tokens", "cost_usd", "cost_source", "models", "model"} {
		if _, has := c[k]; has {
			t.Errorf("a run with no usage items carries %q: %v", k, c[k])
		}
	}
}

// A cumulative total reported while no run is open (between a resident's
// runs) is nobody's spend, but it moves the baseline: the next run counts
// from it.
func TestCumulativeTotalBetweenRunsMovesTheBaseline(t *testing.T) {
	dir := t.TempDir()
	l := openT(t, dir, Options{})
	a := NewAccumulator(l, AccumulatorOptions{})
	t0 := time.Now().Add(-time.Hour)
	tot := func(in int64) UsageReport { return UsageReport{Tokens: Tokens{Input: in}, Cumulative: true} }

	mustAppend(t, l, opened("res", 1, t0), true)
	a.Fold(usageItem("res", "s1", t0.Add(time.Minute), tot(10_000)))
	mustAppend(t, l, closed("res", 1, t0.Add(2*time.Minute), "success"), true)
	a.Fold(usageItem("res", "s1", t0.Add(3*time.Minute), tot(11_000))) // no run open
	mustAppend(t, l, opened("res", 2, t0.Add(4*time.Minute)), true)
	a.Fold(usageItem("res", "s1", t0.Add(5*time.Minute), tot(14_000)))
	mustAppend(t, l, closed("res", 2, t0.Add(6*time.Minute), "success"), true)

	if tok := lastLineOf(t, dir, "res", 2, TypeClosed)["tokens"].(map[string]any); tok["input"] != float64(3_000) {
		t.Errorf("run 2 tokens.input = %v, want 3000: the 1,000 spent between runs is not run 2's", tok["input"])
	}
}

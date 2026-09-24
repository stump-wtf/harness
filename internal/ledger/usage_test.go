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

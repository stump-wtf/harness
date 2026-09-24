package runusage

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/otel"
	"github.com/stump-wtf/agent-trace/tail"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/ledger"
	"github.com/stump-wtf/harness/internal/metrics"
	"github.com/stump-wtf/harness/internal/observe"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// fanout is an observer stand-in with the real one's contract: every
// subscriber gets every event, without blocking, and a full buffer counts a
// drop against that subscriber's name.
type fanout struct {
	mu    sync.Mutex
	subs  map[string]chan observe.Event
	stats observe.Stats
}

func newFanout() *fanout {
	return &fanout{subs: map[string]chan observe.Event{}, stats: observe.Stats{Dropped: map[string]uint64{}}}
}

func (f *fanout) Subscribe(name string, buf int) (<-chan observe.Event, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	ch := make(chan observe.Event, buf)
	f.subs[name] = ch
	var once sync.Once
	return ch, func() {
		once.Do(func() {
			f.mu.Lock()
			delete(f.subs, name)
			f.mu.Unlock()
			close(ch)
		})
	}
}

func (f *fanout) publish(ev observe.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, ch := range f.subs {
		select {
		case ch <- ev:
		default:
			f.stats.Dropped[name]++
		}
	}
}

func (f *fanout) addDrops(name string, n uint64) {
	f.mu.Lock()
	f.stats.Dropped[name] += n
	f.mu.Unlock()
}

func (f *fanout) Stats() observe.Stats {
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.stats
	st.Dropped = map[string]uint64{}
	for k, v := range f.stats.Dropped {
		st.Dropped[k] = v
	}
	return st
}

// rig is a real Manager running one observable crush harness (a stub crush on
// PATH), with a real ledger.
type rig struct {
	dir string
	mgr *supervisor.Manager
	acc *ledger.Accumulator
}

func newRig(t *testing.T) *rig {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "crush"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(work, 0o700); err != nil {
		t.Fatal(err)
	}
	h := core.Harness{Name: "worker", Adapter: "crush", Workdir: work, Backend: core.BackendNative, Restart: core.RestartNo}
	cfg := &core.Config{Harnesses: map[string]core.Harness{"worker": h}, HarnessOrder: []string{"worker"}, Profiles: map[string]core.Profile{}}
	m := supervisor.NewManager(cfg, supervisor.ManagerOptions{
		Policy:    supervisor.Policy{StopGrace: 200 * time.Millisecond},
		StatePath: filepath.Join(dir, "state.json"),
		LogDir:    filepath.Join(dir, "logs"),
	})
	t.Cleanup(m.Close)
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	m.Start("worker")
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, ok := m.Ledger().OpenRun("worker"); ok {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the worker's run never opened")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return &rig{dir: dir, mgr: m, acc: ledger.NewAccumulator(m.Ledger(), ledger.AccumulatorOptions{})}
}

// closedLine stops the worker and returns its run's closed line from disk.
func (r *rig) closedLine(t *testing.T) map[string]any {
	t.Helper()
	r.mgr.Stop("worker")
	files, _ := filepath.Glob(filepath.Join(r.dir, "ledger", "*.jsonl"))
	var out map[string]any
	for _, f := range files {
		b, _ := os.ReadFile(f)
		for _, ln := range strings.Split(string(b), "\n") {
			var m map[string]any
			if json.Unmarshal([]byte(ln), &m) == nil && m["harness"] == "worker" && m["type"] == "closed" {
				out = m
			}
		}
	}
	if out == nil {
		t.Fatal("no closed line on disk for the worker")
	}
	return out
}

func errorEvent(note string, at time.Time) observe.Event {
	ev := observe.Event{Harness: "worker", Adapter: "crush", Kind: observe.KindMark, Time: at, ObservedAt: at}
	ev.Session.Key, ev.Session.ID = "crush:s1", "s1"
	ev.Mark.Type, ev.Mark.Note = "error", note
	return ev
}

// An error mark classified quota lands in the run's errors.quota, and the
// same fixture, through the same observer fan-out, gives SPEC-0013's
// harness_model_call_errors_total{class="quota"} the same count.
func TestQuotaErrorsAgreeWithTheMetricsCollector(t *testing.T) {
	r := newRig(t)
	obs := newFanout()
	met := metrics.New(r.mgr, metrics.Options{Observer: obs})
	met.Start()
	t.Cleanup(met.Close)
	feed := Start(obs, r.acc, Options{Tick: 10 * time.Millisecond})
	t.Cleanup(feed.Stop)

	at := time.Now()
	for i := range 3 {
		obs.publish(errorEvent("429 Too Many Requests", at.Add(time.Duration(i)*time.Millisecond)))
	}
	obs.publish(errorEvent("401 Unauthorized", at))

	var fromMetrics float64
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		fromMetrics = scrapeQuota(t, met)
		if fromMetrics == 3 && r.acc.Stats().Folded == 4 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	errs, _ := r.closedLine(t)["errors"].(map[string]any)
	if fromMetrics != 3 || errs["quota"] != float64(3) {
		t.Errorf("metrics counted %v quota errors, the run record %v: want 3 and 3", fromMetrics, errs["quota"])
	}
	if errs["auth"] != float64(1) {
		t.Errorf("errors = %v, want auth 1", errs)
	}
}

func scrapeQuota(t *testing.T, m *metrics.Metrics) float64 {
	t.Helper()
	rec := httptest.NewRecorder()
	m.Handler().ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	parser := expfmt.NewTextParser(model.UTF8Validation)
	fams, err := parser.TextToMetricFamilies(rec.Body)
	if err != nil {
		t.Fatal(err)
	}
	fam := fams["harness_model_call_errors_total"]
	if fam == nil {
		return 0
	}
	for _, mt := range fam.GetMetric() {
		labels := map[string]string{}
		for _, lp := range mt.GetLabel() {
			labels[lp.GetName()] = lp.GetValue()
		}
		if labels["harness"] == "worker" && labels["class"] == "quota" {
			return mt.GetCounter().GetValue()
		}
	}
	return 0
}

// REQ-8 "The observer drops items", through the feed: the observer's drop
// count for this subscription marks the open run incomplete.
func TestObserverDropsReachTheRunRecord(t *testing.T) {
	r := newRig(t)
	obs := newFanout()
	feed := Start(obs, r.acc, Options{Tick: 10 * time.Millisecond})
	t.Cleanup(feed.Stop)
	obs.publish(observe.Event{Harness: "worker", Adapter: "crush", Kind: observe.KindTool, Time: time.Now()})
	obs.addDrops(SubscriberName, 7)
	deadline := time.Now().Add(5 * time.Second)
	for r.acc.Stats().Incomplete == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if c := r.closedLine(t); c["usage_complete"] != false || c["model_calls"] != float64(1) {
		t.Errorf("close = %v, want usage_complete false and the one call", c)
	}
}

// TraceID is agent-trace's: pinned to BuildTrace's output, so a change in the
// library's derivation fails here rather than breaking every trace link.
func TestTraceIDMatchesAgentTrace(t *testing.T) {
	s := tail.SessionMeta{Key: "crush:abc123", ID: "abc123"}
	tr := otel.BuildTrace(s, []classify.Event{{Seq: 1, Tool: "view", Timestamp: time.Now().UTC().Format(time.RFC3339)}}, nil)
	b, err := json.Marshal(tr)
	if err != nil {
		t.Fatal(err)
	}
	want := TraceID(s)
	if !strings.Contains(string(b), `"traceId":"`+want+`"`) {
		t.Errorf("TraceID %s does not match agent-trace's trace: %s", want, b)
	}
}

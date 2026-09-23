package metrics

// The 2026-09-14 Outage, As It Would Have Appeared
//
// Governing tests: SPEC-0013 Scenario "the 2026-09-14 outage"; design.md
// "Testing" (the exact series an alert would read); ADR-0020's alert.
//
// This drives the real path end to end: a real crush store written by the
// runtracetest fixtures, read by agent-trace's real crush adapter inside the
// real observer, delivered to Metrics over a real subscription, and read back
// through the HTTP handler a scraper uses. Only the Manager is a stand-in
// (cmd/harness covers the real one). A test with a hand-fed event would pass
// against an observer that never delivers error marks on their own — which is
// precisely what agent-trace's own Watcher does during an outage, since it
// parks marks until a tool call arrives to carry them.
//
// @joestump-agent 09/21/2026 - Added for harness#356.

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/observe"
	rt "github.com/stump-wtf/harness/internal/runtrace/runtracetest"
	"github.com/stump-wtf/harness/internal/supervisor"
)

func TestOutageShapeThroughTheRealObserver(t *testing.T) {
	// A whole second, so the second-resolution timestamps crush stores
	// compare exactly.
	start := time.Date(2026, 9, 14, 3, 0, 0, 0, time.UTC)
	home := t.TempDir()
	work := filepath.Join(home, "work")
	db := filepath.Join(work, ".crush", "crush.db")
	c := &clock{t: start}

	src := newFakeSource()
	src.add(core.Harness{Name: "worker", Adapter: "crush", Workdir: work},
		supervisor.Snapshot{State: core.StateRunning, Enabled: true, PID: 4242, LastStarted: start.Add(-3 * time.Hour)})

	// A long-lived worker resumed into a session it opened hours before this
	// daemon started (crush keeps one session across restarts, #347).
	rt.WriteCrushDB(t, db, rt.CrushSession{
		ID: "sess-1", Created: start.Add(-2 * time.Hour), Updated: start.Add(-30 * time.Minute),
		Messages: []rt.CrushMessage{{Role: "user", At: start.Add(-30 * time.Minute), Parts: `[{"type":"text","data":{"text":"drain the queue"}}]`}},
	})

	obs := observe.New(src, observe.Options{
		PollInterval: 10 * time.Millisecond,
		Now:          c.Now,
		Since:        start,
		DaemonDir:    home,
		DiscoveryEnv: func(core.Harness) map[string]string { return map[string]string{"HOME": home} },
	})
	m := New(src, Options{Observer: obs, Now: c.Now})
	m.Start()
	obs.Start()
	t.Cleanup(func() {
		m.Close()
		obs.Stop()
	})
	// The first scan establishes the history floor; the session's old
	// content is never reported as live.
	eventually(t, "the observer's first scan", func() bool { return !obs.Stats().LastScan.IsZero() })

	series := func(name string, kv ...string) (float64, bool) {
		return scrape(t, m).get(name, lbls(append([]string{"harness", "worker"}, kv...)...))
	}

	// Before the outage: the worker does one piece of work.
	worked := start.Add(10 * time.Second)
	c.Set(worked)
	rt.AppendCrushMessages(t, db, "sess-1",
		rt.CrushMessage{Role: "assistant", At: worked, Parts: rt.ToolCall("c1", "view", map[string]any{"file_path": "README.md"})},
		rt.CrushMessage{Role: "tool", At: worked, Parts: rt.ToolResult("c1", "contents")},
	)
	eventually(t, "the tool call to count as a success", func() bool {
		v, _ := series("harness_model_calls_total", "outcome", "success")
		return v == 1
	})
	if v, _ := series("harness_last_successful_call_timestamp"); v != float64(worked.Unix()) {
		t.Fatalf("last success = %v, want %v", v, worked.Unix())
	}

	// The weekly quota empties. Every call fails; no tool call is ever
	// written again; the process stays up.
	for i := 1; i <= 3; i++ {
		at := start.Add(time.Duration(10+20*i) * time.Second)
		c.Set(at)
		rt.AppendCrushMessages(t, db, "sess-1",
			rt.CrushMessage{Role: "user", At: at, Parts: fmt.Sprintf(`[{"type":"text","data":{"text":"doorbell %d"}}]`, i)},
			rt.CrushMessage{Role: "assistant", At: at, Parts: rt.FinishError("Too Many Requests",
				`{"type":"error","error":{"type":"rate_limit_error","message":"You have reached your weekly usage limit."}}`)},
		)
		eventually(t, fmt.Sprintf("quota error %d to count", i), func() bool {
			v, _ := series("harness_model_call_errors_total", "class", "quota")
			return v == float64(i)
		})
		fams := scrape(t, m)
		// Climbing, while running stays 1 and the timestamp stays frozen.
		if v := fams.must(t, "harness_harness_state", lbls("harness", "worker", "state", "running")); v != 1 {
			t.Errorf("error %d: running = %v; the process is up and must say so", i, v)
		}
		if v := fams.must(t, "harness_last_successful_call_timestamp", lbls("harness", "worker")); v != float64(worked.Unix()) {
			t.Errorf("error %d: last success moved to %v; nothing succeeded", i, v)
		}
		if v := fams.must(t, "harness_model_calls_total", lbls("harness", "worker", "outcome", "error")); v != float64(i) {
			t.Errorf("error %d: error calls = %v", i, v)
		}
	}
	fams := scrape(t, m)
	if v := fams.must(t, "harness_model_calls_total", lbls("harness", "worker", "outcome", "success")); v != 1 {
		t.Errorf("success calls = %v, want still 1", v)
	}
	if v := fams.must(t, "harness_model_call_errors_unclassified_total", lbls("harness", "worker")); v != 0 {
		t.Errorf("unclassified = %v; a provider's quota error must be recognised", v)
	}

	// Twenty minutes in: the ADR's alert, evaluated on the scraped values.
	//   time() - harness_last_successful_call_timestamp > 900
	//     and on(instance, harness) harness_harness_state{state="running"} == 1
	now := start.Add(20 * time.Minute)
	c.Set(now)
	fams = scrape(t, m)
	last := fams.must(t, "harness_last_successful_call_timestamp", lbls("harness", "worker"))
	running := fams.must(t, "harness_harness_state", lbls("harness", "worker", "state", "running"))
	if stale := float64(now.Unix()) - last; !(stale > 900 && running == 1) {
		t.Errorf("alert would not fire: staleness %.0fs, running %v", stale, running)
	}
}

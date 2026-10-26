package supervisor

// A Pipe Run's Live Stream Seam — Tests
//
// These drive a real Manager and a fake `claude` first on PATH, exactly as
// pipes_test.go does, so the seam is exercised on the real spawn path: the
// lines a run hands its consumer are the masked lines it writes to its own
// stream file, in order, and the feed is closed with the run. A PTY run has
// no stream to hand, and a daemon that installed no seam moves lines exactly
// as it did before.
//
// Governing: issue #995; ADR-0033 "Structured one-shots run on pipes", "One
// activity stream per session"; ADR-0008; ADR-0007.
//
// @joestump-agent 10/26/2026 - Added with stream-fed ingest (harness#995).

import (
	"fmt"
	"strings"
	"sync"
	"testing"
)

// recorder is a Manager's stream-ingest seam under test: it records what each
// run hands it, and how often it was closed.
type recorder struct {
	mu    sync.Mutex
	feeds map[string]*recordedFeed
}

type recordedFeed struct {
	owner   *recorder
	harness string
	runID   int
	lines   []string
	closes  int
}

func (r *recorder) sink(harness string, runID int) StreamSink {
	key := fmt.Sprintf("%s/%d", harness, runID)
	r.mu.Lock()
	defer r.mu.Unlock()
	f := &recordedFeed{owner: r, harness: harness, runID: runID}
	r.feeds[key] = f
	return f
}

func (f *recordedFeed) Line(line string) {
	f.owner.mu.Lock()
	defer f.owner.mu.Unlock()
	f.lines = append(f.lines, line)
}

func (f *recordedFeed) Close() {
	f.owner.mu.Lock()
	defer f.owner.mu.Unlock()
	f.closes++
}

func (r *recorder) feed(harness string, runID int) *recordedFeed {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.feeds[fmt.Sprintf("%s/%d", harness, runID)]
}

func (r *recorder) opened() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.feeds))
	for k := range r.feeds {
		out = append(out, k)
	}
	return out
}

// streamIngestScript is a fake claude whose stream carries one credential.
const streamIngestScript = `printf '%s\n' '{"type":"system","subtype":"init","session_id":"s-live"}'
printf '%s\n' '{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"curl -H \"Authorization: token abc123\" https://x"}}]}}'
printf '%s\n' '{"type":"result","subtype":"success","total_cost_usd":0.25}'
`

// A pipe run hands its consumer the same masked lines it writes to its stream
// file, in order, and the feed is closed when the run ends — so a consumer
// watching a live run sees exactly the record the run leaves behind, and
// nothing unmasked (issue #995; ADR-0008).
func TestPipeRunHandsItsStreamToTheSeam(t *testing.T) {
	e := newRunsEnv(t)
	fakeAgent(t, "claude", streamIngestScript)
	m, _ := e.manager(t, sweepCfg(pipeSweep("oneshot")), fastPolicy())
	rec := &recorder{feeds: map[string]*recordedFeed{}}
	m.SetStreamIngestSink(rec.sink)

	m.StartRun("oneshot", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "oneshot", "run ends", outcomesAre(OutcomeSuccess))

	f := rec.feed("oneshot", 1)
	if f == nil {
		t.Fatalf("no run handed its stream; feeds opened: %v", rec.opened())
	}
	if f.closes == 0 {
		t.Error("the run's feed was never closed")
	}
	got := strings.Join(f.lines, "\n") + "\n"
	want := readText(t, StreamPathFor(m.RunLogPath("oneshot", 1)))
	if got != want {
		t.Errorf("the run handed %d lines, its file holds %d; handed head %q",
			len(f.lines), strings.Count(want, "\n"), head(got, 200))
	}
	if strings.Contains(got, "abc123") {
		t.Error("the handed stream carries an unmasked credential")
	}
	if !strings.Contains(got, "[REDACTED]") || !strings.Contains(got, `"session_id":"s-live"`) {
		t.Errorf("the handed stream is not the run's stream: %q", head(got, 300))
	}
}

// A PTY-supervised run has no stream to hand: the seam is not opened for it.
func TestPTYRunOpensNoStreamIngest(t *testing.T) {
	e := newRunsEnv(t)
	fakeAgent(t, "crush", "echo hello\n")
	h := sweep("worker", "")
	h.Adapter, h.Args = "crush", []string{"run", "hi"}
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())
	rec := &recorder{feeds: map[string]*recordedFeed{}}
	m.SetStreamIngestSink(rec.sink)

	m.StartRun("worker", RunRequest{Trigger: TriggerManual})
	waitRuns(t, m, "worker", "run ends", outcomesAre(OutcomeSuccess))
	if got := rec.opened(); len(got) != 0 {
		t.Errorf("a PTY run opened a stream feed: %v", got)
	}
}

// With no seam installed — a test's Manager, or a daemon that built no
// observer — a pipe run writes its stream file and nothing else changes.
func TestPipeRunWithNoSeamInstalledIsUnchanged(t *testing.T) {
	e := newRunsEnv(t)
	fakeAgent(t, "claude", streamIngestScript)
	m, _ := e.manager(t, sweepCfg(pipeSweep("oneshot")), fastPolicy())

	m.StartRun("oneshot", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "oneshot", "run ends", outcomesAre(OutcomeSuccess))

	got := readText(t, StreamPathFor(m.RunLogPath("oneshot", 1)))
	if !strings.Contains(got, `"session_id":"s-live"`) {
		t.Errorf("the stream file is not the run's stream: %q", head(got, 200))
	}
}

func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

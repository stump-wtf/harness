package telemetry

// Pipeline Tests
//
// Governing tests: ADR-0022; SPEC-0015 REQ-1 (consent), REQ-4 (redaction on
// the wire), REQ-5/REQ-6 (the log record), REQ-8 (events file), REQ-9
// (bounded, never blocking), REQ-10 (retry policy), REQ-11 (shutdown),
// REQ-12 (stats). Each asserts on what reached the httptest receiver or the
// file, so a test fails if the property silently did not happen.
//
// @joestump-agent 09/21/2026 - Added for harness#391.

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/otel"

	"github.com/stump-wtf/harness/internal/core"
)

// plantedToken is a GitHub-token-shaped credential, assembled at run time so
// no source line holds one whole (the repository's secret scan would object).
var plantedToken = "gh" + "p_" + strings.Repeat("abcdefghij", 3) + "0123"

func startPipeline(t *testing.T, res *Resolved, src *fakeSource, opts Options) (*Pipeline, *fakeObserver) {
	t.Helper()
	obs := newFakeObserver()
	if opts.Logger == nil {
		opts.Logger = quietLogger()
	}
	p := New(res, obs, src, opts)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		p.Shutdown(ctx)
	})
	return p, obs
}

func TestLogsReachTheCollectorDecoded(t *testing.T) {
	rx := newReceiver(t)
	res := testResolved(rx.srv.URL, func(c *core.TelemetryConfig) { c.Traces = false })
	_, obs := startPipeline(t, res, newFakeSource(optIn("worker")), Options{})

	obs.publish(
		markEv("worker", "k1", 0, "user-message", "fix the build", t0),
		toolEv("worker", "k1", 0, "go test ./...", true, t0.Add(time.Second)),
		toolEv("worker", "k1", 1, "read main.go", false, t0.Add(2*time.Second)),
		markEv("worker", "k1", 2, "error", "429 quota exhausted", t0.Add(3*time.Second)),
	)
	waitFor(t, "4 log records", func() bool { return len(rx.snapshotLogs()) == 4 })
	logs := rx.snapshotLogs()

	rx.mu.Lock()
	resource, scope, path := rx.resource, rx.scope, rx.requests[0].URL.Path
	rx.mu.Unlock()
	if path != "/v1/logs" {
		t.Fatalf("path %s", path)
	}
	if attrOf(resource, "service.name") != "harness" || attrOf(resource, "service.version") != "v9.9.9" || attrOf(resource, "host.name") != "box1" {
		t.Fatalf("resource %v", resource)
	}
	if scope["name"] != ScopeName || scope["version"] != "v9.9.9" {
		t.Fatalf("scope %v", scope)
	}

	wantSev := []struct {
		text string
		num  int
		body string
	}{{"INFO", 9, "fix the build"}, {"WARN", 13, "go test ./..."}, {"INFO", 9, "read main.go"}, {"ERROR", 17, "429 quota exhausted"}}
	traceID := otel.BuildTrace(session("k1"), nil, nil).TraceID
	for i, l := range logs {
		if l.SeverityText != wantSev[i].text || l.SeverityNumber != wantSev[i].num || l.body() != wantSev[i].body {
			t.Errorf("record %d: %s/%d %q, want %+v", i, l.SeverityText, l.SeverityNumber, l.body(), wantSev[i])
		}
		if l.TraceID != traceID {
			t.Errorf("record %d traceId %s, want BuildTrace's %s", i, l.TraceID, traceID)
		}
		if l.attr("harness.name") != "worker" || l.attr("agent.adapter") != "crush" || l.attr("agent.session.id") != "sess-k1" {
			t.Errorf("record %d identity attrs %v", i, l.Attributes)
		}
		if id, _ := l.attr("agent.item.id").(string); len(id) != 16 {
			t.Errorf("record %d agent.item.id %v", i, l.attr("agent.item.id"))
		}
	}
	// Error marks map to no span; everything else here does.
	if logs[3].SpanID != "" {
		t.Errorf("error mark carries spanId %s", logs[3].SpanID)
	}
	if logs[1].SpanID == "" || logs[1].SpanID != logs[1].attr("agent.item.id") {
		t.Errorf("tool record spanId %q vs item id %v", logs[1].SpanID, logs[1].attr("agent.item.id"))
	}
	tool := logs[1]
	if tool.attr("agent.tool.name") != "bash" || tool.attr("agent.tool.action") != "exec" || tool.attr("agent.tool.is_error") != true ||
		tool.attr("agent.result.bytes") != "42" || tool.attr("agent.item.kind") != "tool" || tool.attr("agent.item.seq") != "0" {
		t.Errorf("tool attrs %v", tool.Attributes)
	}
	if got, _ := tool.attr("agent.targets").([]string); len(got) != 1 || got[0] != "main.go" {
		t.Errorf("targets %v", tool.attr("agent.targets"))
	}
	if logs[3].attr("agent.mark.type") != "error" || logs[0].attr("agent.session.title") != "fix the build" {
		t.Errorf("mark attrs %v / %v", logs[3].Attributes, logs[0].Attributes)
	}
}

// The outage shape (ADR-0020's twenty green hours): error marks and no tool
// calls. They must arrive as ERROR records on their own, without waiting for a
// tool event to carry them.
func TestOutageErrorMarksOnlyProduceErrorRecords(t *testing.T) {
	rx := newReceiver(t)
	res := testResolved(rx.srv.URL, func(c *core.TelemetryConfig) { c.Traces = false })
	_, obs := startPipeline(t, res, newFakeSource(optIn("a"), optIn("b")), Options{})
	for i := range 3 {
		obs.publish(markEv("a", "ka", i, "error", "provider quota exhausted", t0.Add(time.Duration(i)*time.Second)))
		obs.publish(markEv("b", "kb", i, "error", "provider quota exhausted", t0.Add(time.Duration(i)*time.Second)))
	}
	waitFor(t, "6 error records", func() bool { return len(rx.snapshotLogs()) == 6 })
	for _, l := range rx.snapshotLogs() {
		if l.SeverityText != "ERROR" || l.SeverityNumber != 17 {
			t.Fatalf("outage record %s/%d", l.SeverityText, l.SeverityNumber)
		}
	}
}

func TestRedactionReachesTheWireAndTheFile(t *testing.T) {
	rx := newReceiver(t)
	file := filepath.Join(t.TempDir(), "state", "events.jsonl")
	res := testResolved(rx.srv.URL, func(c *core.TelemetryConfig) {
		c.EventsFile = file
		c.IdleFlush = 50 * time.Millisecond
		c.BatchInterval = 20 * time.Millisecond
	})
	_, obs := startPipeline(t, res, newFakeSource(optIn("worker")), Options{})

	// The observer redacts too; this proves the pipeline does not depend on
	// it, since these strings skip the observer entirely.
	cmd := `curl -H "Authorization: token ` + plantedToken + `" https://api.example.com`
	obs.publish(
		markEv("worker", "k1", 0, "user-message", "use "+plantedToken+" please", t0),
		toolEv("worker", "k1", 0, cmd, true, t0.Add(time.Second)),
		markEv("worker", "k1", 1, "error", "bad credential "+plantedToken, t0.Add(2*time.Second)),
	)
	waitFor(t, "logs and spans", func() bool { return len(rx.snapshotLogs()) == 3 && len(rx.snapshotSpans()) == 2 })
	waitFor(t, "events file lines", func() bool { return countLines(file) == 3 })

	wire := rx.allRaw()
	data, _ := os.ReadFile(file)
	for name, got := range map[string]string{"OTLP requests": wire, "events file": string(data)} {
		if strings.Contains(got, plantedToken) {
			t.Errorf("%s carry the planted credential", name)
		}
		if !strings.Contains(got, "[REDACTED]") {
			t.Errorf("%s show no redaction at all — was the credential ever there?", name)
		}
	}
	for _, sp := range rx.snapshotSpans() {
		if strings.Contains(sp.Name, plantedToken) || strings.Contains(sp.Status.Message, plantedToken) {
			t.Errorf("span %q leaks", sp.Name)
		}
	}
}

func countLines(path string) int {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return strings.Count(string(data), "\n")
}

func TestGateDiscardsNonContributingHarnesses(t *testing.T) {
	rx := newReceiver(t)
	file := filepath.Join(t.TempDir(), "events.jsonl")
	no := false
	res := testResolved(rx.srv.URL, func(c *core.TelemetryConfig) { c.Traces = false; c.EventsFile = file; c.ExportAll = true })
	src := newFakeSource(
		core.Harness{Name: "fleet"},                             // unset: follows export_all
		core.Harness{Name: "payroll-bot", ExportTelemetry: &no}, // false beats export_all
	)
	p, obs := startPipeline(t, res, src, Options{})
	obs.publish(
		toolEv("payroll-bot", "kp", 0, "SECRET-PAYROLL", false, t0),
		toolEv("unknown", "ku", 0, "UNKNOWN-HARNESS", false, t0),
		toolEv("fleet", "kf", 0, "fleet-item", false, t0),
	)
	waitFor(t, "fleet record", func() bool { return len(rx.snapshotLogs()) == 1 && countLines(file) == 1 })

	// A reload flips payroll-bot to follow export_all: the next item goes.
	src.set(core.Harness{Name: "payroll-bot"})
	obs.publish(toolEv("payroll-bot", "kp", 1, "after-reload", false, t0))
	waitFor(t, "post-reload record", func() bool { return len(rx.snapshotLogs()) == 2 && countLines(file) == 2 })

	data, _ := os.ReadFile(file)
	for _, where := range []string{rx.allRaw(), string(data)} {
		if strings.Contains(where, "SECRET-PAYROLL") || strings.Contains(where, "UNKNOWN-HARNESS") {
			t.Fatal("a non-contributing harness's item was exported")
		}
	}
	for name, st := range p.Stats() {
		if st.DroppedQueue != 0 || st.Failed != 0 {
			t.Errorf("%s counted gated items as losses: %+v", name, st)
		}
	}
}

func TestTracesAndLogsCorrelate(t *testing.T) {
	rx := newReceiver(t)
	res := testResolved(rx.srv.URL, func(c *core.TelemetryConfig) { c.IdleFlush = 30 * time.Millisecond })
	_, obs := startPipeline(t, res, newFakeSource(optIn("w")), Options{})
	obs.publish(
		markEv("w", "k", 0, "user-message", "turn one", t0),
		toolEv("w", "k", 0, "ls", false, t0.Add(time.Second)),
		toolEv("w", "k", 1, "cat x", false, t0.Add(2*time.Second)),
	)
	waitFor(t, "3 spans and 3 logs", func() bool { return len(rx.snapshotSpans()) == 3 && len(rx.snapshotLogs()) == 3 })
	spans := map[string]wireSpan{}
	for _, sp := range rx.snapshotSpans() {
		spans[sp.SpanID] = sp
	}
	var turnID string
	for _, l := range rx.snapshotLogs() {
		sp, ok := spans[l.SpanID]
		if !ok {
			t.Fatalf("log spanId %s matches no exported span", l.SpanID)
		}
		if sp.TraceID != l.TraceID {
			t.Fatalf("trace ids differ: span %s log %s", sp.TraceID, l.TraceID)
		}
		if l.attr("agent.mark.type") == "user-message" {
			turnID = l.SpanID
		}
	}
	for _, sp := range spans {
		if sp.SpanID != turnID && sp.ParentSpanID != turnID {
			t.Errorf("tool span %q parent %q, want the turn %q", sp.Name, sp.ParentSpanID, turnID)
		}
		if sp.attr("harness.name") != "w" {
			t.Errorf("span %q lacks harness.name", sp.Name)
		}
	}
}

// The collector down for a long time: memory stays at queue_size, overflow is
// counted exactly and it is the OLDEST that goes, and delivery resumes when
// the collector returns.
func TestCollectorDownIsBoundedAndRecovers(t *testing.T) {
	rx := newReceiver(t)
	var up atomic.Bool
	rx.respond = func(_ int, w http.ResponseWriter) bool {
		if up.Load() {
			return false
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	}
	const queueSize, batch, total = 20, 5, 200
	res := testResolved(rx.srv.URL, func(c *core.TelemetryConfig) {
		c.Traces = false
		c.QueueSize, c.BatchSize = queueSize, batch
		c.BatchInterval = time.Hour // only full batches, so the stuck one is exactly `batch`
	})
	// The first batch gets stuck in its retry wait until the collector is
	// back, so the queue has to absorb everything else.
	release := make(chan struct{})
	var once sync.Once
	sleep := func(ctx context.Context, _ time.Duration) error {
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	p, obs := startPipeline(t, res, newFakeSource(optIn("w")), Options{Sleep: sleep})

	// Publish in scan-sized bursts, letting intake take each burst before the
	// next — the pace a real observer keeps — so every loss is the queue's.
	for i := range total {
		obs.publish(toolEv("w", "k", i, "item", false, t0))
		switch {
		case i == batch-1:
			waitFor(t, "first attempt", func() bool { return rx.hitCount() >= 1 })
		case i >= batch && i%10 == 9:
			n := i + 1
			waitFor(t, "intake to take the burst", func() bool {
				st := p.Stats()[SignalLogs]
				return int(st.DroppedQueue)+st.QueueLength == n-batch
			})
		}
	}
	st := p.Stats()[SignalLogs]
	if st.QueueLength != queueSize {
		t.Fatalf("queue length %d, want the bound %d", st.QueueLength, queueSize)
	}
	if st.DroppedQueue != total-batch-queueSize {
		t.Fatalf("dropped_queue %d, want exactly %d", st.DroppedQueue, total-batch-queueSize)
	}
	if st.DroppedObserver != 0 {
		t.Fatalf("the observer lost %d events: intake blocked on the exporter", st.DroppedObserver)
	}

	up.Store(true)
	once.Do(func() { close(release) })
	waitFor(t, "recovery", func() bool { return len(rx.snapshotLogs()) == batch+queueSize })
	waitFor(t, "the recovery to be counted", func() bool { return p.Stats()[SignalLogs].Exported == batch+queueSize })
	logs := rx.snapshotLogs()
	// The stuck first batch, then the NEWEST queue_size items.
	for i, l := range logs {
		want := i
		if i >= batch {
			want = total - queueSize + (i - batch)
		}
		if got := l.attr("agent.item.seq"); got != itoaStr(want) {
			t.Fatalf("record %d seq %v, want %d (oldest must be the ones dropped)", i, got, want)
		}
	}
	st = p.Stats()[SignalLogs]
	if st.Exported != batch+queueSize || st.RequestsRetryable == 0 || st.LastSuccess.IsZero() {
		t.Fatalf("stats after recovery %+v", st)
	}
}

func itoaStr(n int) string { b, _ := json.Marshal(n); return string(b) }

func TestRetryAfterIsHonoured(t *testing.T) {
	rx := newReceiver(t)
	rx.respond = func(n int, w http.ResponseWriter) bool {
		if n == 1 {
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(http.StatusTooManyRequests)
			return true
		}
		return false
	}
	var mu sync.Mutex
	var waits []time.Duration
	sleep := func(context.Context, time.Duration) error { return nil }
	record := func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return sleep(ctx, d)
	}
	res := testResolved(rx.srv.URL, func(c *core.TelemetryConfig) { c.Traces = false })
	p, obs := startPipeline(t, res, newFakeSource(optIn("w")), Options{Sleep: record, Jitter: func(time.Duration) time.Duration { return time.Hour }})
	obs.publish(toolEv("w", "k", 0, "x", false, t0))
	waitFor(t, "delivery after the retry", func() bool { return len(rx.snapshotLogs()) == 1 })
	// The receiver records the body before its reply reaches the sender, so
	// wait for the sender to count the success rather than racing it.
	waitFor(t, "the success to be counted", func() bool { return p.Stats()[SignalLogs].RequestsSuccess == 1 })
	mu.Lock()
	defer mu.Unlock()
	if len(waits) != 1 || waits[0] != 7*time.Second {
		t.Fatalf("waits %v, want exactly [7s] (Retry-After, not the jittered backoff)", waits)
	}
	if st := p.Stats()[SignalLogs]; st.RequestsRetryable != 1 || st.RequestsSuccess != 1 || st.Exported != 1 {
		t.Fatalf("stats %+v", st)
	}
}

func TestPermanentFailureIsNotRetried(t *testing.T) {
	rx := newReceiver(t)
	rx.respond = func(_ int, w http.ResponseWriter) bool { w.WriteHeader(http.StatusBadRequest); return true }
	res := testResolved(rx.srv.URL, func(c *core.TelemetryConfig) { c.Traces = false })
	p, obs := startPipeline(t, res, newFakeSource(optIn("w")), Options{Sleep: func(context.Context, time.Duration) error {
		t.Error("a 400 was retried")
		return nil
	}})
	obs.publish(toolEv("w", "k", 0, "x", false, t0))
	waitFor(t, "the failure", func() bool { return p.Stats()[SignalLogs].Failed == 1 })
	if st := p.Stats()[SignalLogs]; st.RequestsPermanent != 1 || rx.hitCount() != 1 || !strings.Contains(st.LastError, "400") {
		t.Fatalf("stats %+v hits %d", st, rx.hitCount())
	}
}

func TestBatchIsGivenUpAfterFiveMinutes(t *testing.T) {
	rx := newReceiver(t)
	rx.respond = func(_ int, w http.ResponseWriter) bool { w.WriteHeader(http.StatusServiceUnavailable); return true }
	var clock atomic.Int64
	clock.Store(t0.UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	var mu sync.Mutex
	var waits []time.Duration
	sleep := func(_ context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		clock.Add(int64(d))
		return nil
	}
	res := testResolved(rx.srv.URL, func(c *core.TelemetryConfig) { c.Traces = false })
	p, obs := startPipeline(t, res, newFakeSource(optIn("w")), Options{Now: now, Sleep: sleep, Jitter: func(max time.Duration) time.Duration { return max }})
	obs.publish(toolEv("w", "k", 0, "x", false, t0))
	waitFor(t, "give-up", func() bool { return p.Stats()[SignalLogs].Failed == 1 })
	mu.Lock()
	defer mu.Unlock()
	// 1+2+4+8+16 = 31s, then 30s steps: the batch is abandoned before the
	// cumulative wait would reach 5 minutes.
	want := []time.Duration{1, 2, 4, 8, 16}
	for i, w := range want {
		if waits[i] != w*time.Second {
			t.Fatalf("wait %d = %s, want %ds (waits %v)", i, waits[i], w, waits)
		}
	}
	var total time.Duration
	for i, w := range waits {
		total += w
		if i >= len(want) && w != 30*time.Second {
			t.Fatalf("wait %d = %s, want the 30s cap", i, w)
		}
	}
	if total >= retryGiveUp || total+30*time.Second < retryGiveUp {
		t.Fatalf("waited %s in total; the give-up should land at the last step before 5m", total)
	}
}

func TestPartialSuccessCountsRejected(t *testing.T) {
	rx := newReceiver(t)
	rx.respond = func(_ int, w http.ResponseWriter) bool {
		_, _ = w.Write([]byte(`{"partialSuccess":{"rejectedLogRecords":"1","errorMessage":"one bad record"}}`))
		return true
	}
	res := testResolved(rx.srv.URL, func(c *core.TelemetryConfig) { c.Traces = false; c.BatchSize = 3 })
	p, obs := startPipeline(t, res, newFakeSource(optIn("w")), Options{})
	obs.publish(toolEv("w", "k", 0, "a", false, t0), toolEv("w", "k", 1, "b", false, t0), toolEv("w", "k", 2, "c", false, t0))
	waitFor(t, "counted", func() bool { st := p.Stats()[SignalLogs]; return st.Exported+st.Rejected == 3 })
	if st := p.Stats()[SignalLogs]; st.Rejected != 1 || st.Exported != 2 {
		t.Fatalf("stats %+v", st)
	}
}

func TestShutdownFlushesSpansAndQueuedRecords(t *testing.T) {
	rx := newReceiver(t)
	res := testResolved(rx.srv.URL, func(c *core.TelemetryConfig) {
		c.BatchInterval = time.Hour // nothing flushes on its own
		c.IdleFlush = time.Hour
	})
	obs := newFakeObserver()
	p := New(res, obs, newFakeSource(optIn("w")), Options{Logger: quietLogger()})
	obs.publish(
		markEv("w", "s1", 0, "user-message", "a", t0), toolEv("w", "s1", 0, "x", false, t0),
		markEv("w", "s2", 0, "user-message", "b", t0), toolEv("w", "s2", 0, "y", false, t0),
	)
	time.Sleep(50 * time.Millisecond) // let intake queue them
	if len(rx.snapshotLogs()) != 0 || len(rx.snapshotSpans()) != 0 {
		t.Fatal("exported before shutdown; the test proves nothing")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rep := p.Shutdown(ctx)
	if len(rx.snapshotLogs()) != 4 || len(rx.snapshotSpans()) != 4 {
		t.Fatalf("after shutdown: %d logs, %d spans; want 4 and 4", len(rx.snapshotLogs()), len(rx.snapshotSpans()))
	}
	if rep.TimedOut || rep.Lost[SignalLogs] != 0 || rep.Lost[SignalTraces] != 0 {
		t.Fatalf("report %+v", rep)
	}
}

func TestShutdownWithAHungCollectorKeepsItsDeadline(t *testing.T) {
	hang := make(chan struct{})
	rx := newReceiver(t)
	rx.respond = func(_ int, w http.ResponseWriter) bool { <-hang; return true }
	t.Cleanup(func() { close(hang) })
	res := testResolved(rx.srv.URL, func(c *core.TelemetryConfig) { c.Traces = false; c.BatchSize = 2 })
	obs := newFakeObserver()
	p := New(res, obs, newFakeSource(optIn("w")), Options{Logger: quietLogger()})
	for i := range 10 {
		obs.publish(toolEv("w", "k", i, "x", false, t0))
	}
	waitFor(t, "a request in flight", func() bool { return rx.hitCount() >= 1 })
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	rep := p.Shutdown(ctx)
	if el := time.Since(start); el > time.Second {
		t.Fatalf("shutdown took %s with a 200ms budget", el)
	}
	if !rep.TimedOut || rep.Lost[SignalLogs] != 10 {
		t.Fatalf("report %+v, want timed out with all 10 records lost", rep)
	}
}

func TestEventsFileLinesAndMode(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "new", "dir")
	file := filepath.Join(dir, "events.jsonl")
	res := testResolved("", func(c *core.TelemetryConfig) { c.Logs, c.Traces = false, false; c.EventsFile = file })
	_, obs := startPipeline(t, res, newFakeSource(optIn("w")), Options{})
	obs.publish(toolEv("w", "k", 0, "multi\nline\nsummary", false, t0), markEv("w", "k", 1, "error", "boom", t0))
	waitFor(t, "2 lines", func() bool { return countLines(file) == 2 })

	info, _ := os.Stat(file)
	dinfo, _ := os.Stat(dir)
	if info.Mode().Perm() != 0o600 || dinfo.Mode().Perm() != 0o700 {
		t.Fatalf("modes file %o dir %o, want 600/700", info.Mode().Perm(), dinfo.Mode().Perm())
	}
	f, _ := os.Open(file)
	defer f.Close()
	sc := bufio.NewScanner(f)
	var lines []map[string]any
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("line is not one JSON object: %q", sc.Text())
		}
		lines = append(lines, m)
	}
	tool, mark := lines[0], lines[1]
	for k, want := range map[string]any{
		"schema": Schema, "severity": "INFO", "body": "multi\nline\nsummary", "harness.name": "w",
		"service.name": "harness", "host.name": "box1", "agent.item.kind": "tool", "agent.tool.is_error": false,
	} {
		if tool[k] != want {
			t.Errorf("tool line %s = %v, want %v", k, tool[k], want)
		}
	}
	if tool["span_id"] == nil || tool["trace_id"] == nil || tool["time"] != t0.Format(time.RFC3339Nano) {
		t.Errorf("tool line ids/time %v", tool)
	}
	if _, present := mark["span_id"]; present {
		t.Error("an error mark line carries span_id; absent fields must be absent, not null")
	}
	if mark["severity"] != "ERROR" {
		t.Errorf("mark severity %v", mark["severity"])
	}
}

func TestLogAttributesAndJSONLKeysAgree(t *testing.T) {
	c := &converter{}
	r := c.record(toolEv("w", "k", 3, "x", true, t0))
	line, err := r.JSONLine(nil)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	_ = json.Unmarshal(line, &m)
	for _, kv := range r.LogRecord().Attributes {
		if _, ok := m[kv.Key]; !ok {
			t.Errorf("log attribute %s missing from the JSONL line", kv.Key)
		}
	}
}

// Intake keeps draining the subscription while the exporter is stuck, so the
// observer never loses an event to telemetry's slowness.
func TestIntakeNeverBlocksOnAStuckExporter(t *testing.T) {
	hang := make(chan struct{})
	rx := newReceiver(t)
	rx.respond = func(_ int, w http.ResponseWriter) bool { <-hang; return true }
	res := testResolved(rx.srv.URL, func(c *core.TelemetryConfig) { c.Traces = false; c.QueueSize, c.BatchSize = 16, 4 })
	p, obs := startPipeline(t, res, newFakeSource(optIn("w")), Options{})
	t.Cleanup(func() { close(hang) }) // before the pipeline's shutdown, so it does not wait out the hang
	waitFor(t, "a request in flight", func() bool {
		obs.publish(toolEv("w", "k", 0, "x", false, t0))
		return rx.hitCount() >= 1
	})
	// With the only request hung, keep publishing bursts: each must be taken
	// off the subscription promptly. A pipeline whose intake waited on the
	// exporter would stop draining and this would time out.
	for burst := range 100 {
		for i := range 10 {
			obs.publish(toolEv("w", "k", 1+burst*10+i, "x", false, t0))
		}
		waitFor(t, "the subscription to drain", func() bool { return obs.pending(SubscriptionName(SignalLogs)) == 0 })
	}
	st := p.Stats()[SignalLogs]
	if st.DroppedObserver != 0 || st.QueueLength != 16 || st.DroppedQueue == 0 {
		t.Fatalf("stats %+v: want a full queue dropping its oldest and no observer loss", st)
	}
}

func TestSubscriptionsAreNamedPerSignal(t *testing.T) {
	res := testResolved("http://127.0.0.1:1", func(c *core.TelemetryConfig) { c.EventsFile = filepath.Join(t.TempDir(), "e.jsonl") })
	_, obs := startPipeline(t, res, newFakeSource(), Options{})
	got := strings.Join(obs.subscriptions(), ",")
	if got != "telemetry.logs,telemetry.traces,telemetry.events_file" {
		t.Fatalf("subscriptions %s", got)
	}
}

func TestOmitPrompts(t *testing.T) {
	c := &converter{omitPrompts: true}
	r := c.record(markEv("w", "k", 0, "user-message", "customer SSN is in here", t0))
	if r.Body != PromptOmitted {
		t.Fatalf("body %q", r.Body)
	}
	for _, kv := range r.Attrs {
		if kv.Key == "agent.session.title" {
			t.Fatal("session title exported under omit_prompts")
		}
	}
	// Non-prompt notes are untouched.
	if r := c.record(markEv("w", "k", 1, "error", "boom", t0)); r.Body != "boom" {
		t.Fatalf("error note %q", r.Body)
	}
	acc := &traceAccumulator{omitPrompts: true}
	if ev := acc.prepare(markEv("w", "k", 0, "user-message", "secret prompt", t0)); ev.Mark.Note != PromptOmitted {
		t.Fatalf("span source note %q", ev.Mark.Note)
	}
}

func TestCaps(t *testing.T) {
	c := &converter{}
	ev := toolEv("w", "k", 0, strings.Repeat("é", 5000), false, t0)
	for i := range 40 {
		ev.Tool.Targets = append(ev.Tool.Targets, ev.Tool.Targets[0])
		ev.Tool.Targets[i].Path = strings.Repeat("p", 600)
	}
	r := c.record(ev)
	if len(r.Body) > bodyCap || !strings.HasSuffix(r.Body, "…") {
		t.Fatalf("body %d bytes", len(r.Body))
	}
	if !strings.HasPrefix(r.Body, "é") || strings.ContainsRune(r.Body, '�') {
		t.Fatal("body cut inside a rune")
	}
	for _, kv := range r.Attrs {
		if kv.Key == "agent.targets" {
			ts := kv.Value.([]string)
			if len(ts) != targetsMax || len(ts[0]) > targetCap {
				t.Fatalf("targets %d, first %d bytes", len(ts), len(ts[0]))
			}
		}
	}
	if got := capString("abcdef", 4); got != "a…" {
		t.Fatalf("capString = %q", got)
	}
}

// omit_prompts, end to end: the prompt text and the prompt-derived session
// title reach neither the collector (logs or spans) nor the events file, and
// the placeholder does. TestOmitPrompts checks the converter; this checks the
// bytes, so a sink that bypassed the converter would fail here.
func TestOmitPromptsReachesEverySink(t *testing.T) {
	rx := newReceiver(t)
	file := filepath.Join(t.TempDir(), "events.jsonl")
	res := testResolved(rx.srv.URL, func(c *core.TelemetryConfig) {
		c.OmitPrompts = true
		c.EventsFile = file
		c.IdleFlush = 30 * time.Millisecond
	})
	_, obs := startPipeline(t, res, newFakeSource(optIn("w")), Options{})
	const prompt = "customer ACME-4417 wants a refund"
	obs.publish(
		markEv("w", "k", 0, "user-message", prompt, t0),
		toolEv("w", "k", 0, "ls", false, t0.Add(time.Second)),
	)
	waitFor(t, "logs, spans and lines", func() bool {
		return len(rx.snapshotLogs()) == 2 && len(rx.snapshotSpans()) == 2 && countLines(file) == 2
	})
	data, _ := os.ReadFile(file)
	for name, got := range map[string]string{"OTLP requests": rx.allRaw(), "events file": string(data)} {
		if strings.Contains(got, "ACME-4417") {
			t.Errorf("%s carry the prompt text", name)
		}
		if strings.Contains(got, session("k").Title) {
			t.Errorf("%s carry the prompt-derived session title", name)
		}
		if !strings.Contains(got, PromptOmitted) {
			t.Errorf("%s lack the %q placeholder — was the prompt ever there?", name, PromptOmitted)
		}
	}
}

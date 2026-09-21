package main

// Daemon Telemetry Shutdown
//
// Governing tests: ADR-0022; SPEC-0015 REQ-11 — the shutdown flush returns
// within shutdown_timeout even when an events-file write never returns (a
// hung NFS mount), and logs what it lost.
//
// The pipeline is built by the daemon's own startDaemonTelemetry from a
// config that went through config.Parse, and shut down by the daemon's own
// shutdownDaemonTelemetry, so the timeout the daemon derives from
// shutdown_timeout is the one under test. The observer and Manager are
// stand-ins: what matters here is the file write, which a seam blocks.
//
// @joestump-agent 09/21/2026 - Added for the hung events-file write finding
// on harness#391.

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"
	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"

	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/observe"
	"github.com/stump-wtf/harness/internal/supervisor"
	"github.com/stump-wtf/harness/internal/telemetry"
)

// oneShotObserver hands each subscriber the same fixed events, then waits
// for its cancel.
type oneShotObserver struct{ events []observe.Event }

func (o oneShotObserver) Subscribe(_ string, buf int) (<-chan observe.Event, func()) {
	ch := make(chan observe.Event, max(buf, len(o.events)))
	for _, ev := range o.events {
		ch <- ev
	}
	var once sync.Once
	return ch, func() { once.Do(func() { close(ch) }) }
}

func (oneShotObserver) Stats() observe.Stats { return observe.Stats{} }

// optedInSource knows one harness, opted in and running.
type optedInSource struct{ h core.Harness }

func (s optedInSource) HarnessDef(name string) (core.Harness, bool) { return s.h, name == s.h.Name }

func (s optedInSource) Snapshot(name string) (supervisor.Snapshot, bool) {
	return supervisor.Snapshot{Name: name, State: core.StateRunning}, name == s.h.Name
}

// hungFile is an events file on a mount that stopped answering: it opens
// and stats like the real file, but every Write blocks until release closes.
type hungFile struct {
	*os.File
	entered chan<- struct{}
	release <-chan struct{}
}

func (f hungFile) Write(p []byte) (int, error) {
	select {
	case f.entered <- struct{}{}:
	default:
	}
	<-f.release
	return len(p), nil
}

func TestDaemonTelemetryShutdownIsBoundedByAHungEventsFile(t *testing.T) {
	tmp := t.TempDir()
	cfg, err := config.Parse([]byte(fmt.Sprintf(`[telemetry]
events_file = %q
export_all = true
batch_size = 1
shutdown_timeout = "300ms"
`, filepath.Join(tmp, "events.jsonl"))), filepath.Join(tmp, "harness.toml"))
	if err != nil {
		t.Fatal(err)
	}
	res, err := resolveDaemonTelemetry(cfg.Telemetry, telemetry.Env{Getenv: func(string) string { return "" }, Hostname: func() (string, error) { return "box", nil }, Version: "test"})
	if err != nil || res == nil {
		t.Fatalf("resolve = %v, %v", res, err)
	}

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	open := func(path string) (telemetry.EventsFileHandle, error) {
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
		if err != nil {
			return nil, err
		}
		return hungFile{f, entered, release}, nil
	}
	now := time.Now()
	var events []observe.Event
	for i := range 5 {
		events = append(events, observe.Event{
			Harness: "worker", Adapter: "crush", Kind: observe.KindMark,
			Session: tail.SessionMeta{Key: "k", ID: "s", Harness: tail.HarnessCrush},
			Mark:    classify.Mark{Seq: i, Type: "error", Note: fmt.Sprint("boom ", i), Timestamp: now.Format(time.RFC3339Nano)},
			Time:    now, ObservedAt: now,
		})
	}
	var logBuf syncBuffer
	p := startDaemonTelemetry(res, oneShotObserver{events}, optedInSource{core.Harness{Name: "worker", Adapter: "crush"}},
		telemetry.Options{Logger: log.New(&logBuf), OpenEventsFile: open})
	if p == nil {
		t.Fatal("no pipeline for a configured events_file")
	}
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no write reached the events file; the test would prove nothing")
	}
	// Let intake queue the other four behind the stuck write.
	deadline := time.Now().Add(5 * time.Second)
	for p.Stats()[telemetry.SignalEventsFile].QueueLength < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("queue = %d, want 4 behind the hung write", p.Stats()[telemetry.SignalEventsFile].QueueLength)
		}
		time.Sleep(5 * time.Millisecond)
	}

	start := time.Now()
	done := shutdownDaemonTelemetry(p, res)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the daemon's telemetry shutdown hung behind the events-file write")
	}
	if el, budget := time.Since(start), res.Config.ShutdownTimeout; el > budget+250*time.Millisecond {
		t.Fatalf("shutdown took %s with a %s budget", el, budget)
	}
	rep := p.Shutdown(context.Background()) // idempotent: the first call's report
	if !rep.TimedOut || rep.Lost[telemetry.SignalEventsFile] != 5 {
		t.Fatalf("report %+v, want timed out with all 5 lines lost", rep)
	}
	line := logBuf.String()
	if !strings.Contains(line, "telemetry flushed") || !strings.Contains(line, "events_file_lost=5") || !strings.Contains(line, "abandoned_writes=1") {
		t.Fatalf("shutdown did not log what it lost:\n%s", line)
	}
}

// syncBuffer is a bytes.Buffer safe for the pipeline's goroutines to log to.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

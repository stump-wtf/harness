package notify

// Watcher Tests
//
// Governing tests: SPEC-0003 REQ "Operator Notification"; issue #725 — which
// lifecycle events become which notifications, what each message says, and
// when `recovered` may fire. The daemon's own wiring of these sources is
// covered in cmd/harness (daemon_notify_test.go) against a real Manager.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/loopguard"
	"github.com/stump-wtf/harness/internal/supervisor"
)

type fakeSource struct {
	ch   chan supervisor.Event
	snap supervisor.Snapshot
	dir  string
}

func (f *fakeSource) Events() (<-chan supervisor.Event, func()) {
	return f.ch, func() {
		defer func() { _ = recover() }()
		close(f.ch)
	}
}
func (f *fakeSource) Snapshot(string) (supervisor.Snapshot, bool) { return f.snap, true }
func (f *fakeSource) LogDir() string                              { return f.dir }
func (f *fakeSource) RunLogPath(name string, id int) string {
	return filepath.Join(f.dir, name, "run.log")
}

func newWatcherRig(t *testing.T, events []string) (*fakeSource, *Watcher, string) {
	t.Helper()
	argv, out := NewRecorder(t)
	cfg := testConfig(argv)
	if events != nil {
		cfg.Events = events
	}
	d := newTestDispatcher(t, cfg, Options{})
	src := &fakeSource{ch: make(chan supervisor.Event, 16), dir: t.TempDir()}
	w := Watch(src, d)
	t.Cleanup(w.Close)
	return src, w, out
}

func TestWatcherRunFailedQuotesTheRunLog(t *testing.T) {
	src, _, out := newWatcherRig(t, core.NotifyEvents)
	runLog := filepath.Join(src.dir, "nightly", "run.log")
	if err := os.MkdirAll(filepath.Dir(runLog), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(runLog, []byte("fetching\nfatal: repository not found\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code := 128
	// A successful run is not news.
	src.ch <- supervisor.Event{Kind: supervisor.EventRunFinished, Name: "nightly", Run: supervisor.RunRecord{RunID: 41, Outcome: supervisor.OutcomeSuccess}}
	src.ch <- supervisor.Event{Kind: supervisor.EventRunFinished, Name: "nightly", Run: supervisor.RunRecord{RunID: 42, Outcome: supervisor.OutcomeFailed, ExitCode: &code}}
	r := WaitReceived(t, out, 1)
	time.Sleep(100 * time.Millisecond)
	if n := len(ReadReceived(t, out)); n != 1 {
		t.Fatalf("%d deliveries, want 1 (the successful run must not notify)", n)
	}
	p := r[0].Payload
	if p.Event != core.NotifyRunFailed || p.RunID != 42 || p.Cause != "fatal: repository not found" ||
		p.Hint != "harness logs nightly --run 42" || !strings.Contains(p.Message, "run #42 failed (exit 128)") {
		t.Fatalf("payload = %+v", p)
	}
}

func TestWatcherFlapping(t *testing.T) {
	src, _, out := newWatcherRig(t, nil)
	src.snap = supervisor.Snapshot{State: core.StateRestarting, LastExitCode: 1}
	if err := os.WriteFile(filepath.Join(src.dir, "rc.log"), []byte("Error: You must be logged in to use Remote Control.\n2026/09/25 20:15:11 INFO exited code=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src.ch <- supervisor.Event{Kind: supervisor.EventFlapping, Name: "rc", Restarts: 3, NextRetryIn: 8 * time.Second}
	p := WaitReceived(t, out, 1)[0].Payload
	if p.Event != core.NotifyFlapping || p.Restarts != 3 || p.State != string(core.StateRestarting) ||
		!strings.Contains(p.Message, `rc is crash-looping: 3 restarts, next retry in 8s (last exit 1): "Error: You must be logged in to use Remote Control."`) {
		t.Fatalf("payload = %+v", p)
	}
}

// `recovered` closes an alert the operator received — so only after one.
func TestWatcherRecoveredOnlyAfterAnAlert(t *testing.T) {
	src, w, out := newWatcherRig(t, nil)
	// Running with nothing open: an ordinary start, not a recovery. The
	// flapping sentinel behind it proves the start was consumed before the
	// loop stop below opens an alert.
	src.ch <- supervisor.Event{Kind: supervisor.EventStateChanged, Name: "a", From: core.StateStarting, To: core.StateRunning}
	src.ch <- supervisor.Event{Kind: supervisor.EventFlapping, Name: "sentinel", Restarts: 2}
	WaitReceived(t, out, 1)
	w.LoopStopped(loopguard.Trip{Harness: "a", Tool: "mcp_gitea_issue_write", Count: 8})
	src.ch <- supervisor.Event{Kind: supervisor.EventStateChanged, Name: "a", From: core.StateStarting, To: core.StateRunning}
	got := WaitReceived(t, out, 3)
	var loop, rec Payload
	for _, r := range got {
		switch r.Payload.Event {
		case core.NotifyLoopStopped:
			loop = r.Payload
		case core.NotifyRecovered:
			rec = r.Payload
		}
	}
	if loop.Tool != "mcp_gitea_issue_write" || loop.Count != 8 || loop.State != string(core.StateStopped) ||
		!strings.Contains(loop.Message, "stays down until `harness start a`") {
		t.Fatalf("loop_stopped payload = %+v", loop)
	}
	if rec.Harness != "a" || rec.Message != "a is running again (was loop-stopped)" {
		t.Fatalf("recovered payload = %+v", rec)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(ReadReceived(t, out)); n != 3 {
		t.Fatalf("%d deliveries, want exactly 3 (sentinel, loop_stopped, recovered)", n)
	}
}

// An alert the operator filtered out opens nothing: they must not hear of a
// recovery from something they were never told about.
func TestWatcherNoRecoveryForAnUnwantedEvent(t *testing.T) {
	src, w, out := newWatcherRig(t, []string{core.NotifyRecovered, core.NotifyFlapping})
	w.LoopStopped(loopguard.Trip{Harness: "a", Tool: "t", Count: 8})
	src.ch <- supervisor.Event{Kind: supervisor.EventStateChanged, Name: "a", To: core.StateRunning}
	// A wanted event after it, so the silence below is not just "too soon".
	src.ch <- supervisor.Event{Kind: supervisor.EventFlapping, Name: "b", Restarts: 2}
	got := WaitReceived(t, out, 1)
	time.Sleep(100 * time.Millisecond)
	if got = ReadReceived(t, out); len(got) != 1 || got[0].Payload.Event != core.NotifyFlapping {
		t.Fatalf("deliveries = %+v, want the flapping one only", got)
	}
}

func TestWatcherSessionRotated(t *testing.T) {
	_, w, out := newWatcherRig(t, nil)
	w.SessionRotated(supervisor.SessionRotation{Harness: "crush-qwen", Turns: 5, Errors: 5, Archive: "/w/.crush/crush.db.wedged-20260926", Rotations: 1})
	w.SessionRotated(supervisor.SessionRotation{Harness: "crush-qwen-2", Turns: 4, Errors: 4, Failed: "could not restart the harness after archiving its store"})
	got := WaitReceived(t, out, 2)
	by := map[string]Payload{}
	for _, r := range got {
		by[r.Payload.Harness] = r.Payload
	}
	ok := by["crush-qwen"]
	if ok.Event != core.NotifySessionRotated || ok.State != string(core.StateRunning) ||
		!strings.Contains(ok.Message, "5 of 5 recent turns failed on context-limit errors") ||
		!strings.Contains(ok.Message, "crush.db.wedged-20260926") {
		t.Fatalf("rotation payload = %+v", ok)
	}
	bad := by["crush-qwen-2"]
	if bad.State != string(core.StateStopped) || !strings.Contains(bad.Message, "rotation failed") ||
		!strings.Contains(bad.Message, "harness start crush-qwen-2") {
		t.Fatalf("failed rotation payload = %+v", bad)
	}
}

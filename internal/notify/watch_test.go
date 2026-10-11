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
	runs []supervisor.RunRecord
	park supervisor.ParkInfo
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
func (f *fakeSource) Runs(string) []supervisor.RunRecord { return f.runs }
func (f *fakeSource) ParkOf(string) (supervisor.ParkInfo, bool) {
	return f.park, !f.park.Until.IsZero()
}

func newWatcherRig(t *testing.T, events []string) (*fakeSource, *Watcher, string) {
	t.Helper()
	argv, out := NewRecorder(t)
	cfg := testConfig(t, argv)
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
	src, w, out := newWatcherRig(t, core.NotifyEvents)
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
	r := WaitReceived(t, w.d, out, 1)
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

// A park is announced once — when quota joins the hold reasons — with its
// reset and rule, and not again while it holds (another reason joining or
// leaving changes nothing). Its clearing re-arms the announcement, so the
// next park is heard.
func TestWatcherParkedOncePerPark(t *testing.T) {
	argv, out := NewRecorder(t)
	cfg := testConfig(t, argv)
	cfg.Cooldown = 0 // the second park is a new alert, not a repeat
	d := newTestDispatcher(t, cfg, Options{})
	until := time.Date(2026, 10, 4, 10, 20, 0, 0, time.UTC)
	src := &fakeSource{
		ch:   make(chan supervisor.Event, 16),
		dir:  t.TempDir(),
		snap: supervisor.Snapshot{Name: "review", State: core.StateStopped},
		park: supervisor.ParkInfo{Until: until, Rule: "common/payment required"},
	}
	w := Watch(src, d)
	t.Cleanup(w.Close)
	quota := core.HoldSetOf(core.HoldQuota)
	src.ch <- supervisor.Event{Kind: supervisor.EventHoldChanged, Name: "review", Holds: quota, HoldNext: until}
	p := WaitReceived(t, w.d, out, 1)[0].Payload
	want := "review parked until " + until.Local().Format("Jan 2 15:04 MST") +
		": its provider refused it for quota (common/payment required). Its runs are skipped until then and it is released by itself — see `harness logs review`"
	if p.Event != core.NotifyParked || p.Message != want || p.Cause != "common/payment required" ||
		p.Until != "2026-10-04T10:20:00Z" || p.State != string(core.StateStopped) || p.Hint != "harness logs review" {
		t.Fatalf("payload = %+v\nwant message %q", p, want)
	}

	// Still parked, hours joining and leaving: no new alert.
	src.ch <- supervisor.Event{Kind: supervisor.EventHoldChanged, Name: "review", Holds: quota.With(core.HoldHours)}
	src.ch <- supervisor.Event{Kind: supervisor.EventHoldChanged, Name: "review", Holds: quota}
	// Released, then parked again: one more.
	src.ch <- supervisor.Event{Kind: supervisor.EventHoldChanged, Name: "review"}
	src.ch <- supervisor.Event{Kind: supervisor.EventHoldChanged, Name: "review", Holds: quota}
	WaitReceived(t, w.d, out, 2)
	time.Sleep(100 * time.Millisecond)
	if n := len(ReadReceived(t, out)); n != 2 {
		t.Fatalf("%d deliveries, want 2 (one per park)", n)
	}
}

// A hold that is not quota is not a park.
func TestWatcherHoursHoldIsNotAPark(t *testing.T) {
	src, w, out := newWatcherRig(t, core.NotifyEvents)
	src.ch <- supervisor.Event{Kind: supervisor.EventHoldChanged, Name: "rc", Holds: core.HoldSetOf(core.HoldHours)}
	src.ch <- supervisor.Event{Kind: supervisor.EventHoldChanged, Name: "rc", Holds: core.HoldSetOf(core.HoldBudget)}
	src.ch <- supervisor.Event{Kind: supervisor.EventFlapping, Name: "sentinel", Restarts: 2}
	WaitReceived(t, w.d, out, 1)
	time.Sleep(100 * time.Millisecond)
	if got := ReadReceived(t, out); len(got) != 1 || got[0].Payload.Event != core.NotifyFlapping {
		t.Fatalf("deliveries = %+v, want the sentinel only", got)
	}
}

// A park the daemon booted with was announced by the daemon before it: a
// restart or an upgrade does not repeat it (SPEC-0003 Scenario "A park is
// heard once").
func TestWatcherRestoredParkIsNotAnnouncedAgain(t *testing.T) {
	src, w, out := newWatcherRig(t, core.NotifyEvents)
	src.park = supervisor.ParkInfo{Until: time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC), Rule: "common/payment required", Restored: true}
	src.ch <- supervisor.Event{Kind: supervisor.EventHoldChanged, Name: "review", Holds: core.HoldSetOf(core.HoldQuota)}
	src.ch <- supervisor.Event{Kind: supervisor.EventFlapping, Name: "sentinel", Restarts: 2}
	WaitReceived(t, w.d, out, 1)
	time.Sleep(100 * time.Millisecond)
	if got := ReadReceived(t, out); len(got) != 1 || got[0].Payload.Event != core.NotifyFlapping {
		t.Fatalf("deliveries = %+v, want the sentinel only", got)
	}
}

// A group park says which group, and whose refusal parked it.
func TestWatcherParkedGroupNamesTheTrigger(t *testing.T) {
	src, w, out := newWatcherRig(t, core.NotifyEvents)
	src.park = supervisor.ParkInfo{Until: time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC), Rule: "crush/litellm.ratelimiterror", Group: "hyper", By: "review"}
	src.ch <- supervisor.Event{Kind: supervisor.EventHoldChanged, Name: "glm-review", Holds: core.HoldSetOf(core.HoldQuota)}
	p := WaitReceived(t, w.d, out, 1)[0].Payload
	if !strings.Contains(p.Message, "(crush/litellm.ratelimiterror), which parks quota group hyper after review was refused.") {
		t.Fatalf("message = %q", p.Message)
	}
}

// run_failed names the streak `harness jobs` shows, read off the ledger: the
// closed run is already in it, and records with no verdict (a skip) neither
// count nor break it.
func TestWatcherRunFailedCountsTheStreak(t *testing.T) {
	src, w, out := newWatcherRig(t, core.NotifyEvents)
	src.runs = []supervisor.RunRecord{
		{RunID: 1, Outcome: supervisor.OutcomeSuccess},
		{RunID: 2, Outcome: supervisor.OutcomeFailed},
		{RunID: 3, Outcome: supervisor.OutcomeSkipped},
		{RunID: 4, Outcome: supervisor.OutcomeTimedOut},
		{RunID: 5, Outcome: supervisor.OutcomeFailed},
	}
	code := 1
	src.ch <- supervisor.Event{Kind: supervisor.EventRunFinished, Name: "review", Run: supervisor.RunRecord{RunID: 5, Outcome: supervisor.OutcomeFailed, ExitCode: &code}}
	p := WaitReceived(t, w.d, out, 1)[0].Payload
	if !strings.HasPrefix(p.Message, "review run #5 failed (exit 1), 3 in a row — see") {
		t.Fatalf("message = %q", p.Message)
	}
}

// The first failure of a streak is just "failed": "1 in a row" says nothing.
func TestWatcherRunFailedAloneHasNoStreak(t *testing.T) {
	src, w, out := newWatcherRig(t, core.NotifyEvents)
	src.runs = []supervisor.RunRecord{{RunID: 7, Outcome: supervisor.OutcomeSuccess}, {RunID: 8, Outcome: supervisor.OutcomeFailed}}
	code := 2
	src.ch <- supervisor.Event{Kind: supervisor.EventRunFinished, Name: "nightly", Run: supervisor.RunRecord{RunID: 8, Outcome: supervisor.OutcomeFailed, ExitCode: &code}}
	if p := WaitReceived(t, w.d, out, 1)[0].Payload; strings.Contains(p.Message, "in a row") {
		t.Fatalf("message = %q", p.Message)
	}
}

// A triggered harness's run that fails lands it in `failed`, and its next
// firing is the retry. That is not a give-up: no `failed` alert, and so no
// `recovered` when the next firing starts. Before this, the tars review lane
// sent "gave up after 0 consecutive failures" and "running again" every ten
// minutes for ten hours while each run died on the same 402.
func TestWatcherTriggeredFailedIsNotAGiveUp(t *testing.T) {
	src, w, out := newWatcherRig(t, core.NotifyEvents)
	src.snap = supervisor.Snapshot{Name: "review", State: core.StateFailed, Triggered: true, LastExitCode: 1}
	src.ch <- supervisor.Event{Kind: supervisor.EventStateChanged, Name: "review", From: core.StateDegraded, To: core.StateFailed}
	src.ch <- supervisor.Event{Kind: supervisor.EventStateChanged, Name: "review", From: core.StateStarting, To: core.StateRunning}
	// A wanted event after them, so the silence is not just "too soon".
	src.ch <- supervisor.Event{Kind: supervisor.EventFlapping, Name: "sentinel", Restarts: 2}
	WaitReceived(t, w.d, out, 1)
	time.Sleep(100 * time.Millisecond)
	if got := ReadReceived(t, out); len(got) != 1 || got[0].Payload.Event != core.NotifyFlapping {
		t.Fatalf("deliveries = %+v, want the sentinel only", got)
	}
}

// A resident that lands in `failed` with no streak — a command that never
// came up, under a restart policy that will not retry it — did not give up
// after anything, so the message does not count zero failures.
func TestWatcherFailedWithoutAStreak(t *testing.T) {
	src, w, out := newWatcherRig(t, nil)
	src.snap = supervisor.Snapshot{Name: "rc", State: core.StateFailed, LastExitCode: 127}
	src.ch <- supervisor.Event{Kind: supervisor.EventStateChanged, Name: "rc", From: core.StateStarting, To: core.StateFailed}
	p := WaitReceived(t, w.d, out, 1)[0].Payload
	if p.Event != core.NotifyFailed || !strings.HasPrefix(p.Message, "rc failed and will not restart on its own (last exit 127)") ||
		strings.Contains(p.Message, "gave up") {
		t.Fatalf("payload = %+v", p)
	}
}

func TestWatcherFlapping(t *testing.T) {
	src, w, out := newWatcherRig(t, nil)
	src.snap = supervisor.Snapshot{State: core.StateRestarting, LastExitCode: 1}
	if err := os.WriteFile(filepath.Join(src.dir, "rc.log"), []byte("Error: You must be logged in to use Remote Control.\n2026/09/25 20:15:11 INFO exited code=1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src.ch <- supervisor.Event{Kind: supervisor.EventFlapping, Name: "rc", Restarts: 3, NextRetryIn: 8 * time.Second}
	p := WaitReceived(t, w.d, out, 1)[0].Payload
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
	WaitReceived(t, w.d, out, 1)
	w.LoopStopped(loopguard.Trip{Harness: "a", Tool: "mcp_gitea_issue_write", Count: 8})
	src.ch <- supervisor.Event{Kind: supervisor.EventStateChanged, Name: "a", From: core.StateStarting, To: core.StateRunning}
	got := WaitReceived(t, w.d, out, 3)
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
	got := WaitReceived(t, w.d, out, 1)
	time.Sleep(100 * time.Millisecond)
	if got = ReadReceived(t, out); len(got) != 1 || got[0].Payload.Event != core.NotifyFlapping {
		t.Fatalf("deliveries = %+v, want the flapping one only", got)
	}
}

func TestWatcherSessionRotated(t *testing.T) {
	_, w, out := newWatcherRig(t, nil)
	w.SessionRotated(supervisor.SessionRotation{Harness: "crush-qwen", Turns: 5, Errors: 5, Archive: "/w/.crush/crush.db.wedged-20260926", Rotations: 1})
	w.SessionRotated(supervisor.SessionRotation{Harness: "crush-qwen-2", Turns: 4, Errors: 4, Failed: "could not restart the harness after archiving its store"})
	got := WaitReceived(t, w.d, out, 2)
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

// ---- issue #835: dormant autostart members raise intent_lost -------------

// A boot that finds autostart members left down by persisted intent must tell
// the operator, once per harness, and the notification must carry what
// happened and how to answer it.
func TestWatcherIntentLostNotifiesPerHarness(t *testing.T) {
	_, w, out := newWatcherRig(t, core.NotifyEvents)
	w.IntentLost([]string{"alpha", "beta"})
	got := WaitReceived(t, w.d, out, 2)
	time.Sleep(100 * time.Millisecond)
	if n := len(ReadReceived(t, out)); n != 2 {
		t.Fatalf("%d deliveries, want 2 (one per dormant harness)", n)
	}
	by := map[string]Payload{}
	for _, r := range got {
		by[r.Payload.Harness] = r.Payload
	}
	a, ok := by["alpha"]
	if !ok {
		t.Fatalf("no delivery for alpha: %+v", got)
	}
	if a.Event != core.NotifyIntentLost || a.State != string(core.StateStopped) ||
		a.Cause != "persisted intent is disabled" || a.Hint != "harness describe alpha" ||
		!strings.Contains(a.Message, "left down at boot by persisted intent") ||
		!strings.Contains(a.Message, "harness start alpha") {
		t.Fatalf("alpha payload = %+v", a)
	}
	b, ok := by["beta"]
	if !ok || b.Event != core.NotifyIntentLost {
		t.Fatalf("beta payload = %+v (ok=%v)", b, ok)
	}
}

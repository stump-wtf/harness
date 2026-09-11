package supervisor

// Run History Tests
//
// Every test here drives a real Manager and real PTY-spawned `sh` processes, so
// a record is only ever produced by the same actor-loop path production uses:
// the scheduler's StartRun, an operator's Start/Stop/Restart, a timeout, a
// daemon shutdown.
//
// Governing: ADR-0007, ADR-0008, ADR-0013; SPEC-0008 REQ "Run History", REQ
// "Per-Run Logs", REQ "Run Timeout", REQ "Overlap Policy", REQ "Firing And
// Overlap"; issue #119.
//
// @joestump-agent 09/11/2026 - Added for issue #119.

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// sweep builds a scheduled harness that runs `sh -c script`.
func sweep(name, script string) core.Harness {
	h := shHarness(name, script, 0)
	h.Schedule = "0 3 * * *"
	h.Restart = core.RestartNo
	return h
}

// sweepCfg is a config of scheduled harnesses, none an autostart member.
func sweepCfg(hs ...core.Harness) *core.Config {
	cfg := managerCfg(hs...)
	cfg.Profiles["default"] = core.Profile{Name: "default", Autostart: false}
	return cfg
}

// runsEnv is one daemon's on-disk state, reusable across Manager lifetimes.
type runsEnv struct{ dir, state, logs, jobs string }

func newRunsEnv(t *testing.T) runsEnv {
	t.Helper()
	dir := t.TempDir()
	return runsEnv{
		dir:   dir,
		state: filepath.Join(dir, "state.json"),
		logs:  filepath.Join(dir, "logs"),
		jobs:  filepath.Join(dir, "jobs"),
	}
}

// manager starts a restored Manager on e and returns it with an idempotent
// close, so a test can shut one daemon down and boot the next.
func (e runsEnv) manager(t *testing.T, cfg *core.Config, p Policy) (*Manager, func()) {
	t.Helper()
	m := NewManager(cfg, ManagerOptions{Policy: p, StatePath: e.state, LogDir: e.logs})
	closeOnce := sync.OnceFunc(m.Close)
	t.Cleanup(closeOnce)
	if err := m.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	return m, closeOnce
}

// waitRuns polls name's history until pred holds.
func waitRuns(t *testing.T, m *Manager, name, desc string, pred func([]RunRecord) bool) []RunRecord {
	t.Helper()
	var got []RunRecord
	waitFor(t, 5*time.Second, desc, func() bool {
		got = m.Runs(name)
		return pred(got)
	})
	return got
}

func outcomesOf(rs []RunRecord) []RunOutcome {
	out := make([]RunOutcome, len(rs))
	for i, r := range rs {
		out[i] = r.Outcome
	}
	return out
}

// outcomesAre is a waitRuns predicate for an exact outcome sequence.
func outcomesAre(want ...RunOutcome) func([]RunRecord) bool {
	return func(rs []RunRecord) bool { return slices.Equal(outcomesOf(rs), want) }
}

func readText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestRunRecordsCleanAndFailingExits: a run that exits on its own is recorded
// success or failed with its exit code, trigger, window and bounds, and its log
// holds its output between a start and a finish line.
func TestRunRecordsCleanAndFailingExits(t *testing.T) {
	e := newRunsEnv(t)
	m, _ := e.manager(t, sweepCfg(sweep("ok", "echo hello-from-run; exit 0"), sweep("bad", "exit 3")), fastPolicy())
	window := time.Date(2026, 9, 11, 3, 0, 0, 0, time.UTC)

	m.StartRun("ok", RunRequest{Trigger: TriggerSchedule, Window: window})
	m.StartRun("bad", RunRequest{Trigger: TriggerSchedule, Window: window})

	ok := waitRuns(t, m, "ok", "ok run finishes", outcomesAre(OutcomeSuccess))[0]
	if ok.RunID != 1 || ok.Trigger != TriggerSchedule || ok.ExitCode == nil || *ok.ExitCode != 0 {
		t.Errorf("success record = %+v", ok)
	}
	if ok.Window == nil || !ok.Window.Equal(window) || ok.EndedAt == nil || ok.EndedAt.Before(ok.StartedAt) {
		t.Errorf("success record window/bounds wrong: %+v", ok)
	}
	log := readText(t, m.RunLogPath("ok", 1))
	for _, want := range []string{"run started", "hello-from-run", "run finished"} {
		if !strings.Contains(log, want) {
			t.Errorf("run log missing %q:\n%s", want, log)
		}
	}
	if strings.Index(log, "hello-from-run") > strings.LastIndex(log, "run finished") {
		t.Errorf("run output landed after the finish line:\n%s", log)
	}

	bad := waitRuns(t, m, "bad", "failing run finishes", outcomesAre(OutcomeFailed))[0]
	if bad.ExitCode == nil || *bad.ExitCode != 3 {
		t.Errorf("failed record exit code = %v, want 3", bad.ExitCode)
	}
	if snap, _ := m.Snapshot("bad"); snap.State != core.StateFailed {
		t.Errorf("state after a failed run = %s, want failed", snap.State)
	}
}

// TestRunSpawnFailureIsFailed: a run whose process never starts is failed,
// with no exit code because nothing exited.
func TestRunSpawnFailureIsFailed(t *testing.T) {
	e := newRunsEnv(t)
	h := sweep("nowhere", "exit 0")
	h.Workdir = filepath.Join(e.dir, "does-not-exist")
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	m.StartRun("nowhere", RunRequest{Trigger: TriggerSchedule})
	r := waitRuns(t, m, "nowhere", "spawn failure recorded", outcomesAre(OutcomeFailed))[0]
	if r.ExitCode != nil {
		t.Errorf("spawn failure carries exit code %d", *r.ExitCode)
	}
}

// TestRunTimeoutRecordsTimedOut: a run outliving its timeout is ended —
// SIGTERM, then SIGKILL for one that ignores it — recorded timed_out, and
// leaves the harness failed.
func TestRunTimeoutRecordsTimedOut(t *testing.T) {
	for _, tc := range []struct{ name, script string }{
		{"exits on SIGTERM", "sleep 30"},
		{"ignores SIGTERM", "trap '' TERM; while true; do sleep 0.05; done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newRunsEnv(t)
			h := sweep("slow", tc.script)
			h.Timeout = 300 * time.Millisecond
			m, _ := e.manager(t, sweepCfg(h), fastPolicy())

			m.StartRun("slow", RunRequest{Trigger: TriggerSchedule})
			r := waitRuns(t, m, "slow", "run times out", outcomesAre(OutcomeTimedOut))[0]
			if r.ExitCode == nil {
				t.Error("timed-out run has no exit code")
			}
			if d := r.EndedAt.Sub(r.StartedAt); d < h.Timeout || d > 5*time.Second {
				t.Errorf("run lasted %v, want just past the %v timeout", d, h.Timeout)
			}
			waitFor(t, 3*time.Second, "harness lands in failed", func() bool {
				snap, _ := m.Snapshot("slow")
				return snap.State == core.StateFailed
			})
			if log := readText(t, m.RunLogPath("slow", 1)); !strings.Contains(log, "run timed out") {
				t.Errorf("run log does not record the timeout:\n%s", log)
			}
		})
	}
}

// TestOverlapSkip: a firing while a run is in flight is recorded skipped and
// starts nothing; the run in flight is untouched.
func TestOverlapSkip(t *testing.T) {
	e := newRunsEnv(t)
	m, _ := e.manager(t, sweepCfg(sweep("busy", "sleep 30")), fastPolicy())

	m.StartRun("busy", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "busy", "run in flight", outcomesAre(OutcomeRunning))
	m.StartRun("busy", RunRequest{Trigger: TriggerCatchUp, Windows: 2})

	rs := waitRuns(t, m, "busy", "second firing skipped", outcomesAre(OutcomeRunning, OutcomeSkipped))
	if s := rs[1]; s.ExitCode != nil || s.Trigger != TriggerCatchUp || s.Windows != 2 || s.EndedAt == nil {
		t.Errorf("skipped record = %+v", s)
	}
	m.Stop("busy")
	waitRuns(t, m, "busy", "stop cancels the run", outcomesAre(OutcomeCancelled, OutcomeSkipped))
}

// TestOverlapQueue: a firing while a run is in flight is held and starts when
// the run ends; a second firing while one is held is skipped.
func TestOverlapQueue(t *testing.T) {
	e := newRunsEnv(t)
	h := sweep("queued", "sleep 0.4")
	h.OnOverlap = core.OverlapQueue
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	m.StartRun("queued", RunRequest{Trigger: TriggerSchedule})
	m.StartRun("queued", RunRequest{Trigger: TriggerCatchUp}) // held
	m.StartRun("queued", RunRequest{Trigger: TriggerManual})  // queue full

	rs := waitRuns(t, m, "queued", "held firing runs after the first",
		outcomesAre(OutcomeSuccess, OutcomeSkipped, OutcomeSuccess))
	if rs[1].Trigger != TriggerManual {
		t.Errorf("skipped the wrong firing: %+v", rs[1])
	}
	if rs[2].Trigger != TriggerCatchUp || rs[2].StartedAt.Before(*rs[0].EndedAt) {
		t.Errorf("queued run = %+v, want the held catch-up firing, started after run 1 ended", rs[2])
	}
}

// TestOverlapReplace: a firing while a run is in flight stops that run,
// recorded replaced, and starts the new one.
func TestOverlapReplace(t *testing.T) {
	e := newRunsEnv(t)
	h := sweep("replaced", "sleep 30")
	h.OnOverlap = core.OverlapReplace
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	m.StartRun("replaced", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "replaced", "first run in flight", outcomesAre(OutcomeRunning))
	m.StartRun("replaced", RunRequest{Trigger: TriggerCatchUp})

	rs := waitRuns(t, m, "replaced", "first run replaced", outcomesAre(OutcomeReplaced, OutcomeRunning))
	if rs[0].ExitCode == nil || rs[1].Trigger != TriggerCatchUp {
		t.Errorf("records = %+v", rs)
	}
	waitFor(t, 3*time.Second, "replacement is running", func() bool {
		snap, _ := m.Snapshot("replaced")
		return snap.State == core.StateRunning
	})
	m.Stop("replaced")
	waitRuns(t, m, "replaced", "stop cancels the replacement", outcomesAre(OutcomeReplaced, OutcomeCancelled))
}

// TestStopCancelsRunAndQueuedFiring: an operator stop cancels the run in
// flight and the firing held behind it, which never starts.
func TestStopCancelsRunAndQueuedFiring(t *testing.T) {
	e := newRunsEnv(t)
	h := sweep("held", "sleep 30")
	h.OnOverlap = core.OverlapQueue
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	m.StartRun("held", RunRequest{Trigger: TriggerSchedule})
	m.StartRun("held", RunRequest{Trigger: TriggerCatchUp})
	m.Stop("held")

	rs := waitRuns(t, m, "held", "run and held firing cancelled", outcomesAre(OutcomeCancelled, OutcomeCancelled))
	if rs[1].Trigger != TriggerCatchUp || rs[1].ExitCode != nil {
		t.Errorf("cancelled held firing = %+v", rs[1])
	}
	time.Sleep(200 * time.Millisecond)
	if snap, _ := m.Snapshot("held"); snap.State != core.StateStopped {
		t.Errorf("the held firing started after a stop: state %s", snap.State)
	}
}

// TestManualStartAndRestartRecordRuns: an operator start of a scheduled
// harness is a manual run, and a restart replaces it with another.
func TestManualStartAndRestartRecordRuns(t *testing.T) {
	e := newRunsEnv(t)
	m, _ := e.manager(t, sweepCfg(sweep("hand", "sleep 30")), fastPolicy())

	m.Start("hand")
	waitRuns(t, m, "hand", "manual run in flight", outcomesAre(OutcomeRunning))
	m.Restart("hand")
	rs := waitRuns(t, m, "hand", "restart replaces the run", outcomesAre(OutcomeReplaced, OutcomeRunning))
	for _, r := range rs {
		if r.Trigger != TriggerManual {
			t.Errorf("trigger = %s, want manual: %+v", r.Trigger, r)
		}
	}
	m.Stop("hand")
}

// TestStartRunWhileStoppingIsSkipped: a firing that lands during a graceful
// stop is recorded skipped and does not start the harness again once the stop
// completes.
func TestStartRunWhileStoppingIsSkipped(t *testing.T) {
	e := newRunsEnv(t)
	p := fastPolicy()
	p.StopGrace = time.Second
	// The ready file is written only once the trap is installed. Stopping
	// before that races the shell: a SIGTERM that lands first kills it at once,
	// and the stopping state is gone before anything can observe it.
	ready := filepath.Join(e.dir, "trap-installed")
	script := fmt.Sprintf("trap '' TERM; touch %q; while true; do sleep 0.05; done", ready)
	m, _ := e.manager(t, sweepCfg(sweep("stubborn", script)), p)

	m.StartRun("stubborn", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "stubborn", "run in flight", outcomesAre(OutcomeRunning))
	waitFor(t, 3*time.Second, "the run has installed its TERM trap", func() bool {
		_, err := os.Stat(ready)
		return err == nil
	})
	stopped := make(chan struct{})
	go func() {
		m.Stop("stubborn")
		close(stopped)
	}()
	waitFor(t, 3*time.Second, "harness is stopping", func() bool {
		snap, _ := m.Snapshot("stubborn")
		return snap.State == core.StateStopping
	})
	m.StartRun("stubborn", RunRequest{Trigger: TriggerSchedule})
	<-stopped

	waitRuns(t, m, "stubborn", "run cancelled, firing skipped", outcomesAre(OutcomeCancelled, OutcomeSkipped))
	time.Sleep(200 * time.Millisecond)
	if snap, _ := m.Snapshot("stubborn"); snap.State != core.StateStopped {
		t.Errorf("a firing during the stop restarted the harness: state %s", snap.State)
	}
}

// TestShutdownInterruptsRun: a clean daemon shutdown ends the run in flight as
// interrupted, and the next daemon reads it back that way.
func TestShutdownInterruptsRun(t *testing.T) {
	e := newRunsEnv(t)
	cfg := sweepCfg(sweep("long", "sleep 30"))
	m, closeM := e.manager(t, cfg, fastPolicy())
	m.StartRun("long", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "long", "run in flight", outcomesAre(OutcomeRunning))
	closeM()

	m2, _ := e.manager(t, cfg, fastPolicy())
	r := m2.Runs("long")
	if len(r) != 1 || r[0].Outcome != OutcomeInterrupted || r[0].EndedAt == nil {
		t.Errorf("after a clean shutdown: %+v, want one interrupted run with an end", r)
	}
}

// TestCrashMidRunIsReconciledInterrupted: a record a crashed daemon left
// running is interrupted on the next boot, with its end left unknown and a
// line appended to its log, and the next run does not reuse its id.
func TestCrashMidRunIsReconciledInterrupted(t *testing.T) {
	e := newRunsEnv(t)
	started := time.Now().Add(-time.Minute).UTC().Truncate(time.Second)
	state := fmt.Sprintf(`{"version":1,"harnesses":{},"runs":{"sweep":{"last_run_id":4,"runs":[`+
		`{"run_id":4,"trigger":"schedule","outcome":"running","started_at":%q}]}}}`, started.Format(time.RFC3339))
	if err := os.WriteFile(e.state, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(e.jobs, "sweep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.jobs, "sweep", "4.log"), []byte("partial output\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	m, _ := e.manager(t, sweepCfg(sweep("sweep", "exit 0")), fastPolicy())
	rs := m.Runs("sweep")
	if len(rs) != 1 || rs[0].Outcome != OutcomeInterrupted || rs[0].EndedAt != nil || !rs[0].StartedAt.Equal(started) {
		t.Fatalf("reconciled = %+v, want run 4 interrupted with no end", rs)
	}
	if log := readText(t, m.RunLogPath("sweep", 4)); !strings.Contains(log, "partial output") || !strings.Contains(log, "run interrupted") {
		t.Errorf("reconciled log:\n%s", log)
	}

	m.StartRun("sweep", RunRequest{Trigger: TriggerSchedule})
	rs = waitRuns(t, m, "sweep", "next run finishes", outcomesAre(OutcomeInterrupted, OutcomeSuccess))
	if rs[1].RunID != 5 {
		t.Errorf("next run id = %d, want 5", rs[1].RunID)
	}
}

// TestRunIDsSurviveRestart: run ids keep counting across daemons.
func TestRunIDsSurviveRestart(t *testing.T) {
	e := newRunsEnv(t)
	cfg := sweepCfg(sweep("sweep", "exit 0"))
	m, closeM := e.manager(t, cfg, fastPolicy())
	m.StartRun("sweep", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "sweep", "run 1", outcomesAre(OutcomeSuccess))
	m.StartRun("sweep", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "sweep", "run 2", outcomesAre(OutcomeSuccess, OutcomeSuccess))
	closeM()

	m2, _ := e.manager(t, cfg, fastPolicy())
	m2.StartRun("sweep", RunRequest{Trigger: TriggerSchedule})
	rs := waitRuns(t, m2, "sweep", "run 3", outcomesAre(OutcomeSuccess, OutcomeSuccess, OutcomeSuccess))
	if ids := []int{rs[0].RunID, rs[1].RunID, rs[2].RunID}; !slices.Equal(ids, []int{1, 2, 3}) {
		t.Errorf("run ids = %v, want [1 2 3]", ids)
	}
}

// TestRunIDFloorsAtLogsOnDisk: with the history gone, the next id still
// clears every log on disk, so no log is ever reused.
func TestRunIDFloorsAtLogsOnDisk(t *testing.T) {
	e := newRunsEnv(t)
	if err := os.MkdirAll(filepath.Join(e.jobs, "sweep"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(e.jobs, "sweep", "7.log"), []byte("from a lost history\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, _ := e.manager(t, sweepCfg(sweep("sweep", "exit 0")), fastPolicy())
	m.StartRun("sweep", RunRequest{Trigger: TriggerSchedule})
	rs := waitRuns(t, m, "sweep", "run finishes", outcomesAre(OutcomeSuccess))
	if rs[0].RunID != 8 {
		t.Errorf("run id = %d, want 8 (above the 7.log on disk)", rs[0].RunID)
	}
}

// TestKeepRunsPrunesRecordsAndLogs: history is bounded, and a record falling
// out takes its log with it — as does a log no record refers to.
func TestKeepRunsPrunesRecordsAndLogs(t *testing.T) {
	e := newRunsEnv(t)
	h := sweep("sweep", "echo out; exit 0")
	h.KeepRuns = 2
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())
	if err := os.MkdirAll(filepath.Join(e.jobs, "sweep"), 0o700); err != nil {
		t.Fatal(err)
	}

	for i := 1; i <= 4; i++ {
		m.StartRun("sweep", RunRequest{Trigger: TriggerSchedule})
		waitFor(t, 5*time.Second, fmt.Sprintf("run %d finishes", i), func() bool {
			rs := m.Runs("sweep")
			return len(rs) > 0 && rs[len(rs)-1].RunID == i && rs[len(rs)-1].Outcome == OutcomeSuccess
		})
	}
	rs := m.Runs("sweep")
	if len(rs) != 2 || rs[0].RunID != 3 || rs[1].RunID != 4 {
		t.Fatalf("history = %+v, want runs 3 and 4", rs)
	}
	entries, err := os.ReadDir(filepath.Join(e.jobs, "sweep"))
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, entry := range entries {
		files = append(files, entry.Name())
	}
	if !slices.Equal(files, []string{"3.log", "4.log"}) {
		t.Errorf("run logs on disk = %v, want [3.log 4.log]", files)
	}
}

// TestPruneKeepsRunInFlight: a history bound smaller than the decisions made
// during one run never drops that run, or its open log.
func TestPruneKeepsRunInFlight(t *testing.T) {
	e := newRunsEnv(t)
	h := sweep("sweep", "sleep 30")
	h.KeepRuns = 1
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	m.StartRun("sweep", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "sweep", "run in flight", outcomesAre(OutcomeRunning))
	m.StartRun("sweep", RunRequest{Trigger: TriggerSchedule})
	m.StartRun("sweep", RunRequest{Trigger: TriggerSchedule})

	rs := m.Runs("sweep")
	if len(rs) == 0 || rs[0].RunID != 1 || rs[0].Outcome != OutcomeRunning {
		t.Errorf("history = %+v, want run 1 still in flight", rs)
	}
	if _, err := os.Stat(m.RunLogPath("sweep", 1)); err != nil {
		t.Errorf("in-flight run's log was pruned: %v", err)
	}
	m.Stop("sweep")
}

// TestRecordMissed: the scheduler's missed-window seam lands as a missed
// record naming the windows.
func TestRecordMissed(t *testing.T) {
	e := newRunsEnv(t)
	m, _ := e.manager(t, sweepCfg(sweep("sweep", "exit 0")), fastPolicy())
	first := time.Date(2026, 9, 11, 2, 0, 0, 0, time.UTC)
	last := time.Date(2026, 9, 11, 6, 0, 0, 0, time.UTC)
	detected := time.Date(2026, 9, 11, 6, 30, 0, 0, time.UTC)

	if err := m.RecordMissed("sweep", first, last, 5, detected); err != nil {
		t.Fatal(err)
	}
	rs := m.Runs("sweep")
	if len(rs) != 1 {
		t.Fatalf("history = %+v", rs)
	}
	r := rs[0]
	if r.Outcome != OutcomeMissed || r.Trigger != TriggerSchedule || r.Windows != 5 || r.ExitCode != nil ||
		!r.FirstWindow.Equal(first) || !r.Window.Equal(last) || !r.StartedAt.Equal(detected) {
		t.Errorf("missed record = %+v", r)
	}
	if err := m.RecordMissed("ghost", first, last, 1, detected); err == nil {
		t.Error("recording a miss for an unknown harness succeeded")
	}
}

// TestMalformedHistoryKeepsRegistry: a history that does not decode costs that
// harness its records, never the rest of state.json.
func TestMalformedHistoryKeepsRegistry(t *testing.T) {
	e := newRunsEnv(t)
	state := `{"version":1,"active_profile":"default","harnesses":{"good":{"enabled":false,"state":"stopped","restart_count":7,"last_exit_code":0,"flapping":false}},` +
		`"runs":{"broken":"not a history","good":{"last_run_id":2,"runs":[{"run_id":2,"trigger":"manual","outcome":"success","started_at":"2026-09-11T03:00:00Z"}]}}}`
	if err := os.WriteFile(e.state, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	m, _ := e.manager(t, sweepCfg(sweep("good", "exit 0"), sweep("broken", "exit 0")), fastPolicy())

	if rs := m.Runs("good"); len(rs) != 1 || rs[0].RunID != 2 {
		t.Errorf("good history = %+v", rs)
	}
	if rs := m.Runs("broken"); len(rs) != 0 {
		t.Errorf("broken history = %+v, want none", rs)
	}
	if m.ActiveProfile() != "default" {
		t.Errorf("active profile = %q: the malformed history cost the registry", m.ActiveProfile())
	}
	if snap, _ := m.Snapshot("good"); snap.RestartCount != 7 {
		t.Errorf("restart count = %d: the malformed history cost the registry", snap.RestartCount)
	}
}

// TestMalformedStateFileIsKept: a state file that does not parse is copied
// aside before the daemon's next save replaces it.
func TestMalformedStateFileIsKept(t *testing.T) {
	e := newRunsEnv(t)
	body := []byte(`{"version":1,"harnesses":{`)
	if err := os.WriteFile(e.state, body, 0o600); err != nil {
		t.Fatal(err)
	}
	m := NewManager(sweepCfg(sweep("sweep", "exit 0")), ManagerOptions{Policy: fastPolicy(), StatePath: e.state, LogDir: e.logs})
	t.Cleanup(m.Close)

	err := m.Restore()
	if err == nil || !strings.Contains(err.Error(), "kept a copy at") {
		t.Fatalf("Restore() = %v, want a malformed-state error naming the copy", err)
	}
	copies, _ := filepath.Glob(e.state + ".malformed-*")
	if len(copies) != 1 {
		t.Fatalf("copies = %v, want one", copies)
	}
	if got := readText(t, copies[0]); got != string(body) {
		t.Errorf("copy = %q, want the original bytes", got)
	}
}

// TestPreRunHistoryStateFileLoads: a state.json written before run history
// existed — the shape tars and kitt carry today — loads, and starts a history
// from run 1.
func TestPreRunHistoryStateFileLoads(t *testing.T) {
	e := newRunsEnv(t)
	state := `{
  "version": 1,
  "active_profile": "default",
  "harnesses": {
    "blog-sweep": {"enabled": false, "state": "stopped", "restart_count": 0, "last_exit_code": 0, "flapping": false, "created": "2026-08-01T10:00:00Z"},
    "crush-signal": {"enabled": true, "state": "running", "restart_count": 2, "last_exit_code": 1, "last_exit_at": "2026-09-10T09:00:00Z", "flapping": false, "created": "2026-08-01T10:00:00Z", "last_started": "2026-09-10T09:00:05Z"}
  }
}`
	if err := os.WriteFile(e.state, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
	signal := shHarness("crush-signal", "sleep 30", 0)
	cfg := sweepCfg(sweep("blog-sweep", "exit 0"), signal)
	m, _ := e.manager(t, cfg, fastPolicy())

	if rs := m.Runs("blog-sweep"); len(rs) != 0 {
		t.Errorf("history from a pre-history state file = %+v", rs)
	}
	if snap, _ := m.Snapshot("crush-signal"); snap.RestartCount != 2 || !snap.Enabled {
		t.Errorf("crush-signal restored as %+v", snap)
	}
	m.StartRun("blog-sweep", RunRequest{Trigger: TriggerSchedule})
	if rs := waitRuns(t, m, "blog-sweep", "first run", outcomesAre(OutcomeSuccess)); rs[0].RunID != 1 {
		t.Errorf("first run id = %d, want 1", rs[0].RunID)
	}
	if _, err := loadState(e.state); err != nil {
		t.Errorf("state.json written with run history does not reload: %v", err)
	}
}

// TestRunHistoryCarriesNoSecrets: nothing from a harness's env_file reaches
// its run records or state.json (ADR-0008).
func TestRunHistoryCarriesNoSecrets(t *testing.T) {
	e := newRunsEnv(t)
	envFile := filepath.Join(e.dir, "secrets.env")
	const secret = "hunter2-must-not-persist"
	if err := os.WriteFile(envFile, []byte("HARNESS_TEST_SECRET="+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	h := sweep("sweep", `test "$HARNESS_TEST_SECRET" = "`+secret+`"`)
	h.EnvFile = envFile
	m, closeM := e.manager(t, sweepCfg(h), fastPolicy())

	m.StartRun("sweep", RunRequest{Trigger: TriggerSchedule})
	rs := waitRuns(t, m, "sweep", "run finishes", func(rs []RunRecord) bool {
		return len(rs) == 1 && rs[0].Outcome != OutcomeRunning
	})
	if rs[0].Outcome != OutcomeSuccess {
		t.Fatalf("the run did not see its env_file: %+v", rs[0])
	}
	closeM()

	records, _ := json.Marshal(rs)
	for what, body := range map[string]string{"run records": string(records), "state.json": readText(t, e.state)} {
		if strings.Contains(body, secret) {
			t.Errorf("%s carry the env_file secret", what)
		}
	}
}

// closeTally counts run log files opened and closed.
type closeTally struct {
	mu             sync.Mutex
	opened, closed int
}

type tallyCloser struct {
	io.WriteCloser
	tally *closeTally
}

func (c tallyCloser) Close() error {
	c.tally.mu.Lock()
	c.tally.closed++
	c.tally.mu.Unlock()
	return c.WriteCloser.Close()
}

// TestRunLogClosedOnEveryExitPath: every run log opened is closed, whichever
// way its run ends — success, failure, timeout, replace, cancel, and shutdown.
func TestRunLogClosedOnEveryExitPath(t *testing.T) {
	tally := &closeTally{}
	orig := openRunLog
	t.Cleanup(func() { openRunLog = orig })
	openRunLog = func(path string) (io.WriteCloser, error) {
		f, err := orig(path)
		if err != nil {
			return nil, err
		}
		tally.mu.Lock()
		tally.opened++
		tally.mu.Unlock()
		return tallyCloser{WriteCloser: f, tally: tally}, nil
	}

	e := newRunsEnv(t)
	timeout := sweep("timeout", "sleep 30")
	timeout.Timeout = 200 * time.Millisecond
	replace := sweep("replace", "sleep 30")
	replace.OnOverlap = core.OverlapReplace
	m, closeM := e.manager(t, sweepCfg(
		sweep("success", "exit 0"), sweep("failure", "exit 1"), timeout, replace,
		sweep("cancel", "sleep 30"), sweep("shutdown", "sleep 30"),
	), fastPolicy())

	for _, name := range []string{"success", "failure", "timeout", "replace", "cancel", "shutdown"} {
		m.StartRun(name, RunRequest{Trigger: TriggerSchedule})
	}
	waitRuns(t, m, "success", "success", outcomesAre(OutcomeSuccess))
	waitRuns(t, m, "failure", "failure", outcomesAre(OutcomeFailed))
	waitRuns(t, m, "timeout", "timeout", outcomesAre(OutcomeTimedOut))
	m.StartRun("replace", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "replace", "replace", outcomesAre(OutcomeReplaced, OutcomeRunning))
	m.Stop("cancel")
	waitRuns(t, m, "cancel", "cancel", outcomesAre(OutcomeCancelled))
	closeM() // interrupts "shutdown" and the replacement

	tally.mu.Lock()
	defer tally.mu.Unlock()
	if tally.opened != 7 || tally.closed != tally.opened {
		t.Errorf("run logs opened %d, closed %d; want 7 of each", tally.opened, tally.closed)
	}
}

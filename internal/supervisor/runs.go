package supervisor

// Run History
//
// A scheduled harness does its work in discrete runs, and the question an
// operator asks is about one of them: did last night's run pass, and what did
// it print. ADR-0007's single rotating log and single last exit code cannot
// answer that, so every run of a scheduled harness gets a record and a log file
// of its own.
//
// A record is what the daemon DECIDED, not only what executed. A firing the
// overlap policy dropped, a window nobody was awake for, and a run the daemon
// died under all leave records, so "it never fired" is as visible as "it ran
// and failed". This file is the supervisor's half: the actor loop opens and
// closes records, because it is the one goroutine that sees a spawn, an exit, a
// timeout and a stop in order. The Manager's half (manager_runs.go) allocates
// run ids, bounds each history to keep_runs, deletes the logs of records that
// fall out of it, and persists the lot in state.json.
//
// Governing: ADR-0007 (extended with per-run records and logs), ADR-0008
// (records carry outcomes, times and exit codes — never environment, prompt or
// output), ADR-0013; SPEC-0008 REQ "Run History", REQ "Per-Run Logs", REQ "Run
// Timeout", REQ "Overlap Policy".
//
// @joestump-agent 09/11/2026 - Added for issue #119.

import (
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	clog "github.com/charmbracelet/log"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// RunTrigger names what started a run.
type RunTrigger string

const (
	// TriggerSchedule is a firing that came due on time.
	TriggerSchedule RunTrigger = "schedule"
	// TriggerManual is an operator start or restart.
	TriggerManual RunTrigger = "manual"
	// TriggerCatchUp is the single run `catch_up = true` gets for windows that
	// elapsed while nobody was evaluating.
	TriggerCatchUp RunTrigger = "catch_up"
)

// RunOutcome is how a run ended — or, for the outcomes that start no process,
// what the daemon decided instead.
type RunOutcome string

const (
	// OutcomeRunning marks the run in flight. It is never final: a record
	// still reading running when the daemon boots is reconciled to
	// OutcomeInterrupted.
	OutcomeRunning RunOutcome = "running"
	// OutcomeSuccess is a process that exited 0 on its own.
	OutcomeSuccess RunOutcome = "success"
	// OutcomeFailed is a process that exited non-zero on its own, or never
	// spawned.
	OutcomeFailed RunOutcome = "failed"
	// OutcomeTimedOut is a process killed for outliving `timeout`.
	OutcomeTimedOut RunOutcome = "timed_out"
	// OutcomeSkipped is a firing dropped because a run was already in flight
	// (or the harness was mid-stop). No process.
	OutcomeSkipped RunOutcome = "skipped"
	// OutcomeReplaced is a run stopped so another could start: on_overlap =
	// "replace", or an operator restart.
	OutcomeReplaced RunOutcome = "replaced"
	// OutcomeMissed is one or more schedule windows that elapsed while nobody
	// was evaluating, with catch_up = false. No process.
	OutcomeMissed RunOutcome = "missed"
	// OutcomeCancelled is a run (or a queued firing) ended by an operator stop.
	OutcomeCancelled RunOutcome = "cancelled"
	// OutcomeInterrupted is a run the daemon went away under: stopped by a
	// clean daemon shutdown, or found still "running" on the next boot after a
	// crash. A queued firing dropped by a shutdown is recorded the same way.
	OutcomeInterrupted RunOutcome = "interrupted"
)

// RunRecord is one entry in a scheduled harness's run history.
type RunRecord struct {
	// RunID is monotonic per harness and never reused, across restarts too.
	RunID   int        `json:"run_id"`
	Trigger RunTrigger `json:"trigger"`
	Outcome RunOutcome `json:"outcome"`
	// StartedAt is when the process was started — or, for a record with no
	// process (skipped, missed), when the daemon made the decision.
	StartedAt time.Time `json:"started_at"`
	// EndedAt is unset while running, and for a run reconciled as interrupted
	// after a crash, whose real end is unknown.
	EndedAt *time.Time `json:"ended_at,omitempty"`
	// ExitCode is set only when a process was reaped (-1 if signalled).
	ExitCode *int `json:"exit_code,omitempty"`
	// Window is the schedule window a record honors: the due window of a
	// scheduled run, the latest missed window of a catch-up run or a missed
	// record. Unset for a manual run.
	Window *time.Time `json:"window,omitempty"`
	// FirstWindow is the earliest window a missed record covers.
	FirstWindow *time.Time `json:"first_window,omitempty"`
	// Windows counts the windows a missed record or a catch-up run covers.
	Windows int `json:"windows,omitempty"`
}

// RunRequest asks for a run of a scheduled harness.
type RunRequest struct {
	Trigger RunTrigger
	// Window is the schedule window being honored (zero for manual).
	Window time.Time
	// Windows counts the missed windows a catch-up run stands in for.
	Windows int
}

// RunJournal stores run records. The Manager implements it; a supervisor with
// no journal still enforces timeouts and overlap but keeps no history.
type RunJournal interface {
	// OpenRun allocates rec's run id, records it, persists, and returns the
	// record with its id and the path its log should be written to.
	OpenRun(name string, rec RunRecord) (RunRecord, string, error)
	// CloseRun replaces the record with rec's run id and persists.
	CloseRun(name string, rec RunRecord) error
	// AppendRun records a decision that started no process and persists.
	AppendRun(name string, rec RunRecord) (RunRecord, error)
}

// activeRun is the loop-owned state of the run in flight.
type activeRun struct {
	rec   RunRecord
	gen   uint64         // the process generation this run owns
	file  io.WriteCloser // per-run log; nil when none could be opened
	evlog *clog.Logger   // lifecycle lines into file
	timer *time.Timer    // timeout; nil when unlimited
}

// decisionRecord builds the record of a decision that starts no process.
func decisionRecord(req RunRequest, outcome RunOutcome, now time.Time) RunRecord {
	rec := RunRecord{Trigger: req.Trigger, Outcome: outcome, StartedAt: now, EndedAt: &now, Windows: req.Windows}
	if !req.Window.IsZero() {
		w := req.Window
		rec.Window = &w
	}
	return rec
}

// startProcess brings the harness up from idle: as a recorded run if it is
// scheduled, as a plain start otherwise.
func (s *Supervisor) startProcess(req RunRequest) {
	if s.harness.Schedule != "" {
		s.beginRun(req)
		return
	}
	s.beginStart()
}

// startRun handles a firing. From idle it starts a run; with a run in flight
// it applies the harness's overlap policy.
//
// Deciding here, on the actor loop, is what makes overlap exact: the check and
// the start cannot be separated by another start, as a read-the-snapshot then
// call-Start caller can be.
func (s *Supervisor) startRun(req RunRequest) {
	if !s.hasProcess() {
		s.clearFailLatch()
		s.startProcess(req)
		return
	}
	if s.harness.Schedule == "" {
		return // no overlap policy without a schedule: already up is up
	}
	switch s.harness.OnOverlap {
	case core.OverlapQueue:
		if s.queued == nil {
			q := req
			s.queued = &q
			s.logEvent("run queued", "trigger", string(req.Trigger))
			return
		}
		s.recordDecision(req, OutcomeSkipped)
	case core.OverlapReplace:
		s.gracefulStop()
		s.finishRun(OutcomeReplaced, &s.lastExitCode)
		s.clearFailLatch()
		s.beginRun(req)
	default:
		s.recordDecision(req, OutcomeSkipped)
	}
}

// beginRun opens a record for req, opens its log, starts the process, and arms
// the timeout.
func (s *Supervisor) beginRun(req RunRequest) {
	s.ensureLog()
	rec := decisionRecord(req, OutcomeRunning, time.Now())
	rec.EndedAt = nil
	run := &activeRun{}
	if s.journal != nil {
		opened, path, err := s.journal.OpenRun(s.harness.Name, rec)
		rec = opened
		if err != nil {
			s.logEvent("run history not saved", "run_id", rec.RunID, "err", err.Error())
		}
		if path != "" {
			if f, err := openRunLog(path); err != nil {
				s.logEvent("run log unavailable", "run_id", rec.RunID, "err", err.Error())
			} else {
				run.file = f
				run.evlog = newEventLogger(f)
			}
		}
	}
	run.rec = rec
	s.run = run
	s.logEvent("run started", "run_id", rec.RunID, "trigger", string(rec.Trigger))

	s.beginStart()
	// A spawn failure has already finished the run inside beginStart.
	if s.run == run && s.hasProcess() {
		run.gen = s.gen
		s.armRunTimeout(run)
	}
}

// openRunLog creates a run's log file. Private to the daemon's user (ADR-0008):
// it holds whatever the agent printed. A variable so a test can observe that
// every file it opens is closed.
var openRunLog = func(path string) (io.WriteCloser, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	return os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// armRunTimeout schedules the run's timeout, delivered to the loop.
func (s *Supervisor) armRunTimeout(run *activeRun) {
	d := s.harness.Timeout
	if d <= 0 {
		return
	}
	gen := run.gen
	run.timer = time.AfterFunc(d, func() {
		select {
		case s.timeoutCh <- gen:
		case <-s.done:
		}
	})
}

// handleRunTimeout ends a run that outlived its timeout: SIGTERM, SIGKILL after
// the stop grace, and a timed_out record. The harness lands in failed, the same
// state a failing run leaves it in, rather than the stopped of a graceful stop.
func (s *Supervisor) handleRunTimeout(gen uint64) {
	run := s.run
	if run == nil || run.gen != gen || s.gen != gen || !s.hasProcess() {
		return // the run already ended; a stale timer
	}
	s.logEvent("run timed out", "run_id", run.rec.RunID, "timeout", s.harness.Timeout.String())
	code, ok := s.killProcess()
	now := time.Now()
	if ok {
		s.lastExitCode = code
	}
	s.lastExitAt = now
	// The same exit line and event a natural exit produces, so log readers
	// (run spans, `harness logs`) see this run end.
	s.logEvent("exited", "code", s.lastExitCode)
	if s.bus != nil {
		s.bus.Publish(Event{Kind: EventExited, Name: s.harness.Name, Time: now, Code: s.lastExitCode})
	}
	s.resetCrashState()
	if s.state == core.StateRunning {
		s.transition(core.StateDegraded) // running→failed is not a legal edge
	}
	s.transition(core.StateFailed)
	var exit *int
	if ok {
		exit = &code
	}
	s.finishRun(OutcomeTimedOut, exit)
}

// killProcess ends the live process group the way a graceful stop does —
// SIGTERM, then SIGKILL after StopGrace — and reaps it, leaving the state
// transition to the caller. ok is false when the exit consumed belonged to an
// earlier generation.
func (s *Supervisor) killProcess() (code int, ok bool) {
	proc, gen := s.proc, s.gen
	proc.signalGroup(syscall.SIGTERM)
	var ex exitResult
	select {
	case ex = <-s.exitCh:
	case <-time.After(s.policy.StopGrace):
		proc.signalGroup(syscall.SIGKILL)
		ex = <-s.exitCh
	}
	s.reapProcess()
	return ex.code, ex.gen == gen
}

// finishRun closes the run in flight with outcome, then starts a queued firing
// if the run ended on its own terms.
//
// Every way a run ends comes through here — natural exit, spawn failure,
// timeout, replace, operator stop and restart, daemon shutdown — so this is the
// one place the run's log file is closed.
func (s *Supervisor) finishRun(outcome RunOutcome, code *int) {
	run := s.run
	if run == nil {
		return
	}
	s.run = nil
	if run.timer != nil {
		run.timer.Stop()
	}
	now := time.Now()
	run.rec.Outcome = outcome
	run.rec.EndedAt = &now
	if code != nil {
		c := *code
		run.rec.ExitCode = &c
	}
	kv := []any{"run_id", run.rec.RunID, "outcome", string(outcome)}
	if code != nil {
		kv = append(kv, "exit_code", *code)
	}
	s.logEvent("run finished", kv...)
	if run.file != nil {
		// The PTY reader lands the run's final screenful after the exit is
		// processed (#279); wait for it so the log holds the whole run and the
		// finish line comes last.
		s.awaitReader()
		run.evlog.Info("run finished", kv...)
		_ = run.file.Close()
	}
	if s.journal != nil {
		if err := s.journal.CloseRun(s.harness.Name, run.rec); err != nil {
			s.logEvent("run history not saved", "run_id", run.rec.RunID, "err", err.Error())
		}
	}
	switch outcome {
	case OutcomeCancelled, OutcomeInterrupted:
		return // the caller has already dealt with the queue
	case OutcomeReplaced:
		return // a replacement starts next; any queued firing waits behind it
	}
	if q := s.queued; q != nil {
		s.queued = nil
		s.clearFailLatch()
		s.beginRun(*q)
	}
}

// dropQueued records a held firing that will now never start.
func (s *Supervisor) dropQueued(outcome RunOutcome) {
	if s.queued == nil {
		return
	}
	q := *s.queued
	s.queued = nil
	s.recordDecision(q, outcome)
}

// recordDecision records a firing that starts no process.
func (s *Supervisor) recordDecision(req RunRequest, outcome RunOutcome) {
	rec := decisionRecord(req, outcome, time.Now())
	if s.journal != nil {
		appended, err := s.journal.AppendRun(s.harness.Name, rec)
		rec = appended
		if err != nil {
			s.logEvent("run history not saved", "run_id", rec.RunID, "err", err.Error())
		}
	}
	s.logEvent("run "+string(outcome), "run_id", rec.RunID, "trigger", string(req.Trigger))
}

// awaitReader waits, bounded, for the PTY reader of the latest spawn to land
// its final flush. A wedged reader must not wedge the actor loop.
func (s *Supervisor) awaitReader() {
	if s.readerDone == nil {
		return
	}
	select {
	case <-s.readerDone:
	case <-time.After(2 * time.Second):
	}
}

// historyOut is where the sanitized output history of the next spawn goes: the
// harness's rotating log, the run's own log, or both.
func (s *Supervisor) historyOut() io.Writer {
	var runLog io.Writer
	if s.run != nil && s.run.file != nil {
		runLog = s.run.file
	}
	switch {
	case s.log != nil && runLog != nil:
		return teeWriter{primary: s.log, secondary: runLog}
	case s.log != nil:
		return s.log
	case runLog != nil:
		return runLog
	}
	return nil
}

// teeWriter writes to both writers, reporting only the primary's result: a run
// log that cannot be written must not cost the harness its main log.
type teeWriter struct{ primary, secondary io.Writer }

func (t teeWriter) Write(p []byte) (int, error) {
	_, _ = t.secondary.Write(p)
	return t.primary.Write(p)
}

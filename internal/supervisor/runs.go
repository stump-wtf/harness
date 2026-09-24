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
//
// @joestump-agent 09/11/2026 - Review of PR #310: killProcess now SIGKILLs what
// is left of the process group after the leader exits, so a timeout, replace or
// stop cannot orphan an MCP child that ignores SIGTERM and SIGHUP.

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	clog "github.com/charmbracelet/log"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/trigger"
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
	// TriggerChannel is a `notifications/claude/channel` from a channel
	// source (SPEC-0014 REQ "Run Record Fields").
	TriggerChannel RunTrigger = "channel"
	// TriggerWebhook is a verified delivery to a webhook source's route.
	TriggerWebhook RunTrigger = "webhook"
)

// RunReason says why a decision that started no process was taken. It
// qualifies OutcomeSkipped, whose bare presence in a history answers "did it
// run?" but not "why not?" — and the three answers want different responses
// from an operator.
// Governing: SPEC-0014 REQ "Run Record Fields", REQ "Overlap Skip Coalescing".
type RunReason string

const (
	// ReasonOverlap: a run was already in flight and the overlap policy
	// dropped this firing.
	ReasonOverlap RunReason = "overlap"
	// ReasonStopping: the harness was mid-stop, so the firing had nowhere to
	// go.
	ReasonStopping RunReason = "stopping"
	// ReasonOutsideHours: the firing arrived outside the harness's
	// operating_hours window. Defined here with its siblings so the
	// vocabulary is in one place; the gate that produces it is SPEC-0014 REQ
	// "Operating Hours On Triggered Harnesses", not yet implemented.
	ReasonOutsideHours RunReason = "outside_hours"
	// ReasonTemplateUnresolved: a required argv template value was absent
	// for this run, so nothing was exec'd. The record names the path in
	// MissingPath, never a value.
	// Governing: SPEC-0017 REQ-11 "Rendering", REQ-17 (the SPEC-0008 and
	// SPEC-0014 skip-reason amendment).
	ReasonTemplateUnresolved RunReason = "template_unresolved"
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
	// Reason says WHY a decision that started no process was taken. Present
	// only on a skipped record.
	// Governing: SPEC-0014 REQ "Run Record Fields".
	Reason RunReason `json:"reason,omitempty"`
	// MissingPath is the template path whose value was absent, set only when
	// Reason is ReasonTemplateUnresolved. A path's NAME ("run.source"), never
	// a value: nothing rendered is persisted (SPEC-0017 REQ-11).
	MissingPath string `json:"missing_path,omitempty"`
	// Coalesced counts the firings one skipped record covers. It starts at 1
	// and increments once per further skip that matches the same open key,
	// so a burst of 200 firings during one run leaves one record rather than
	// 199 that flush `keep_runs` and take the real history with them.
	// Governing: SPEC-0014 REQ "Overlap Skip Coalescing".
	Coalesced int `json:"coalesced,omitempty"`
	// Source is the trigger source reference that caused this decision, e.g.
	// "webhook.gitea-pr". Unset for a schedule, catch-up or bare manual run.
	// Governing: SPEC-0014 REQ "Run Record Fields".
	Source string `json:"source,omitempty"`
	// EventID is the event's ID when the decision carried one: the sender's
	// delivery ID, or a daemon-generated one for a scheme with no delivery
	// header. It is an IDENTIFIER, never a payload — a record carries no byte
	// of an event body, no header value and no credential (ADR-0008).
	// Governing: SPEC-0014 REQ "Run Record Fields".
	EventID string `json:"event_id,omitempty"`
}

// RunRequest asks for a run of a triggered harness.
type RunRequest struct {
	Trigger RunTrigger
	// Window is the schedule window being honored (zero for manual).
	Window time.Time
	// Windows counts the missed windows a catch-up run stands in for.
	Windows int
	// Source is the trigger source reference behind this request, when one
	// caused it. Governing: SPEC-0014 REQ "Run Record Fields".
	Source string
	// Event is the event to deliver to the run, or nil. It rides the request
	// rather than being written when the event arrives, because a firing held
	// under `on_overlap = "queue"` must not touch disk until it actually
	// starts: a skipped or coalesced firing that had already written a file
	// would leave an event file with no run, and a queued one needs the run
	// id it does not have yet to name its file.
	// Governing: SPEC-0014 REQ "Event Delivery To The Run".
	Event *trigger.Envelope
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
	// CoalesceRun increments Coalesced on the stored record with this run id
	// and persists, returning the updated record. It deliberately has no
	// event: SPEC-0014 REQ "Overlap Skip Coalescing" says later increments
	// emit none, because the point of coalescing is that a burst produces one
	// notification rather than 199.
	//
	// A record that no longer exists is errNoRunToCoalesce; any other error
	// means the increment landed but did not persist, and the returned
	// record carries it.
	CoalesceRun(name string, id int) (RunRecord, error)
}

// errNoRunToCoalesce is CoalesceRun's answer when the open record has gone
// (pruned, or a history reset) — the one case in which the skip must open a
// new record. It is distinct from a failed Save, after which the increment
// has already landed: treating that as "gone" too counted the firing twice.
var errNoRunToCoalesce = errors.New("supervisor: no run to coalesce into")

// RunDecisionKind names what StartRun did with a request.
type RunDecisionKind string

const (
	// DecisionStarted: a run started a process (on_overlap = "replace" may have
	// stopped the run in flight first).
	DecisionStarted RunDecisionKind = "started"
	// DecisionQueued: on_overlap = "queue" is holding the request; it gets a
	// record, and a run id, when it starts.
	DecisionQueued RunDecisionKind = "queued"
	// DecisionSkipped: the request was recorded skipped.
	DecisionSkipped RunDecisionKind = "skipped"
)

// RunDecision is what StartRun did with a request.
type RunDecision struct {
	Kind RunDecisionKind
	// Run is the started run's record as opened, or the skipped record; zero
	// for a queued request.
	Run RunRecord
}

// activeRun is the loop-owned state of the run in flight.
type activeRun struct {
	rec   RunRecord
	gen   uint64         // the process generation this run owns
	file  io.WriteCloser // per-run log; nil when none could be opened
	evlog *clog.Logger   // lifecycle lines into file
	timer *time.Timer    // timeout; nil when unlimited
	// eventFile is the absolute path of the run's event file, or "" when the
	// run carries no event (or the harness has no per-run log directory to
	// put one in). It is read back out by runEnv, so a restart of the same
	// run spawns with the same HARNESS_EVENT_FILE.
	eventFile string
}

// decisionRecord builds the record of a decision that starts no process.
func decisionRecord(req RunRequest, outcome RunOutcome, now time.Time) RunRecord {
	rec := RunRecord{Trigger: req.Trigger, Outcome: outcome, StartedAt: now, EndedAt: &now, Windows: req.Windows, Source: req.Source}
	if !req.Window.IsZero() {
		w := req.Window
		rec.Window = &w
	}
	if req.Event != nil {
		rec.EventID = req.Event.EventID
	}
	return rec
}

// startProcess brings the harness up from idle: as a recorded run if it is
// scheduled, as a plain start otherwise. It returns the run's record as opened
// (zero for an unscheduled harness).
func (s *Supervisor) startProcess(req RunRequest) RunRecord {
	// Triggered, not scheduled: SPEC-0014 extends the run machinery to event
	// sources, so a webhook-only harness gets a record, a log, `timeout`,
	// `on_overlap` and `keep_runs` exactly as a cron one-shot does. A bare
	// `Schedule != ""` here would read a triggered harness as an ordinary
	// resident one and give its firings none of that.
	if s.harness.Triggered() {
		return s.beginRun(req)
	}
	s.beginStart()
	return RunRecord{}
}

// startRun handles a firing. From idle it starts a run; with a run in flight
// it applies the harness's overlap policy.
//
// Deciding here, on the actor loop, is what makes overlap exact: the check and
// the start cannot be separated by another start, as a read-the-snapshot then
// call-Start caller can be.
func (s *Supervisor) startRun(req RunRequest) RunDecision {
	if !s.hasProcess() {
		s.clearFailLatch()
		return RunDecision{Kind: DecisionStarted, Run: s.startProcess(req)}
	}
	if !s.harness.Triggered() {
		// No overlap policy without a firing source: already up is up.
		return RunDecision{Kind: DecisionSkipped}
	}
	switch s.harness.OnOverlap {
	case core.OverlapQueue:
		if s.queued == nil {
			q := req
			s.queued = &q
			s.logEvent("run queued", "trigger", string(req.Trigger))
			return RunDecision{Kind: DecisionQueued}
		}
		return RunDecision{Kind: DecisionSkipped, Run: s.recordSkip(req, ReasonOverlap)}
	case core.OverlapReplace:
		s.gracefulStop()
		s.finishRun(OutcomeReplaced, &s.lastExitCode)
		s.clearFailLatch()
		return RunDecision{Kind: DecisionStarted, Run: s.beginRun(req)}
	default:
		return RunDecision{Kind: DecisionSkipped, Run: s.recordSkip(req, ReasonOverlap)}
	}
}

// beginRun opens a record for req, opens its log, starts the process, and arms
// the timeout.
func (s *Supervisor) beginRun(req RunRequest) RunRecord {
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
			// Written HERE, after the run id exists and before the process
			// spawns, because the spawn's environment names this path. A
			// failure to write it is logged and the run continues without an
			// event file rather than being abandoned: an agent that finds no
			// HARNESS_EVENT_FILE can still do its job, where a firing dropped
			// for a disk error is work silently lost with nothing to
			// re-deliver it.
			if req.Event != nil {
				if p, err := writeEventFile(eventPathFor(path), req.Event); err != nil {
					s.logEvent("run event not saved", "run_id", rec.RunID, "err", err.Error())
				} else {
					run.eventFile = p
				}
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
		s.publishRun(EventRunStarted, run.rec)
	}
	return rec
}

// publishRun announces a run record on the lifecycle bus (SPEC-0008 REQ
// "Lifecycle Events"). The bus drops events for a slow subscriber, so these are
// notifications; the run history is the record.
func (s *Supervisor) publishRun(kind EventKind, rec RunRecord) {
	if s.bus != nil {
		s.bus.Publish(Event{Kind: kind, Name: s.harness.Name, Time: time.Now(), Run: rec})
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

// eventPathFor turns a run's log path into its event file path. One function,
// so the write here and the prune in manager_runs cannot drift into disagreeing
// about the name — a mismatch would leave every event file on disk forever
// while `keep_runs` faithfully removed the logs beside them.
func eventPathFor(logPath string) string {
	if logPath == "" {
		return ""
	}
	return strings.TrimSuffix(logPath, ".log") + ".event.json"
}

// writeEventFile writes env to path with mode 0600 and returns the absolute
// path it can be named by.
//
// 0600 is the requirement, not a default: a webhook body is attacker-supplied
// text, and the file sits in a jobs directory an operator may well have made
// group-readable. It is written with O_EXCL — a run id is never reused, so an
// existing file at this path is a bug or a collision worth hearing about
// rather than something to overwrite.
//
// A variable so a test can observe the mode and the failure path.
// Governing: SPEC-0014 REQ "Event Delivery To The Run"; ADR-0008.
var writeEventFile = func(path string, env *trigger.Envelope) (string, error) {
	if path == "" || env == nil {
		return "", nil
	}
	b, err := env.Encode()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return "", err
	}
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		// The relative path still names the file; the agent's cwd is the
		// harness workdir, so prefer being honest over inventing one.
		return path, nil
	}
	return abs, nil
}

// runEnv is the run-context environment the process in flight is spawned with.
// It is derived from the run rather than stored, so a respawn of the same run
// gets the same values.
// Governing: SPEC-0014 REQ "Event Delivery To The Run".
func (s *Supervisor) runEnv() RunEnv {
	if s.run == nil {
		return RunEnv{}
	}
	return RunEnv{
		RunID:     s.run.rec.RunID,
		Trigger:   s.run.rec.Trigger,
		Source:    s.run.rec.Source,
		EventFile: s.run.eventFile,
		StartedAt: s.run.rec.StartedAt,
	}
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
//
// The leader exiting is not the group exiting. An agent's MCP servers (uvx,
// npx, ssh) share its process group, and one that is slow on SIGTERM — or deaf
// to it and to the SIGHUP the session leader's exit raises — would otherwise
// outlive the run a timeout, replace or stop was meant to end. Whatever is left
// of the group gets the rest of the grace, then SIGKILL.
func (s *Supervisor) killProcess() (code int, ok bool) {
	proc, gen := s.proc, s.gen
	deadline := time.Now().Add(s.policy.StopGrace)
	proc.signalGroup(syscall.SIGTERM)
	var ex exitResult
	select {
	case ex = <-s.exitCh:
	case <-time.After(s.policy.StopGrace):
		proc.signalGroup(syscall.SIGKILL)
		ex = <-s.exitCh
	}
	s.reapProcess()
	proc.killStragglers(deadline)
	return ex.code, ex.gen == gen
}

// killStragglers SIGKILLs whatever remains of the child's process group once
// deadline passes, after the leader has been reaped. Members that exit on their
// own before then are left to finish. A group id is not handed to a new group
// while any member of the old one lives, so -pid reaches only this harness's
// leftovers.
func (p *process) killStragglers(deadline time.Time) {
	if p == nil || p.pid <= 1 {
		return
	}
	for syscall.Kill(-p.pid, 0) != syscall.ESRCH {
		if !time.Now().Before(deadline) {
			_ = syscall.Kill(-p.pid, syscall.SIGKILL)
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// finishRun closes the run in flight with outcome, then starts a queued firing
// if the run ended on its own terms.
//
// Every way a run ends comes through here — natural exit, spawn failure,
// timeout, replace, operator stop and restart, daemon shutdown — so this is the
// one place the run's log file is closed.
func (s *Supervisor) finishRun(outcome RunOutcome, code *int) {
	// The process these skips were "during" is over, so the next skip opens
	// a new record (REQ "Overlap Skip Coalescing": "When the run in flight
	// ends, the next skip SHALL create a new record"). Cleared BEFORE the
	// no-run return: every caller has just ended the process, and a skip
	// against a process with no run (a harness a reload made triggered while
	// it was up) must not stay open into the next run.
	clear(s.openSkips)
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
		// The PTY reader lands the run's final screenful at EOF (#279),
		// normally before the exit is even reported (wait) but after it when
		// the drain gave up and reapProcess's close ended the read; wait for
		// it so the log holds the whole run and the finish line comes last.
		s.awaitReader()
		run.evlog.Info("run finished", kv...)
		_ = run.file.Close()
	}
	if s.journal != nil {
		if err := s.journal.CloseRun(s.harness.Name, run.rec); err != nil {
			s.logEvent("run history not saved", "run_id", run.rec.RunID, "err", err.Error())
		}
	}
	s.publishRun(EventRunFinished, run.rec)
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

// recordDecision records a firing that starts no process, and returns the
// record.
func (s *Supervisor) recordDecision(req RunRequest, outcome RunOutcome) RunRecord {
	return s.recordDecisionWithReason(req, outcome, "")
}

func (s *Supervisor) recordDecisionWithReason(req RunRequest, outcome RunOutcome, reason RunReason) RunRecord {
	rec := decisionRecord(req, outcome, time.Now())
	rec.Reason = reason
	if outcome == OutcomeSkipped {
		rec.Coalesced = 1
	}
	if s.journal != nil {
		appended, err := s.journal.AppendRun(s.harness.Name, rec)
		rec = appended
		if err != nil {
			s.logEvent("run history not saved", "run_id", rec.RunID, "err", err.Error())
		}
	}
	kv := []any{"run_id", rec.RunID, "trigger", string(req.Trigger)}
	if reason != "" {
		kv = append(kv, "reason", string(reason))
	}
	s.logEvent("run "+string(outcome), kv...)
	s.publishRun(EventRunFinished, rec)
	return rec
}

// skipKey identifies the class of skip a record covers. Two skips coalesce
// only when all three agree, so a burst from one webhook and a burst from a
// second source stay distinguishable in the history — which is the whole
// reason an operator reads it.
// Governing: SPEC-0014 REQ "Overlap Skip Coalescing".
type skipKey struct {
	trigger RunTrigger
	source  string
	reason  RunReason
}

// recordSkip records a skipped firing, coalescing it into the open record for
// its class when one exists.
//
// It lives on the actor loop for the reason the overlap decision does: the two
// are one decision. A separate ingress-side debouncer would be a second
// decision-maker that can disagree with the first, and the disagreement would
// show up as a history that does not add up.
//
// The open keys are per RUN, not per harness: they are cleared when the run in
// flight ends (finishRun), so the next skip opens a new record. Without that,
// a harness skipping once a day would keep incrementing one record forever and
// the history would never show WHEN the skips happened.
// Governing: SPEC-0014 REQ "Overlap Skip Coalescing".
func (s *Supervisor) recordSkip(req RunRequest, reason RunReason) RunRecord {
	key := skipKey{trigger: req.Trigger, source: req.Source, reason: reason}
	if s.journal != nil {
		if id, open := s.openSkips[key]; open {
			rec, err := s.journal.CoalesceRun(s.harness.Name, id)
			if !errors.Is(err, errNoRunToCoalesce) {
				// No event, and no log line per firing: 200 of either is the
				// noise coalescing exists to remove. A failed save is the
				// exception worth a line — the increment landed in memory
				// but not on disk — and it stays coalesced: opening a new
				// record here would count this firing twice.
				if err != nil {
					s.logEvent("run history not saved", "run_id", rec.RunID, "err", err.Error())
				}
				return rec
			}
			// The record went away under us (pruned, or a history reset).
			// Fall through and open a new one rather than losing the skip.
			delete(s.openSkips, key)
		}
	}
	rec := s.recordDecisionWithReason(req, OutcomeSkipped, reason)
	if s.journal != nil && rec.RunID > 0 {
		if s.openSkips == nil {
			s.openSkips = map[skipKey]int{}
		}
		s.openSkips[key] = rec.RunID
	}
	return rec
}

// awaitReader waits, bounded, for the PTY reader of the latest spawn to land
// its final flush. A wedged reader must not wedge the actor loop.
func (s *Supervisor) awaitReader() {
	if s.readerDone == nil {
		return
	}
	drainReader(s.readerDone)
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

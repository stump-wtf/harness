package supervisor

// Admission: The Supervisor's Half
//
// Every process start passes admission first (SPEC-0021 REQ-4), and this file
// is where the actor loop asks. It holds the one value spawn accepts, an
// admission, and the one function that mints it, admitStart, which goes
// through the Manager's funnel (Manager.Admit, manager_admit.go): the decision and the
// run's `opened` ledger line are made there under one lock. So no start path
// reaches exec without the funnel: beginStart takes an admission, spawn
// refuses to run without one, and TestEveryStartPathIsAdmitted reads this
// package's source to prove that nothing else creates one and nothing else
// execs a process.
//
// The paths, every one funnelled here:
//
//   - a firing (schedule, catch-up, channel, webhook), `harness trigger`, a
//     queued firing and on_overlap = "replace": beginRun or admitRun;
//   - `harness start`, Autostart, a reload, use-profile, a lease and a hold's
//     release: startProcess, which is beginRun for a one-shot and
//     startResident for a resident;
//   - the restart policy's respawn after an exit: handleRestartTimer, then
//     startResident.
//
// A refusal does what the decision says, without a spawn. A one-shot firing
// is recorded skipped with the reason, coalesced like any other skip
// (SPEC-0014 REQ "Overlap Skip Coalescing"). A resident is held for the
// reason with `enabled` untouched; a respawn refused this way is neither a
// crash nor a step toward give-up, so a crash loop that spends its run budget
// ends held over-budget, never failed (REQ-5 Scenario "A crash loop spends the
// budget"). A resident refused because its ledger record could not be written
// has no hold that could wait the outage out, so it is handled as a start that
// failed: the restart policy retries it with backoff, as it would a spawn
// failure.
//
// Governing: ADR-0027; SPEC-0021 REQ-4 "Admission", REQ-5 "Run-count budget",
// REQ-14, REQ-21; design.md § "Admission is a funnel on the Manager";
// SPEC-0022 REQ-3, REQ-6 (a run's record opens before its process spawns).
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#470.

import (
	"errors"
	"time"

	"github.com/stump-wtf/harness/internal/budget"
	"github.com/stump-wtf/harness/internal/core"
)

// Admitter is the Manager's admission funnel as a supervisor reaches it: Admit
// decides req for name and, when it admits, opens rec in the run ledger under
// the same lock, so two starts can never both take the last unit of a budget.
// It runs on the caller's actor loop, so it must never call back into that
// supervisor's loop.
type Admitter interface {
	Admit(name string, req budget.AdmitRequest, rec RunRecord) AdmitResult
}

// AdmitResult is what the funnel decided.
type AdmitResult struct {
	Decision budget.Decision
	// Run is the run's record: as opened when admitted; as already recorded
	// when a ledger failure refused a start the funnel had opened a record
	// for (RunID > 0); otherwise zero, and the refusal is the caller's to
	// record.
	Run RunRecord
	// Log is an admitted one-shot's per-run log path; "" for a resident.
	Log string
	// Err is the ledger's answer for an admitted start whose `opened` line
	// is queued but not yet on disk (SPEC-0022 REQ-6). The start goes on.
	Err error
}

// admission is a start's proof that it was admitted, and what the funnel
// opened for it: the only value spawn accepts. It is minted by admitStart
// and by nothing else (TestEveryStartPathIsAdmitted).
type admission struct {
	rec RunRecord
	log string
}

// errNotAdmitted is spawn's answer to a nil admission: a start path that
// skipped the funnel. It is a programming error, surfaced as a failed spawn
// rather than an exec nobody admitted.
var errNotAdmitted = errors.New("supervisor: spawn without admission (SPEC-0021 REQ-4)")

// admitStart asks the funnel to admit a start of this harness by trigger,
// opening rec when it does. It returns the admission to start with, or nil and
// the refusal to apply. A supervisor built outside a Manager has no funnel and
// nothing budgeted, so its admission is the journal's open alone, as before
// admission existed.
func (s *Supervisor) admitStart(trigger RunTrigger, resident bool, rec RunRecord) (*admission, AdmitResult) {
	req := budget.AdmitRequest{
		Harness:  s.harness.Name,
		Resident: resident,
		Trigger:  string(trigger),
		Holds:    s.holds,
	}
	var res AdmitResult
	switch {
	case s.admitter != nil:
		res = s.admitter.Admit(s.harness.Name, req, rec)
	case s.journal != nil:
		opened, path, err := s.journal.OpenRun(s.harness.Name, rec)
		res = AdmitResult{Decision: budget.Decision{Verdict: budget.Admit}, Run: opened, Log: path, Err: err}
	default:
		res = AdmitResult{Decision: budget.Decision{Verdict: budget.Admit}, Run: rec}
	}
	if !res.Decision.Admitted() {
		return nil, res
	}
	if res.Err != nil {
		// The run goes on: its line is queued and retried in order
		// (SPEC-0022 REQ-6), and a harness with no budget does not wait on
		// the ledger (SPEC-0021 REQ-4).
		s.logEvent("run history not saved", "run_id", res.Run.RunID, "err", res.Err.Error())
	}
	return &admission{rec: res.Run, log: res.Log}, res
}

// startResident admits a resident's next process lifetime and starts it, or
// applies the refusal. The trigger is the one its start path set
// (startTrigger), consumed here.
func (s *Supervisor) startResident() RunDecision {
	trig := s.startTrigger
	s.startTrigger = ""
	if trig == "" {
		trig = TriggerManual
	}
	if s.resident != nil {
		// Every exit path closes the record, so an open one here is a path
		// that forgot to. Close it before the next one opens, rather than
		// leave it open until the next boot calls it a crash.
		s.closeResident(OutcomeInterrupted, nil, "")
	}
	s.ensureLog()
	rec := RunRecord{Kind: KindResident, Trigger: trig, Outcome: OutcomeRunning, StartedAt: time.Now()}
	if s.log != nil {
		rec.Log = s.log.path()
	}
	adm, res := s.admitStart(trig, true, rec)
	if adm == nil {
		return s.refuseResident(res)
	}
	s.beginStart(adm)
	return RunDecision{Kind: DecisionStarted, Run: adm.rec}
}

// refuseResident applies a refused resident start: held for the decision's
// reason, or, for a refusal no hold can wait out, a start that failed.
func (s *Supervisor) refuseResident(res AdmitResult) RunDecision {
	d := res.Decision
	out := RunDecision{Kind: DecisionSkipped, Run: res.Run, Refused: d.Err()}
	reason := d.Reason.HoldReason()
	if !reason.Valid() {
		// ledger_unavailable: the start could not be recorded, so it did not
		// happen. Hand it to the restart policy as a spawn failure (from
		// starting, the state a spawn failure is reported in), which retries
		// with backoff while the ledger recovers.
		s.logEvent("start refused", "reason", string(d.Reason), "detail", d.Detail)
		if s.state != core.StateStarting {
			s.transition(core.StateStarting)
		}
		s.lastStarted = time.Now()
		s.onProcessGone(-1, true)
		return out
	}
	// Held, the way a hold holds a harness that is down: not a crash, so no
	// respawn is pending and crash-loop bookkeeping resets, and enabled
	// stays as it was. The reason is in the set before any transition
	// publishes a snapshot, so no reader sees a stopped harness that is not
	// held.
	s.cancelRestartTimer()
	s.resetCrashState()
	s.consecFailures = 0
	wasHeld := s.holds.Has(reason)
	s.holds = s.holds.With(reason)
	switch s.state {
	case core.StateStopped:
	case core.StateFailed:
		// An operator start cleared the failed latch before admission
		// refused it. failed has one way out, through starting; the start
		// went no further than that.
		s.transition(core.StateStarting)
		s.transition(core.StateStopped)
	default:
		// restarting: the restart policy's respawn, refused.
		s.transition(core.StateStopped)
	}
	if !wasHeld {
		s.logEvent("held", "reason", holdLogReason(reason), "detail", d.Detail, "next", nextText(d.Next))
	}
	s.publishSnapshot()
	return out
}

// refuseRun applies a refused one-shot firing: a skipped record with the
// reason, coalesced into the open skip of its class, unless the funnel
// already recorded the refusal (a ledger failure after it opened the run).
func (s *Supervisor) refuseRun(req RunRequest, res AdmitResult) RunDecision {
	d := res.Decision
	var rec RunRecord
	if res.Run.RunID > 0 {
		rec = res.Run
		s.logEvent("run "+string(rec.Outcome), "run_id", rec.RunID, "trigger", string(req.Trigger), "reason", string(rec.Reason), "detail", d.Detail)
		s.publishRun(EventRunFinished, rec)
	} else {
		// Concurrency (REQ-6's Wait) is recorded skipped here for now: the
		// admission queue that makes a firing wait for a slot is the
		// concurrency story's (stump.wtf/harness#479), and until it lands
		// nothing feeds Decide a concurrency cap, so this branch is not
		// reached by a Wait.
		rec = s.recordSkip(req, skipReasonFor(d.Reason))
		if rec.Coalesced <= 1 && d.Detail != "" {
			// The first refusal of a class says why on the durable log; the
			// coalesced ones after it stay quiet, as every coalesced skip
			// does.
			s.logEvent("admission refused", "run_id", rec.RunID, "reason", string(d.Reason), "detail", d.Detail, "next", nextText(d.Next))
		}
	}
	return RunDecision{Kind: DecisionSkipped, Run: rec, Refused: d.Err()}
}

// skipReasonFor is the run-record skip reason for an admission refusal
// (SPEC-0014 REQ "Run Record Fields" as SPEC-0021 amends it).
func skipReasonFor(r budget.Reason) RunReason {
	switch r {
	case budget.ReasonHours:
		return ReasonOutsideHours
	case budget.ReasonQuotaParked:
		return ReasonQuotaParked
	case budget.ReasonBudget:
		return ReasonBudget
	case budget.ReasonConcurrency:
		return ReasonConcurrency
	case budget.ReasonLedgerUnavailable:
		return ReasonLedgerUnavailable
	}
	return RunReason(r)
}

// nextText renders when a refusal clears, for a durable-log line.
func nextText(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.Format(time.RFC3339)
}

// SettleBudget closes the harness's budget skip records now that the budget
// day they were refused in is over, so the next refusal opens a fresh record,
// and with `catch_up = true` starts the one catch_up run the skipped firings
// earned (SPEC-0021 REQ-5 Scenario "Catch-up after a budget skip"). It
// returns that run's decision, or the zero decision when no catch-up applies.
func (s *Supervisor) SettleBudget() RunDecision {
	var d RunDecision
	s.send(command{kind: cmdSettleBudget, decided: &d})
	return d
}

// settleBudget is cmdSettleBudget on the actor loop.
func (s *Supervisor) settleBudget() RunDecision {
	for k := range s.openSkips {
		if k.reason == ReasonBudget {
			delete(s.openSkips, k)
		}
	}
	if !s.harness.CatchUp {
		s.logEvent("budget day rolled over; firings skipped for budget are not caught up", "catch_up", false)
		return RunDecision{}
	}
	s.logEvent("budget day rolled over; catching up firings skipped for budget")
	// A firing like any other: it goes back through admission, and counts
	// toward the new day.
	return s.startRun(RunRequest{Trigger: TriggerCatchUp})
}

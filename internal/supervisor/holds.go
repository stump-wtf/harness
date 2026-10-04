package supervisor

// Hold Reasons: Hold And Release
//
// A harness the daemon keeps down on purpose is held, for one or more reasons
// (core.HoldSet): its operating hours are closed, its provider's quota is
// parked, or a budget is spent. Every rule SPEC-0012 established for the
// operating-hours hold applies to each reason alike: a hold is not a crash,
// `enabled` survives it, crash-loop bookkeeping resets, a failed harness stays
// failed, and an operator stop clears it. Hold adds a reason and stops the
// harness if it is up; Release clears one; only when the LAST reason clears
// does the harness go back through admission and start, so a park that
// expires out of hours leaves it held for its hours.
//
// Who decides WHEN lives elsewhere: the scheduler's gate pass for hours
// (internal/scheduler, gate.go), and the pass's clearing hooks for quota and
// budget (Manager.HoldsCleared), which the park and budget stories feed. This
// file is the HOW, and it runs on the actor loop so that "still up? then
// hold" is atomic with the process's own exits: once a hold is accepted, an
// exit that follows — from the close's own stop or the process's own accord
// — is a hold exit and never reaches the restart policy, so it is neither a
// crash nor a respawn and the restart count cannot move.
//
// A hold never touches s.enabled, not even transiently. gracefulStopKeepEnabled
// (the manual-restart path) clears it for the length of the stop, and every
// transition inside that window publishes a snapshot the debounced persist
// loop may write — which would leave `enabled = false` in state.json for a
// hold that no start follows to heal it. The exit is already consumed by
// killProcess, so that guard buys nothing here.
//
// Hours and budget close the way the harness's hours_shutdown says (graceful
// by default; closeStep in hours.go finishes the close). Quota always stops at
// once: a parked provider refuses every call, so there is no turn to finish
// (SPEC-0021 REQ-13).
//
// Governing: ADR-0027, SPEC-0021 REQ-4, REQ-13 "Park effects", REQ-14
// "Release and hold reasons", REQ-19; design.md § "Holds become a reason
// set", § "The park detector"; ADR-0019, SPEC-0012 REQ
// "Gate Enforcement", REQ "Graceful Shutdown", REQ "Operating Hours
// Visibility"; SPEC-0003 REQ "Graceful Stop", REQ "Restart On Exit".
//
// @joestump-agent 09/21/2026 - Added for stump.wtf/harness#382 (in hours.go).
//
// @joestump 10/04/2026 - Moved out of hours.go and generalized from a held
// flag to a set of reasons with one release path (stump.wtf/harness#468).
//
// @joestump 10/04/2026 - A quota hold carries its park (Park, parkSelf): the
// durable log names the rule, the reset and that no restart happens, the
// reset is harness_hold_changed's next, and a one-shot's park clearing
// settles its quota_parked skips with one catch_up run (stump.wtf/harness#477).

import (
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// AdmitFunc is the admission seam a harness passes through when its last hold
// reason clears, before it starts again (SPEC-0021 REQ-4, REQ-14). It answers
// the reasons that still hold the harness; an empty set admits. It runs on
// the harness's actor loop, so it must never call back into this supervisor
// or wait on anything that does.
type AdmitFunc func(name string) core.HoldSet

// Hold adds reason to the harness's hold: it cancels any pending respawn,
// resets crash-loop bookkeeping, and marks the harness held — leaving
// `enabled` alone. A harness that is up is stopped. Under
// HoursShutdownGraceful a running harness is marked Closing instead, and the
// scheduler's CloseStep finishes the close from the turn-state watch; a quota
// hold stops at once whatever mode says. A failed harness is never held, and
// for hours neither is a disabled or scheduled one (holdable). Blocks until
// the hold is decided, not until a graceful close is done. closeAt is the
// instant the harness went out of hours (the window's end or the lease's
// end), which anchors the close's deadline; zero means the caller could not
// determine it and the close is capped from now.
func (s *Supervisor) Hold(reason core.HoldReason, mode core.HoursShutdownMode, closeAt time.Time) {
	s.send(command{kind: cmdHold, holdReason: reason, mode: mode, closeAt: closeAt})
}

// Park holds the harness for quota under park p (SPEC-0021 REQ-13): a harness
// that is up stops at once, with no restart, no crash-loop count and its
// backoff reset; `enabled` is left alone. The Manager writes p to state.json
// first. A failed harness is not held (it stays failed).
func (s *Supervisor) Park(p ParkInfo) {
	s.send(command{kind: cmdHold, holdReason: core.HoldQuota, mode: core.HoursShutdownImmediate, park: &p})
}

// EnableHeld records enabled intent (persisted, as Start does) and holds the
// harness for its hours instead of starting it. Autostart, reload and
// use-profile use it for a gated harness that is down, so a harness whose
// hours are closed never comes up just to be shut again by the next gate
// tick; the gate releases it if it is in hours. Governing: ADR-0014 (a newly
// introduced harness records its intent), SPEC-0012 REQ "Gate Enforcement"
// (boot out of hours begins held), REQ "Operating Hours Reload".
func (s *Supervisor) EnableHeld(trigger RunTrigger) {
	s.send(command{kind: cmdHold, holdReason: core.HoldHours, enable: true, mode: core.HoursShutdownImmediate, source: intentSourceFor(trigger)})
}

// Release clears reason from the harness's hold, without writing `enabled`.
// While another reason remains the harness stays held. When the last one
// clears, a graceful close still in flight is cancelled, and a harness that
// is enabled and stopped goes back through admission and starts if it passes:
// a release never starts a failed harness or one the operator stopped
// (SPEC-0012 REQ "Gate Enforcement", SPEC-0021 REQ-14). A no-op for a reason
// the harness is not held for.
func (s *Supervisor) Release(reason core.HoldReason) {
	s.send(command{kind: cmdRelease, holdReason: reason})
}

// holdable reports whether reason may be added on the loop now. A failed
// harness stays failed for every reason (SPEC-0021 REQ-14). Hours keep
// SPEC-0012's meaning of held, "enabled, and down because of its hours", so
// a disabled harness is down because the operator said so and is not the
// gate's to hold, and a scheduled one-shot is never gated (the config parser
// rejects the combination; guarded anyway). A park or a spent budget is not
// about intent: a quota group parks a disabled member too, and the harness
// stays down when that park clears (SPEC-0021 REQ-14 Scenario "A disabled
// parked harness"), so those reasons are refused only for a failed harness.
func (s *Supervisor) holdable(reason core.HoldReason) bool {
	if !reason.Valid() || s.state == core.StateFailed {
		return false
	}
	if reason == core.HoldHours {
		return s.enabled && s.harness.Schedule == ""
	}
	return true
}

// hold is cmdHold on the actor loop. park is the quota park a quota hold
// applies, nil when it is not known.
func (s *Supervisor) hold(reason core.HoldReason, enable bool, mode core.HoursShutdownMode, closeAt time.Time, source string, park *ParkInfo) {
	if enable && s.setIntent(true, source, "") {
		s.publishChangeUnchanged() // persist the recorded intent, as cmdStart does
	}
	if !s.holdable(reason) {
		return
	}
	if reason == core.HoldQuota && park != nil && s.quota != nil {
		// A park reaches the loop by command, and the gate may have
		// released it on the way (a live park racing the exit that parked
		// the harness, then its reset): apply the park in force now, or
		// none, never one that has ended.
		cur, ok := s.quota.ParkOf(s.harness.Name)
		if !ok {
			return
		}
		park = &cur
	}
	wasHeld := s.holds.Has(reason)
	if reason == core.HoldQuota && (park != nil || !wasHeld) {
		// The park this hold enforces, for the log line and the hold's
		// next transition; a quota hold with none is unknown, never a
		// previous park's.
		s.park = ParkInfo{}
		if park != nil {
			s.park = *park
		}
	}
	// nextHoursTransition scans up to eight days of the expression
	// (internal/hours) — cheap in absolute terms, but not free, and every
	// call below that makes new state visible (publishSnapshot, gracefulStop)
	// is also what lets the scheduler's gate pass re-decide this harness, as
	// soon as dispatchGate's deferred cleanup clears s.gating for it.
	// Computing it FIRST, before any of that visibility, keeps this
	// function's actual work — and so a real close/hold's window against the
	// next tick — exactly as short as it was before this line existed,
	// rather than stretched by however long the scan takes.
	var next string
	if !wasHeld {
		next = s.holdNextText(reason)
	}
	runReason := holdRunReason(core.HoldSetOf(reason))
	// (1) cancel any pending respawn; (5) crash-loop bookkeeping reset.
	s.cancelRestartTimer()
	s.dropQueued(OutcomeCancelled, runReason)
	s.resetCrashState()
	s.consecFailures = 0
	s.holds = s.holds.With(reason)
	logKV := []any{"reason", holdLogReason(reason), "next", next}
	if len(s.holds.Reasons()) > 1 {
		// Held for more than this one reason: say so, so the line that
		// explains a later "hold cleared" without a start is on disk.
		logKV = append(logKV, "hold_reasons", s.holds.String())
	}
	if reason == core.HoldQuota {
		// SPEC-0021 REQ-13: a park stops a running harness at once, with no
		// graceful close; the provider refuses every turn it would finish.
		mode = core.HoursShutdownImmediate
	}
	if graceful := mode == core.HoursShutdownGraceful && s.hasProcess() && s.state == core.StateRunning; graceful {
		// SPEC-0012 REQ "Graceful Shutdown": mark the close, record the
		// deadline's anchor, and let the tick finish the close. The
		// process keeps running, still receiving what its agent receives;
		// a new prompt in here does not move the deadline. A second
		// reason arriving mid-close joins the close already in flight and
		// keeps its anchor, so it cannot stretch the deadline.
		if !s.closing {
			if closeAt.IsZero() {
				closeAt = time.Now()
			}
			s.closeAt = closeAt
		}
		s.closing = true
		if !wasHeld {
			// "close start", not "held": the process is still up, and CLAUDE.md
			// "A zero" cuts both ways — a line claiming the harness stopped
			// while it is still running would be the wrong kind of silent
			// wrong. SPEC-0012 REQ "Operating Hours Visibility" asks for the
			// next transition too; "close ended" (already logged by
			// closeStep) is the actual stop.
			s.logEvent("close start", logKV...)
		}
		s.publishSnapshot()
		return
	}
	// An immediate stop ends any graceful close in flight: there is nothing
	// left for closeStep to wait on.
	s.closing = false
	switch {
	case s.hasProcess():
		// (2)+(3): immediate, or graceful against a harness that is not
		// plainly running (starting/restarting/degraded stop at once in
		// any mode — SPEC-0012 REQ "Gate Enforcement"). gracefulStop
		// consumes the exit itself, so the restart policy never sees it
		// and the restart count is untouched (5); enabled is never
		// written (4).
		s.gracefulStop()
		s.finishRunWith(OutcomeCancelled, &s.lastExitCode, runReason)
	case s.state != core.StateStopped:
		// starting / restarting / degraded with no live process: stop at once.
		s.gracefulStop()
	default:
		s.publishSnapshot()
	}
	switch {
	case wasHeld:
	case reason == core.HoldQuota && park != nil:
		s.logParked(*park)
	default:
		s.logEvent("held", logKV...)
	}
}

// quotaExit consults the park detector about the exit in hand (SPEC-0021
// REQ-13), before anything counts it. It asks only about an exit a park
// could change: a one-shot's non-zero exit (a zero exit never parks, REQ-11
// Scenario "The phrase in a successful run"), and a resident's exit the
// restart policy would act on — not one a hold owns, not one of a disabled
// harness, and not one `restart` says is final, since nothing would restart
// that harness for a park to prevent. A process that never spawned made no
// model call.
func (s *Supervisor) quotaExit(code int, spawnFailed bool) (ParkInfo, bool) {
	if s.quota == nil || spawnFailed {
		return ParkInfo{}, false
	}
	oneShot := s.harness.Triggered()
	switch {
	case oneShot && code == 0:
		return ParkInfo{}, false
	case !oneShot && (!s.holds.Empty() || !s.enabled || !s.harness.Restart.ShouldRestart(code)):
		return ParkInfo{}, false
	}
	return s.quota.ParkOnExit(s.harness.Name, QuotaExit{OneShot: oneShot, Started: s.startedAt, Code: code})
}

// parkSelf holds the harness for quota on its own loop: the exit path's park
// (SPEC-0021 REQ-13), already on disk. The reason is in the set before the
// caller's transition publishes a snapshot, so no reader sees the harness
// stopped and not held.
func (s *Supervisor) parkSelf(p ParkInfo) {
	wasHeld := s.holds.Has(core.HoldQuota)
	s.park = p
	s.closing = false
	s.holds = s.holds.With(core.HoldQuota)
	if !wasHeld {
		s.logParked(p)
	}
}

// logParked is a park's durable-log line (SPEC-0021 REQ-19 Scenario "A park
// is logged"): the rule, the reset, and that nothing restarts the harness
// before it. A member parked by its group also names the member whose
// refusal parked it. Never the error text (ADR-0008).
func (s *Supervisor) logParked(p ParkInfo) {
	kv := []any{"reason", "quota", "rule", p.Rule, "until", nextText(p.Until), "restart", "none before the reset"}
	if p.Clamped {
		kv = append(kv, "clamped", true)
	}
	if p.Group != "" {
		kv = append(kv, "quota_group", p.Group)
	}
	if p.By != "" && p.By != s.harness.Name {
		kv = append(kv, "by", p.By)
	}
	if len(s.holds.Reasons()) > 1 {
		kv = append(kv, "hold_reasons", s.holds.String())
	}
	s.logEvent("parked", kv...)
}

// release is cmdRelease on the actor loop: clear one reason, and start the
// harness only if it was the last one, the harness is enabled and stopped,
// and admission passes (SPEC-0021 REQ-14).
func (s *Supervisor) release(reason core.HoldReason) {
	if !s.holds.Has(reason) {
		return
	}
	s.holds = s.holds.Without(reason)
	if reason == core.HoldQuota {
		s.park = ParkInfo{}
	}
	if !s.holds.Empty() {
		// Another reason still holds it: clearing this one starts nothing
		// (SPEC-0021 REQ-14 Scenario "A park expires out of hours"), and a
		// close in flight carries on for the reasons left.
		s.logEvent("hold cleared", "reason", holdLogReason(reason), "hold_reasons", s.holds.String())
		s.publishSnapshot()
		return
	}
	s.closing = false // the last reason clearing cancels a close in flight
	if reason == core.HoldQuota && s.harness.Triggered() {
		// A one-shot's firings are its only starters, so its park clearing
		// starts nothing by itself: the firings it skipped are settled,
		// with one catch_up run if they earned it (SPEC-0021 REQ-13).
		s.logEvent("hold cleared", "reason", holdLogReason(reason))
		s.publishSnapshot()
		s.settleQuotaSkips()
		return
	}
	if !s.enabled || s.hasProcess() || s.state != core.StateStopped {
		// Nothing to start: a disabled harness stays down (SPEC-0021 REQ-14
		// Scenario "A disabled parked harness"), a failed one stays failed,
		// and one still up keeps running. Hours keep SPEC-0012's silence
		// here; a park or a budget clearing is worth its line (REQ-19).
		if reason != core.HoldHours {
			s.logEvent("hold cleared", "reason", holdLogReason(reason))
		}
		s.publishSnapshot()
		return
	}
	// SPEC-0021 REQ-14: a harness whose last reason clears goes back through
	// admission (REQ-4), and starts only if it passes. A reason admission
	// still finds holds it again, with no start in between.
	if s.admit != nil {
		if still := s.admit(s.harness.Name); !still.Empty() {
			s.holds = still
			s.logEvent("held", "reason", "admission", "hold_reasons", still.String())
			s.publishSnapshot()
			return
		}
	}
	if reason == core.HoldHours {
		// "open": SPEC-0012 REQ "Operating Hours Visibility" names this line
		// alongside close start/hold/lease start/lease end. next is the
		// window's own close, since the harness is about to be running in
		// hours again.
		s.logEvent("open", "reason", "operating_hours", "next", s.nextHoursTransition())
	} else {
		s.logEvent("released", "reason", holdLogReason(reason))
	}
	s.startTrigger = TriggerRelease
	s.startProcess(RunRequest{Trigger: TriggerManual})
}

// settleQuotaSkips closes a one-shot's quota_parked skip records now that its
// park has cleared, so the next refusal opens a fresh one, and with
// `catch_up = true` starts the one catch_up run the skipped firings earned,
// through admission like any firing (SPEC-0021 REQ-13: "catch_up gets one run
// after release"; the rule SPEC-0021 REQ-5 gives budget skips).
func (s *Supervisor) settleQuotaSkips() {
	skipped := s.quotaSkipped
	s.quotaSkipped = false
	for k := range s.openSkips {
		if k.reason == ReasonQuotaParked {
			delete(s.openSkips, k)
		}
	}
	if !skipped {
		return
	}
	if !s.harness.CatchUp {
		s.logEvent("quota park cleared; firings skipped while parked are not caught up", "catch_up", false)
		return
	}
	s.logEvent("quota park cleared; catching up firings skipped while parked")
	// A firing like any other: StartRun's intent handling (#159).
	s.suppressPersist = true
	s.startRun(RunRequest{Trigger: TriggerCatchUp})
	s.suppressPersist = false
}

// holdLogReason is the reason= value a hold's durable-log lines carry. Hours
// keep the wording SPEC-0012 shipped, "operating_hours" (design.md § "Holds
// become a reason set": "the log line keeps that wording for hours"); every
// other reason logs its own name.
func holdLogReason(r core.HoldReason) string {
	if r == core.HoldHours {
		return "operating_hours"
	}
	return r.String()
}

// holdRunReason is the run-record reason for a run a hold ended: that of the
// first reason in hs in canonical order. "" for an empty set.
func holdRunReason(hs core.HoldSet) RunReason {
	for _, r := range hs.Reasons() {
		switch r {
		case core.HoldHours:
			return ReasonHours
		case core.HoldQuota:
			return ReasonQuotaParked
		case core.HoldBudget:
			return ReasonBudget
		}
	}
	return ""
}

// holdNextText renders when a hold for reason next clears, for a durable-log
// line: the next operating-hours open for hours, the park's reset for quota.
// A budget day's rollover is not known to the loop, so it reads "unknown" —
// a line still worth writing.
func (s *Supervisor) holdNextText(reason core.HoldReason) string {
	switch reason {
	case core.HoldHours:
		return s.nextHoursTransition()
	case core.HoldQuota:
		return nextText(s.park.Until)
	}
	return "unknown"
}

// holdNext is the `next` of a harness_hold_changed event (SPEC-0021 REQ-19):
// when the current hold is expected to clear. It is known when hours are the
// sole reason, and when quota is, from its park's reset; with any reason
// whose clearing instant the loop does not know, a time would understate the
// hold, so it is zero (unknown), as it is when the harness is not held.
func (s *Supervisor) holdNext() time.Time {
	if s.holds == core.HoldSetOf(core.HoldQuota) {
		return s.park.Until
	}
	if s.holds != core.HoldSetOf(core.HoldHours) {
		return time.Time{}
	}
	expr := s.hoursExpr()
	if expr.String() == "" {
		return time.Time{}
	}
	_, next, ok := expr.In(time.Now())
	if !ok {
		return time.Time{}
	}
	return next
}

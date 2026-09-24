package supervisor

// Operating Hours Hold, Close And Release
//
// A gated harness (one with operating_hours) is held down outside its windows
// and started again when one opens, without its `enabled` intent ever
// changing. The scheduler's gate pass decides WHEN (internal/scheduler,
// gate.go); this file is the HOW, and it runs on the actor loop so that
// "still up? then hold" is atomic with the process's own exits: once a hold
// is accepted, an exit that follows — from the close's own stop or the
// process's own accord — is a hold exit and never reaches the restart
// policy, so it is neither a crash nor a respawn and the restart count
// cannot move.
//
// A hold never touches s.enabled, not even transiently. gracefulStopKeepEnabled
// (the manual-restart path) clears it for the length of the stop, and every
// transition inside that window publishes a snapshot the debounced persist
// loop may write — which would leave `enabled = false` in state.json for a
// hold that no start follows to heal it. The exit is already consumed by
// killProcess, so that guard buys nothing here.
//
// A graceful close (hours_shutdown = "graceful", the default) marks the
// supervisor Closing and returns; the scheduler tick then drives
// Manager.CloseStep, which samples the daemon's turn-state watch
// (internal/runtrace) and sends the observation here. The close stops the
// harness at the first of: turn ended and settled, quiet long enough with no
// markers to settle, or the deadline — closeAt (the instant the harness went
// out of hours, not when the daemon noticed) plus the configured timeout,
// re-read from the staged config on every step so a reload applies to a close
// in progress. An immediate close, and any close of a harness that is not
// plain running, stops at once.
//
// Governing: ADR-0019 (operating hours), SPEC-0012 REQ "Gate Enforcement",
// REQ "Graceful Shutdown", REQ "Shutdown Mode", REQ "Operating Hours Reload";
// design.md § "Hold and Release on the Manager", § "Turn state from a
// daemon-side watcher"; SPEC-0003 REQ "Graceful Stop", REQ "Restart On Exit".
//
// @joestump-agent 09/21/2026 - Added for stump.wtf/harness#382.
//
// @joestump-agent 09/21/2026 - Graceful close for #384: Closing marks a close
// in flight, closeStep advances it on the tick, and an exit while held is
// consumed by the gate instead of the restart policy.

import (
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/hours"
	"github.com/stump-wtf/harness/internal/runtrace"
)

// Close-timing constants (SPEC-0012 REQ "Graceful Shutdown"): how long a
// settled turn end is given for the agent to really stop talking, and how long
// a marker-less agent must stay silent before the close reads it as done.
const (
	closeSettle = 10 * time.Second
	closeQuiet  = 2 * time.Minute
)

// Hold stops the harness for its operating hours: it cancels any pending
// respawn, resets crash-loop bookkeeping, and marks the harness held —
// leaving `enabled` alone. Under HoursShutdownGraceful a running harness is
// marked Closing instead of stopped; the scheduler's CloseStep finishes the
// close from the turn-state watch. A harness that is disabled or failed is
// not held. Blocks until the hold is decided, not until a graceful close is
// done. closeAt is the instant the harness went out of hours (the window's
// end or the lease's end), which anchors the close's deadline; zero means the
// caller could not determine it and the close is capped from now.
func (s *Supervisor) Hold(mode core.HoursShutdownMode, closeAt time.Time) {
	s.send(command{kind: cmdHold, mode: mode, closeAt: closeAt})
}

// EnableHeld records enabled intent (persisted, as Start does) and holds the
// harness instead of starting it. Autostart, reload and use-profile use it for
// a gated harness that is down, so a harness whose hours are closed never
// comes up just to be shut again by the next gate tick; the gate releases it
// if it is in hours. Governing: ADR-0014 (a newly introduced harness records
// its intent), SPEC-0012 REQ "Gate Enforcement" (boot out of hours begins
// held), REQ "Operating Hours Reload".
func (s *Supervisor) EnableHeld() {
	s.send(command{kind: cmdHold, enable: true, mode: core.HoursShutdownImmediate})
}

// Release clears a hold and starts the harness, without writing `enabled`. A
// no-op unless the harness is held, enabled and stopped: hours opening never
// start a failed harness or one the operator stopped (SPEC-0012 REQ "Gate
// Enforcement"). A graceful close still in flight is cancelled.
func (s *Supervisor) Release() { s.send(command{kind: cmdRelease}) }

// CloseStep advances a graceful close by one observation: the manager samples
// the turn-state watch and hands the answer to the loop, which stops the
// harness when the close is done waiting and otherwise leaves it running. A
// no-op when no close is in flight.
func (s *Supervisor) CloseStep(now time.Time, ts runtrace.TurnState, ok bool, why string) {
	s.send(command{kind: cmdCloseStep, step: &closeStepReq{now: now, ts: ts, ok: ok, why: why}})
}

// gated reports whether the harness carries operating_hours, reading a staged
// definition first: hours are a supervision key, so a reload that also stages
// a run-affecting change is still seen as gated (or not) at once.
func (s *Supervisor) gated() bool {
	if s.pending != nil {
		return s.pending.OperatingHours != ""
	}
	return s.harness.OperatingHours != ""
}

// shutdownMode and shutdownTimeout read a supervision value from the staged
// definition first, the applied one second, the way gated does. Both apply to
// a close in progress without a restart (SPEC-0012 REQ "Shutdown Mode"), so
// every close decision reads them fresh.
func (s *Supervisor) shutdownMode() core.HoursShutdownMode {
	if s.pending != nil {
		return s.pending.HoursShutdown
	}
	return s.harness.HoursShutdown
}

func (s *Supervisor) shutdownTimeout() time.Duration {
	if s.pending != nil {
		return s.pending.HoursShutdownTimeout
	}
	return s.harness.HoursShutdownTimeout
}

// hoursExpr reads the staged HoursExpr first, the applied one second, the
// same supervision-key contract gated/shutdownMode/shutdownTimeout use. Read
// by the durable-log lines below for the next transition to report (SPEC-0012
// REQ "Operating Hours Visibility") — a presentation-only read, never a gate
// decision, which is still decided from CloseAt/mode/deadline exactly as
// before.
func (s *Supervisor) hoursExpr() hours.Expr {
	if s.pending != nil {
		return s.pending.HoursExpr
	}
	return s.harness.HoursExpr
}

// hold is cmdHold on the actor loop.
func (s *Supervisor) hold(enable bool, mode core.HoursShutdownMode, closeAt time.Time) {
	if enable && !s.enabled {
		s.enabled = true
		s.publishChangeUnchanged() // persist the recorded intent, as cmdStart does
	}
	// Held means "enabled, and down because of its hours". A disabled harness
	// is down because the operator said so, and a failed one stays failed;
	// neither is the gate's to hold. A scheduled one-shot is never gated (the
	// config parser rejects the combination); guard it anyway.
	if !s.enabled || s.state == core.StateFailed || s.harness.Schedule != "" {
		return
	}
	wasHeld := s.held
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
		next = s.nextHoursTransition()
	}
	// (1) cancel any pending respawn; (5) crash-loop bookkeeping reset.
	s.cancelRestartTimer()
	s.dropQueued(OutcomeCancelled, "")
	s.resetCrashState()
	s.consecFailures = 0
	s.held = true
	if graceful := mode == core.HoursShutdownGraceful && s.hasProcess() && s.state == core.StateRunning; graceful {
		// SPEC-0012 REQ "Graceful Shutdown": mark the close, record the
		// deadline's anchor, and let the tick finish the close. The
		// process keeps running, still receiving what its agent receives;
		// a new prompt in here does not move the deadline.
		s.closing = true
		if closeAt.IsZero() {
			closeAt = time.Now()
		}
		s.closeAt = closeAt
		if !wasHeld {
			// "close start", not "held": the process is still up, and CLAUDE.md
			// "A zero" cuts both ways — a line claiming the harness stopped
			// while it is still running would be the wrong kind of silent
			// wrong. SPEC-0012 REQ "Operating Hours Visibility" asks for the
			// next transition too; "close ended" (already logged by
			// closeStep) is the actual stop.
			s.logEvent("close start", "reason", "operating_hours", "next", next)
		}
		s.publishSnapshot()
		return
	}
	switch {
	case s.hasProcess():
		// (2)+(3): immediate, or graceful against a harness that is not
		// plainly running (starting/restarting/degraded stop at once in
		// any mode — SPEC-0012 REQ "Gate Enforcement"). gracefulStop
		// consumes the exit itself, so the restart policy never sees it
		// and the restart count is untouched (5); enabled is never
		// written (4).
		s.gracefulStop()
		s.finishRun(OutcomeCancelled, &s.lastExitCode)
	case s.state != core.StateStopped:
		// starting / restarting / degraded with no live process: stop at once.
		s.gracefulStop()
	default:
		s.publishSnapshot()
	}
	if !wasHeld {
		s.logEvent("held", "reason", "operating_hours", "next", next)
	}
}

// closeStep is cmdCloseStep on the actor loop: one observation of the
// turn-state watch, decided against the close's conditions. The stop is the
// gate's own (hold steps 3–5 of SPEC-0012 REQ "Gate Enforcement"): no
// restart, no restart-count increment, `enabled` untouched. The durable log
// records which condition ended the close (SPEC-0012 REQ "Graceful
// Shutdown").
func (s *Supervisor) closeStep(req *closeStepReq) {
	if !s.closing || req == nil {
		return // the close was cancelled (release, start, stop) before this step
	}
	now := req.now
	if now.IsZero() {
		now = time.Now()
	}
	mode := s.shutdownMode()
	timeout := s.shutdownTimeout()
	if timeout <= 0 {
		timeout = core.DefaultHoursShutdownTimeout
	}
	deadline := s.closeAt.Add(timeout)

	stop, reason := false, ""
	switch {
	case mode != core.HoursShutdownGraceful:
		// A reload to immediate mid-close applies on the next tick
		// (SPEC-0012 REQ "Shutdown Mode").
		stop, reason = true, "hours_shutdown=immediate"
	case !req.ok:
		// Nothing attributable to the run — a generic adapter, no workdir,
		// or a session correlation excludes: stop at once and say why.
		stop, reason = true, "graceful unavailable: "+req.why
	case req.ts.TurnMarkers && req.ts.TurnEnded && now.Sub(req.ts.LastEventAt) >= closeSettle:
		stop, reason = true, "turn ended"
	case !req.ts.TurnMarkers && now.Sub(req.ts.LastEventAt) >= closeQuiet:
		stop, reason = true, "quiet"
	case !now.Before(deadline):
		stop, reason = true, "deadline reached"
	}
	if !stop {
		s.publishSnapshot()
		return
	}
	s.closing = false
	s.logEvent("close ended", "reason", reason, "deadline", deadline.Format(time.RFC3339))
	s.gracefulStop()
	s.finishRun(OutcomeCancelled, &s.lastExitCode)
	s.publishSnapshot()
}

// release is cmdRelease on the actor loop.
func (s *Supervisor) release() {
	if !s.held {
		return
	}
	s.held = false
	s.closing = false // hours reopening cancels a close in flight
	if !s.enabled || s.hasProcess() || s.state != core.StateStopped {
		s.publishSnapshot()
		return
	}
	// "open": SPEC-0012 REQ "Operating Hours Visibility" names this line
	// alongside close start/hold/lease start/lease end. next is the window's
	// own close, since the harness is about to be running in hours again.
	s.logEvent("open", "reason", "operating_hours", "next", s.nextHoursTransition())
	s.startProcess(RunRequest{Trigger: TriggerManual})
}

// nextHoursTransition renders the next operating-hours flip for a durable-log
// line (SPEC-0012 REQ "Operating Hours Visibility": "stating the reason and
// the next transition"): the next open while held, the next close once
// released. "unknown" when the expression covers the entire week (no next
// flip exists) or hours were removed from underneath this decision — a log
// line still worth writing, just without a time to give.
func (s *Supervisor) nextHoursTransition() string {
	expr := s.hoursExpr()
	if expr.String() == "" {
		return "unknown"
	}
	_, next, ok := expr.In(time.Now())
	if !ok {
		return "unknown"
	}
	return next.Format(time.RFC3339)
}

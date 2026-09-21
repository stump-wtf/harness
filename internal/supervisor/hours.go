package supervisor

// Operating Hours Hold And Release
//
// A gated harness (one with operating_hours) is held down outside its windows
// and started again when one opens, without its `enabled` intent ever
// changing. The scheduler's gate pass decides WHEN (internal/scheduler,
// gate.go); this file is the HOW, and it runs on the actor loop so that
// "still up? then hold" is atomic with the process's own exits: once a hold
// is accepted, every exit that follows is consumed by gracefulStop
// (killProcess reads exitCh itself) as a hold exit and never reaches
// onProcessGone, so it is neither a crash nor a respawn and the restart count
// cannot move.
//
// A hold never touches s.enabled, not even transiently. gracefulStopKeepEnabled
// (the manual-restart path) clears it for the length of the stop, and every
// transition inside that window publishes a snapshot the debounced persist
// loop may write — which would leave `enabled = false` in state.json for a
// hold that no start follows to heal it. The exit is already consumed by
// killProcess, so that guard buys nothing here.
//
// Closes are immediate in this story: `hours_shutdown = "graceful"` is
// reported by the gate pass as not yet available and closes at once.
//
// Governing: ADR-0019 (operating hours), SPEC-0012 REQ "Gate Enforcement",
// REQ "Operating Hours Reload"; design.md § "Hold and Release on the
// Manager"; SPEC-0003 REQ "Graceful Stop", REQ "Restart On Exit".
//
// @joestump-agent 09/21/2026 - Added for stump.wtf/harness#382.

import "github.com/stump-wtf/harness/internal/core"

// Hold stops the harness for its operating hours: it cancels any pending
// respawn, runs the SPEC-0003 graceful-stop sequence, resets crash-loop
// bookkeeping, and marks the harness held — leaving `enabled` alone. A
// harness that is disabled or failed is not held. Blocks until the harness is
// stopped.
func (s *Supervisor) Hold() { s.send(command{kind: cmdHold}) }

// EnableHeld records enabled intent (persisted, as Start does) and holds the
// harness instead of starting it. Autostart, reload and use-profile use it for
// a gated harness that is down, so a harness whose hours are closed never
// comes up just to be shut again by the next gate tick; the gate releases it
// if it is in hours. Governing: ADR-0014 (a newly introduced harness records
// its intent), SPEC-0012 REQ "Gate Enforcement" (boot out of hours begins
// held), REQ "Operating Hours Reload".
func (s *Supervisor) EnableHeld() { s.send(command{kind: cmdHold, enable: true}) }

// Release clears a hold and starts the harness, without writing `enabled`. A
// no-op unless the harness is held, enabled and stopped: hours opening never
// start a failed harness or one the operator stopped (SPEC-0012 REQ "Gate
// Enforcement").
func (s *Supervisor) Release() { s.send(command{kind: cmdRelease}) }

// gated reports whether the harness carries operating_hours, reading a staged
// definition first: hours are a supervision key, so a reload that also stages
// a run-affecting change is still seen as gated (or not) at once.
func (s *Supervisor) gated() bool {
	if s.pending != nil {
		return s.pending.OperatingHours != ""
	}
	return s.harness.OperatingHours != ""
}

// hold is cmdHold on the actor loop.
func (s *Supervisor) hold(enable bool) {
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
	// (1) cancel any pending respawn; (5) crash-loop bookkeeping reset.
	s.cancelRestartTimer()
	s.dropQueued(OutcomeCancelled)
	s.resetCrashState()
	s.consecFailures = 0
	s.held = true
	switch {
	case s.hasProcess():
		// (2)+(3): the close is immediate in this story, so straight into the
		// graceful-stop sequence. gracefulStop consumes the exit itself, so the
		// restart policy never sees it and the restart count is untouched (5);
		// enabled is never written (4).
		s.gracefulStop()
		s.finishRun(OutcomeCancelled, &s.lastExitCode)
	case s.state != core.StateStopped:
		// starting / restarting / degraded with no live process: stop at once.
		s.gracefulStop()
	default:
		s.publishSnapshot()
	}
	if !wasHeld {
		s.logEvent("held", "reason", "operating_hours")
	}
}

// release is cmdRelease on the actor loop.
func (s *Supervisor) release() {
	if !s.held {
		return
	}
	s.held = false
	if !s.enabled || s.hasProcess() || s.state != core.StateStopped {
		s.publishSnapshot()
		return
	}
	s.logEvent("released", "reason", "operating_hours")
	s.startProcess(RunRequest{Trigger: TriggerManual})
}

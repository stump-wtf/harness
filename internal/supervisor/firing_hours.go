package supervisor

// Operating Hours On A Triggered Harness
//
// On a resident harness operating_hours gates the PROCESS: the gate pass holds
// it at close and releases it at open (hours.go). On a harness that sets
// `triggers` the same key gates FIRINGS instead, and nothing here ever stops a
// process. The source manager asks whether each bound harness is in hours at
// the event's receive time, before fan-out, and a harness that is not gets
// SkipRun rather than StartRun: a `skipped` record with reason
// `outside_hours`, coalesced on this loop through the same open-skip map an
// overlap skip uses, so a weekend of doorbells is one record, not a history
// flushed by keep_runs. A run already in flight at the close is not touched:
// it ends on its own or at its `timeout`, like any other run.
//
// The loop remembers that something was skipped (hoursSkipped), and the
// scheduler's gate pass reads it off the snapshot. On the first in-hours
// evaluation after one or more such skips the pass calls OpenFirings, which
// settles them atomically here: the flag clears, the outside_hours skip
// records close (the next closed window opens fresh ones), and with
// `catch_up = true` exactly one `catch_up` run is asked for, through the
// overlap policy like any other firing. Deciding on the loop is what makes
// "exactly one" hold: two passes racing an OpenFirings would find the flag
// already clear.
//
// The flag is derived state, but it must survive a daemon restart over the
// weekend or the Monday catch-up silently never happens, so boot re-seeds it
// from the persisted history (Manager.seedHoursSkipped): the newest record
// being an outside_hours skip means nothing has run since.
//
// Governing: ADR-0021, ADR-0019; SPEC-0014 REQ "Operating Hours On Triggered
// Harnesses", REQ "Overlap Skip Coalescing"; SPEC-0012 REQ "Gate Evaluation".
//
// @joestump 09/23/2026 - Added for stump.wtf/harness#484.

// SkipRun records req as a skipped firing with reason, coalesced with the
// open skip of the same class (runs.go recordSkip), without consulting the
// overlap policy: the caller already decided the firing must not run. An
// outside_hours skip also marks the harness as owed the settle-up
// OpenFirings performs. Blocks until the loop has recorded it.
//
// A supervisor that has already shut down records nothing, and the zero
// RunDecision says so.
func (s *Supervisor) SkipRun(req RunRequest, reason RunReason) RunDecision {
	var d RunDecision
	s.send(command{kind: cmdSkipRun, run: &req, reason: reason, decided: &d})
	return d
}

// OpenFirings settles the outside_hours skips on the first in-hours
// evaluation after them, starting one catch_up run when the harness sets
// `catch_up = true`. It returns that run's decision, or the zero decision
// when nothing was owed or no catch-up applies.
func (s *Supervisor) OpenFirings() RunDecision {
	var d RunDecision
	s.send(command{kind: cmdOpenFirings, decided: &d})
	return d
}

// SeedHoursSkipped marks the harness as owed an OpenFirings settle-up. Boot
// calls it for a harness whose persisted history ends in an outside_hours
// skip.
func (s *Supervisor) SeedHoursSkipped() { s.send(command{kind: cmdSeedHoursSkipped}) }

// skipRun is cmdSkipRun on the actor loop.
func (s *Supervisor) skipRun(req RunRequest, reason RunReason) RunDecision {
	rec := s.recordSkip(req, reason)
	if reason == ReasonOutsideHours && !s.hoursSkipped {
		s.hoursSkipped = true
		s.publishSnapshot()
	}
	return RunDecision{Kind: DecisionSkipped, Run: rec}
}

// openFirings is cmdOpenFirings on the actor loop.
func (s *Supervisor) openFirings() RunDecision {
	if !s.hoursSkipped {
		return RunDecision{}
	}
	s.hoursSkipped = false
	// The window those skips were "during" is over, so the next closed
	// window opens a new record rather than incrementing one from last
	// week — the same reason finishRun clears the map at the end of a run.
	for k := range s.openSkips {
		if k.reason == ReasonOutsideHours {
			delete(s.openSkips, k)
		}
	}
	s.publishSnapshot()
	if !s.harness.CatchUp {
		s.logEvent("operating hours opened; firings skipped while closed are not caught up", "catch_up", false)
		return RunDecision{}
	}
	s.logEvent("operating hours opened; catching up firings skipped while closed")
	return s.startRun(RunRequest{Trigger: TriggerCatchUp})
}

// SkipRun records a firing of name that must not run, with reason, and
// returns the skipped decision. ok is false for an unknown harness. It is the
// source manager's path for a firing its operating-hours gate refused
// (SPEC-0014 REQ "Operating Hours On Triggered Harnesses").
func (m *Manager) SkipRun(name string, req RunRequest, reason RunReason) (RunDecision, bool) {
	s := m.get(name)
	if s == nil {
		return RunDecision{}, false
	}
	return s.SkipRun(req, reason), true
}

// HoursSkipped reports whether name has skipped a firing as outside_hours
// since its hours last opened. It reads the snapshot, so the gate pass can
// ask every tick without a trip through the actor loop.
func (m *Manager) HoursSkipped(name string) bool {
	s := m.get(name)
	if s == nil {
		return false
	}
	return s.Snapshot().HoursSkipped
}

// OpenFirings settles name's outside_hours skips now that its hours have
// opened, starting one catch_up run under `catch_up = true`. ok is false for
// an unknown harness.
func (m *Manager) OpenFirings(name string) (RunDecision, bool) {
	s := m.get(name)
	if s == nil {
		return RunDecision{}, false
	}
	return s.OpenFirings(), true
}

// seedHoursSkipped re-derives, at boot, the one piece of firing-gate state a
// restart would otherwise lose: a triggered harness whose newest run record
// is an outside_hours skip has had nothing run since, and so is still owed
// the settle-up (and its catch_up) when its hours next open.
//
// The records come from the ledger (Manager.Runs), not the pre-ledger
// runHistory list: that type now holds the id allocator alone, and the record
// list is read back from the ledger. Runs is newest-last within its window,
// so the newest record is the last one.
func (m *Manager) seedHoursSkipped(name string, s *Supervisor) {
	recs := m.Runs(name)
	if n := len(recs); n > 0 &&
		recs[n-1].Outcome == OutcomeSkipped &&
		recs[n-1].Reason == ReasonOutsideHours {
		s.SeedHoursSkipped()
	}
}

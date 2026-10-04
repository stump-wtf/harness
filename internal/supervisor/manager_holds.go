package supervisor

// Manager Side Of Hold Reasons
//
// The Manager half of the hold-reason machinery (holds.go is the actor-loop
// half): Hold and Release are the seams the scheduler's gate pass and, in
// later stories, the park detector and the budget meter call; HoldsCleared is
// the pass's clearing hook for every reason other than hours; and
// admitRelease is the admission a harness whose last reason clears passes
// through before it starts again.
//
// SPEC-0021 REQ-4 puts a fixed series of checks in front of every start, a
// released one included: admitRelease (manager_admit.go) is that check for a
// release, and the funnel admits the start that follows it.
//
// Governing: ADR-0027, SPEC-0021 REQ-4, REQ-14 "Release and hold reasons";
// design.md § "Holds become a reason set", § "Admission is a funnel on the
// Manager"; ADR-0019, SPEC-0012 REQ "Gate Enforcement", REQ "Graceful
// Shutdown".
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#468; Hold moved here
// from manager_hours.go and Release from manager.go, both taking a reason.
//
// @joestump 10/04/2026 - admitRelease filled in and moved to
// manager_admit.go (stump.wtf/harness#470).

import (
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// HoldClearer answers whether name's hold for one reason has cleared at now,
// the gate tick's clock: a park whose reset instant has passed, a budget day
// that rolled over. It is asked on every tick for every harness held for its
// reason, so it must be cheap and must not call back into the Manager's
// supervisors.
type HoldClearer func(name string, now time.Time) bool

// Hold adds reason to name's hold without touching its enabled intent
// (Supervisor.Hold): a harness that is up is stopped, at once for quota, and
// for hours and budget the way mode says. Under HoursShutdownGraceful a
// running harness only marks its close and the Manager arms the turn-state
// watch for it; CloseStep finishes the close. closeAt anchors the close's
// deadline. An hours hold consumes the lease record (if any) that just
// expired, since this hold is what enforces its end. Governing: ADR-0019,
// SPEC-0012 REQ "Gate Enforcement", REQ "Graceful Shutdown"; SPEC-0021
// REQ-14.
func (m *Manager) Hold(name string, reason core.HoldReason, mode core.HoursShutdownMode, closeAt time.Time) {
	m.mu.Lock()
	s := m.supervisors[name]
	if reason == core.HoldHours {
		delete(m.expiredLease, name) // consumed: this hold enforces the lease's end
	}
	m.mu.Unlock()
	if s == nil {
		return
	}
	s.Hold(reason, mode, closeAt)
	if snap := s.Snapshot(); snap.Closing {
		// Follow with the anchor the actor loop settled on, not the
		// caller's: an unanchored hold (closeAt zero) is capped from now
		// there, and the watch's linger backstop must be bounded by that
		// same instant rather than by the zero time, which would retire
		// the watch on its first poll and end the close as "graceful
		// unavailable".
		m.follow(name, snap.CloseAt)
	}
}

// Release clears reason from name's hold without touching its enabled intent
// (Supervisor.Release). The harness starts only when that was its last
// reason, it is enabled and stopped, and admission passes. ok=false if
// unknown. When the hold has ended, a graceful close still in flight is
// cancelled and its turn-state watch torn down: hours reopening leaves the
// harness running as an ordinary in-hours one. A close another reason still
// wants keeps its watch. Governing: SPEC-0012 REQ "Gate Enforcement";
// SPEC-0021 REQ-14.
func (m *Manager) Release(name string, reason core.HoldReason) bool {
	s := m.get(name)
	if s == nil {
		return false
	}
	s.Release(reason)
	if !s.Snapshot().Closing {
		m.unfollowWatch(name)
	}
	return true
}

// HoldsCleared reports, at now, every harness whose hold for a reason other
// than hours has cleared, with the reasons that did: the gate pass's clearing
// hook (SPEC-0021 REQ-14: "The gate tick SHALL clear quota when the park's
// reset instant is reached, and budget when the budget day rolls over or the
// spent cap is raised by a reload"). The pass releases each one. Hours are
// the pass's own to judge, from the expression, and never appear here. With
// no clearer configured it reports nothing without reading a snapshot, so
// the hook costs nothing per tick until a story feeds it.
func (m *Manager) HoldsCleared(now time.Time) map[string]core.HoldSet {
	if len(m.holdClearers) == 0 {
		return nil
	}
	var out map[string]core.HoldSet
	for _, s := range m.snapshotSupervisors() {
		snap := s.Snapshot()
		for _, r := range snap.Holds.Without(core.HoldHours).Reasons() {
			clear := m.holdClearers[r]
			if clear == nil || !clear(snap.Name, now) {
				continue
			}
			if out == nil {
				out = make(map[string]core.HoldSet)
			}
			out[snap.Name] = out[snap.Name].With(r)
		}
	}
	return out
}

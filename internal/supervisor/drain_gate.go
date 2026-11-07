package supervisor

// The Operator Drain Gate
//
// `harness drain NAME|--all [--discard]` takes a harness out of service: no
// source starts a new run from then on — not cron, a channel, a webhook,
// `harness trigger`, a catch_up or a self-triggered queue — while the run in
// flight keeps running untouched to its own end. `harness undrain` resumes:
// the gate clears, and the one firing the drain held (an
// on_overlap = "queue" request) starts like any other firing.
//
// The gate is a hold reason, core.HoldDrain, checked first in the admission
// funnel (SPEC-0021 REQ-4, check 0): every start path already passes that
// funnel, so arming one reason gates every source instead of adding a
// second-guessed check to eight entry points. Unlike hours, quota and budget
// it never stops anything that is up: a hold keeps a harness DOWN, a drain
// keeps it from coming back up. That is why drain is its own loop command
// rather than Manager.Hold — hold would SIGTERM the run in flight, the very
// thing the issue this answers (stump.wtf/harness#828) is about.
//
// A queued firing is not run through admission while the drain holds it: it
// would only record a skip and be gone. `drain` therefore leaves s.queued
// alone (finishRunWith keeps it queued while the reason is in the set), and
// `--discard` drops it once, recorded cancelled with reason=draining — the
// way a hold's and a shutdown's drop is recorded.
//
// Like the rest of the hold set, the drain is derived runtime state: it is
// never written to state.json. A daemon restart resumes everything — which
// is exactly what the packaging script that chains
// `drain --all && drain --wait && <restart>` wants: the wait is the gate,
// the restart is the resume.
//
// Governing: stump.wtf/harness#828; ADR-0062 (proposed); SPEC-0021 REQ-4
// "Admission", REQ-14 "Release and hold reasons" (as amended by the drain
// reason); SPEC-0003 REQ "State Model" (draining/drained are surface labels
// over State and Holds, not new lifecycle states).
//
// @joestump-agent 11/06/2026 - Added for stump.wtf/harness#828.

import (
	"errors"
	"fmt"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// ErrDrainTimeout reports that DrainWait passed its deadline with a drained
// harness still running. The CLI exits non-zero on it, which is what makes
// `drain --wait` usable as a restart gate.
var ErrDrainTimeout = errors.New("drain wait timed out")

// Drain arms the gate on this harness (Manager.Drain): every source's next
// start is refused at admission, the run in flight is left alone, and a
// queued firing is either held for undrain or — with discard — dropped now.
func (s *Supervisor) Drain(discard bool) {
	s.send(command{kind: cmdDrain, drainDiscard: discard})
}

// drain is cmdDrain on the actor loop, where the hold set and the queue
// live. It is idempotent: arming a drain that is already armed changes the
// gate not at all; a discard that arrives late still drops the held firing,
// because asking for --discard twice means the operator still wants it gone.
func (s *Supervisor) drain(discard bool) {
	if s.holds.Has(core.HoldDrain) {
		if discard && s.queued != nil {
			s.dropQueued(OutcomeCancelled, ReasonDraining)
		}
		return
	}
	queued := "none"
	switch {
	case s.queued == nil:
	case discard:
		s.dropQueued(OutcomeCancelled, ReasonDraining)
		queued = "discarded"
	default:
		queued = "held"
	}
	// The reason joins the set before any publish, so no reader sees a
	// gate that is armed but not recorded — the same ordering rule holds
	// for every other reason (refuseResident, parkSelf).
	s.holds = s.holds.With(core.HoldDrain)
	state := DrainDrained
	if s.hasProcess() {
		state = DrainDraining
	}
	s.logEvent("drain", "state", state, "queued", queued, "hold_reasons", s.holds.String())
	s.publishSnapshot()
}

// Drain arms the gate on every named harness. An unknown name is refused
// whole, before any harness is touched: an operator who typed one harness
// wrong must not get a partial out-of-service.
func (m *Manager) Drain(names []string, discard bool) error {
	sups, err := m.drainTargets(names)
	if err != nil {
		return err
	}
	for _, s := range sups {
		s.Drain(discard)
	}
	return nil
}

// Undrain clears the gate on every named harness (SPEC-0021 REQ-14 through
// the one release path): a resident whose last reason just cleared goes
// back through admission and starts, a one-shot starts only the firing the
// drain held, and a harness still held for another reason stays down.
func (m *Manager) Undrain(names []string) error {
	sups, err := m.drainTargets(names)
	if err != nil {
		return err
	}
	for _, s := range sups {
		m.releaseDrain(s)
	}
	return nil
}

// releaseDrain is Release scoped to the drain reason: it clears the reason
// and tears down a turn-state watch if the harness is no longer closing, as
// every release does.
func (m *Manager) releaseDrain(s *Supervisor) {
	s.Release(core.HoldDrain)
	if snap := s.Snapshot(); !snap.Closing {
		m.unfollowWatch(snap.Name)
	}
}

// drainTargets resolves verb names to supervisors: an explicit list, or —
// for --all, which the CLI expands from the daemon's own list — nothing
// empty. Unknown names are refused before the first supervisor is touched.
func (m *Manager) drainTargets(names []string) ([]*Supervisor, error) {
	sups := make([]*Supervisor, 0, len(names))
	for _, n := range names {
		s := m.get(n)
		if s == nil {
			return nil, fmt.Errorf("%w: %s", ErrUnknownHarness, n)
		}
		sups = append(sups, s)
	}
	return sups, nil
}

// DrainWait blocks until every harness currently holding the drain is idle —
// no process up — and returns nil then. It answers nil immediately when
// nothing is draining (or nothing is queued behind a drain: the queue is not
// a process). A harness that finishes its run, and an on_overlap = "queue"
// firing the drain is holding, both count as idle: the gate did its job and
// the held firing waits for undrain, not for the script. timeout zero means
// wait without a deadline; the wait ends with ErrDrainTimeout otherwise.
// Polling like the close waits (manager_drain.go): the condition is a pure
// read of snapshots, so a tick is honest and simple.
func (m *Manager) DrainWait(timeout time.Duration) error {
	deadline := time.Time{}
	if timeout > 0 {
		deadline = time.Now().Add(timeout)
	}
	for {
		if m.drainedIdle() {
			return nil
		}
		if !deadline.IsZero() && !time.Now().Before(deadline) {
			return ErrDrainTimeout
		}
		time.Sleep(drainWaitTick)
	}
}

// drainWaitTick is how often DrainWait re-reads the snapshots. Runs end on
// seconds-to-minutes scales; this only decides how fast a finished one is
// noticed. Tests shorten it.
var drainWaitTick = 250 * time.Millisecond

// drainedIdle reports whether no draining harness has a process up.
func (m *Manager) drainedIdle() bool {
	for _, s := range m.snapshotSupervisors() {
		snap := s.Snapshot()
		if snap.Holds.Has(core.HoldDrain) && snapUp(snap.State) {
			return false
		}
	}
	return true
}

// The drain labels are the two the CLI and the wire show for a drained
// harness (stump.wtf/harness#828). They name where a drain has got to, over
// State and Holds: SPEC-0003's lifecycle states stay untouched, and an older
// client still reads the gate as one more hold reason.
const (
	// DrainDraining: drained, with a run still in flight.
	DrainDraining = "draining"
	// DrainDrained: drained and idle, so a restart is safe.
	DrainDrained = "drained"
)

// DrainLabel is a snapshot's drain label: "draining" while a drained harness
// still has a run up, "drained" once it is idle, empty when nothing drains it.
func DrainLabel(snap Snapshot) string {
	if !snap.Holds.Has(core.HoldDrain) {
		return ""
	}
	if snapUp(snap.State) {
		return DrainDraining
	}
	return DrainDrained
}

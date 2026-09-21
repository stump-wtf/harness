package supervisor

// Manager Side Of The Operating-Hours Close
//
// The scheduler's gate pass decides when to hold, when to step a graceful
// close, and when a close's conditions were met; this file is the Manager
// half of those decisions. It owns the turn-state bridge (the daemon's
// internal/runtrace watcher by default): Hold arms it, CloseStep samples it
// and hands the observation to the supervisor's actor loop, and every path
// that ends or cancels a close tears the watch down again, so the daemon does
// no trace I/O outside a close.
//
// It also answers the pass's "when did this harness go out of hours" — the
// instant a close's deadline is anchored to (SPEC-0012 REQ "Graceful
// Shutdown"): the end of the lease that just expired where one governed, the
// end of the window that just closed otherwise.
//
// Governing: ADR-0019, SPEC-0012 REQ "Graceful Shutdown", REQ "Turn State
// Signal", REQ "Shutdown Mode"; design.md § "Turn state from a daemon-side
// watcher", § "Hold and Release on the Manager".
//
// @joestump-agent 09/21/2026 - Added for stump.wtf/harness#384.

import (
	"os"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/runtrace"
)

// TurnBridge is the Manager's seam to the live turn-state watch. Production
// wires runtrace.NewWatcher; tests inject a stub, which is how the no-trace-
// I/O-unless-near-a-close property is asserted. The Manager calls Follow only
// for a harness closing or within a minute of it.
type TurnBridge interface {
	Follow(name string, target runtrace.Scope, window runtrace.Window, peers []runtrace.Scope, closeAt time.Time)
	Unfollow(name string)
	Turn(name string) (runtrace.TurnState, bool)
	Unavailable(name string) string
}

// armSlack skips a re-arm the pass repeats every tick inside the same minute
// before a close: one synchronous attribution is enough to be warm.
const armSlack = time.Minute

// GateStatus reports whether name is up (starting, running, degraded or
// restarting — the states a close holds), whether it is held, and whether a
// graceful close is in flight, for the scheduler's operating-hours gate pass.
// ok is false for an unknown harness. Governing: SPEC-0012 REQ "Gate
// Enforcement", REQ "Graceful Shutdown".
func (m *Manager) GateStatus(name string) (up, held, closing, ok bool) {
	s := m.get(name)
	if s == nil {
		return false, false, false, false
	}
	snap := s.Snapshot()
	return snapUp(snap.State), snap.Held, snap.Closing, true
}

// Hold stops a gated harness for its operating hours without touching its
// enabled intent. Under HoursShutdownGraceful a running harness only marks
// its close and the Manager arms the turn-state watch for it; CloseStep
// finishes the close. closeAt anchors the close's deadline; the Manager
// consumes the lease record (if any) that just expired, since this hold is
// what enforces its end. Governing: ADR-0019, SPEC-0012 REQ "Gate
// Enforcement", REQ "Graceful Shutdown".
func (m *Manager) Hold(name string, mode core.HoursShutdownMode, closeAt time.Time) {
	m.mu.Lock()
	s := m.supervisors[name]
	delete(m.expiredLease, name) // consumed: this hold enforces the lease's end
	m.mu.Unlock()
	if s == nil {
		return
	}
	s.Hold(mode, closeAt)
	if snap := s.Snapshot(); snap.Closing {
		m.follow(name, closeAt)
	}
}

// CloseStep advances name's graceful close by one observation, sampled from
// the turn-state watch and decided on the supervisor's actor loop. Governing:
// SPEC-0012 REQ "Graceful Shutdown".
func (m *Manager) CloseStep(name string, now time.Time) {
	m.mu.Lock()
	s := m.supervisors[name]
	m.mu.Unlock()
	if s == nil {
		return
	}
	snap := s.Snapshot()
	if !snap.Closing {
		m.watch.Unfollow(name) // the close is over; drop any watch left behind
		return
	}
	ts, ok := m.watch.Turn(name)
	why := ""
	if !ok {
		why = m.watch.Unavailable(name)
	}
	s.CloseStep(now, ts, ok, why)
	if !s.Snapshot().Closing {
		m.watch.Unfollow(name) // the close ended: no more trace I/O
	}
}

// Arm warms the turn-state watch for a harness whose close is within a
// minute, so its first CloseStep already has a sample to decide on. It arms
// nothing for an immediate harness (nothing waits) and re-arms at most once
// per close instant, however many ticks land inside the minute. Governing:
// SPEC-0012 REQ "Turn State Signal".
func (m *Manager) Arm(name string, closeAt time.Time) {
	m.mu.Lock()
	h, known := m.cfg.Harnesses[name]
	armed, already := m.armedCloseAt[name]
	if !known || already && armed.Equal(closeAt) {
		m.mu.Unlock()
		return
	}
	graceful := h.HoursShutdown == core.HoursShutdownGraceful
	if m.armedCloseAt == nil {
		m.armedCloseAt = make(map[string]time.Time)
	}
	m.armedCloseAt[name] = closeAt
	m.mu.Unlock()
	if !graceful {
		return
	}
	m.follow(name, closeAt)
}

// CloseAt reports the instant name went out of hours, for the close the gate
// pass is about to decide: the end of the lease that just expired where one
// governed, otherwise the end of the window that just closed. ok is false
// when neither can be determined, and the close is then anchored to now
// instead. Governing: SPEC-0012 REQ "Graceful Shutdown" (the deadline is
// measured from the instant out of hours, not from when the daemon noticed).
func (m *Manager) CloseAt(name string, now time.Time) (time.Time, bool) {
	m.mu.Lock()
	exp, hadExp := m.expiredLease[name]
	h, known := m.cfg.Harnesses[name]
	m.mu.Unlock()
	if !known {
		return time.Time{}, false
	}
	prev, hadPrev := h.HoursExpr.PrevEnd(now)
	switch {
	case hadExp && (!hadPrev || !exp.Before(prev)):
		return exp, true
	case hadPrev:
		return prev, true
	}
	return time.Time{}, false
}

// follow builds the correlation inputs for name and arms the watch. It is the
// only place the Manager starts trace I/O for a harness, and every caller is
// on a close path: a hold that began a graceful close, or an arm inside the
// minute before one.
func (m *Manager) follow(name string, closeAt time.Time) {
	m.mu.Lock()
	s := m.supervisors[name]
	h, known := m.cfg.Harnesses[name]
	m.mu.Unlock()
	if s == nil || !known {
		return
	}
	target := runtrace.Scope{Name: name, Adapter: h.Adapter, Workdir: Workdir(h), Args: h.Args}
	env, err := DiscoveryEnv(h, runtrace.DiscoveryEnvKeys)
	if err != nil {
		log.Warn("turn-state watch cannot read env_file; discovering with the daemon's environment", "harness", name, "err", err)
	}
	target.Env = env
	snap := s.Snapshot()
	window := runtrace.Window{Start: snap.LastStarted}
	peers := m.peerScopes(target)
	log.Info("watching agent activity for a graceful close", "harness", name,
		"closes_at", closeAt.Format(time.RFC3339))
	m.watch.Follow(name, target, window, peers, closeAt)
}

// peerScopes builds the correlation peers for target: every other harness
// that could have written to the same workdir, with its run windows — the
// input that makes a session ambiguous. It is the Manager-side twin of the
// logs view's peer construction (internal/daemon), from the same records.
func (m *Manager) peerScopes(target runtrace.Scope) []runtrace.Scope {
	daemonDir, _ := os.Getwd()
	var peers []runtrace.Scope
	for _, snap := range m.Snapshots() {
		if snap.Name == target.Name {
			continue
		}
		h, _, ok := m.HarnessRecord(snap.Name)
		if !ok {
			continue
		}
		p := runtrace.Scope{Name: snap.Name, Adapter: h.Adapter, Workdir: Workdir(h), Args: h.Args}
		if p.Workdir == "" {
			p.Workdir = daemonDir
		}
		if !runtrace.CouldWrite(p.Adapter, target.Adapter) || !runtrace.SameDir(p.Workdir, target.Workdir) {
			continue
		}
		if !snap.LastStarted.IsZero() {
			w := runtrace.Window{Start: snap.LastStarted}
			if snap.LastExitAt.After(snap.LastStarted) {
				w.End = snap.LastExitAt
			}
			p.Runs = append(p.Runs, w)
		}
		lifecycle, err := ReadLifecycle(m.LogDir(), snap.Name, time.Time{})
		if err != nil {
			// History unreadable: treat the peer as running throughout, so
			// it can always claim a shared session — hiding our own state
			// beats wearing theirs.
			p.Runs = append(p.Runs, runtrace.Window{Start: time.Unix(1, 0)})
		}
		for _, s := range RunSpans(lifecycle) {
			p.Runs = append(p.Runs, runtrace.Window{Start: s.Start, End: s.End})
		}
		peers = append(peers, p)
	}
	return peers
}

// unfollowWatch tears the turn-state watch down for name after a close ended
// or was cancelled. Best-effort on every path that could leave one armed.
func (m *Manager) unfollowWatch(name string) {
	m.mu.Lock()
	delete(m.armedCloseAt, name)
	m.mu.Unlock()
	m.watch.Unfollow(name)
}

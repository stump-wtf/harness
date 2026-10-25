package supervisor

// Waiting For A Turn On A Manual Stop
//
// The operating-hours close is stepped by the scheduler's gate pass, because the
// pass is what notices the window closed. A stop the operator asked for has no
// pass to wait for, so this file supplies the steps: the Manager arms the same
// turn-state watch the gate arms, steps the close on its own tick, and returns
// once the harness is down. Nothing about the close's decision changes here; the
// supervisor still decides (hours.go), and an immediate stop, and every stop
// taken without the flag, follow the path they always did.
//
// The daemon-wide case is the same machinery entered all at once. `harness
// daemon stop --graceful` arms a drain on the running daemon before it sends
// SIGTERM; Manager.Close, which that signal leads to, then lets every resident
// harness finish its turn (bounded) before the normal shutdown sequence stops
// them. Nothing armed means no drain, so a plain SIGTERM and a `systemctl stop`
// keep today's timing exactly.
//
// Governing: ADR-0019 (operating hours, whose close this reuses), SPEC-0012 REQ
// "Graceful Shutdown" and REQ "Turn State Signal", SPEC-0003 REQ "Graceful
// Stop"; stump.wtf/harness#871.
//
// @joestump-agent 10/25/2026 - Added for stump.wtf/harness#871.

import (
	"time"

	"charm.land/log/v2"
)

// DefaultCloseTick is how often a close entered outside the gate pass is
// stepped while the caller waits. The close's own bounds (settle, quiet, cap)
// are all seconds to minutes apart, so this only decides how quickly a finished
// turn is noticed; tests shorten it through Manager.closeTick.
const DefaultCloseTick = 2 * time.Second

// StopGraceful is Stop that first waits for the harness's current turn to end,
// bounded by cap (zero: the harness's hours_shutdown_timeout, falling back to
// core.DefaultHoursShutdownTimeout), and then stops it exactly as an immediate
// stop would. It returns once the harness is down, or once the cap passed.
func (m *Manager) StopGraceful(name string, cap time.Duration) bool {
	return m.stopBy(name, "verb:stop", "", &gracefulClose{cap: cap, origin: closeOriginStop})
}

// StopGracefulPeer is StopGraceful naming the socket peer the verb came from
// (issue #835).
func (m *Manager) StopGracefulPeer(name, peer string, cap time.Duration) bool {
	return m.stopBy(name, "verb:stop", peer, &gracefulClose{cap: cap, origin: closeOriginStop})
}

// RestartGracefulPeer is RestartPeer that waits for the current turn to end
// before stopping and bringing the harness straight back up. Like RestartChecked
// it reports what the start decided; a restart the wait deferred reports no
// refusal, because the start that follows it is admitted when it runs.
func (m *Manager) RestartGracefulPeer(name, peer string, cap time.Duration) (ok bool, refused error) {
	s := m.get(name)
	if s == nil {
		return false, nil
	}
	s.RestartGracefulBy("verb:restart", peer, cap)
	if s.Snapshot().Closing {
		m.armWatch(name)
		m.stepCloses([]string{name})
		return true, nil
	}
	return true, s.restartChecked("verb:restart", peer).Refused
}

// ArmShutdownDrain asks this Manager's next Close to let every resident harness
// finish the turn it is in, bounded by cap, before anything is stopped. It is the
// daemon-wide form of `harness stop --graceful`, armed by `harness daemon stop
// --graceful` an instant before the CLI sends the SIGTERM that reaches Close
// (SPEC-0012 REQ "Graceful Shutdown"; stump.wtf/harness#871).
func (m *Manager) ArmShutdownDrain(cap time.Duration) {
	m.mu.Lock()
	m.drainArmed, m.drainCap = true, cap
	m.mu.Unlock()
	log.Info("daemon shutdown armed to drain turns before stopping", "cap", gracefulCap(cap))
}

// armWatch starts the turn-state watch for a close the Manager itself began,
// anchored to now: a manual stop's deadline is measured from the ask, not from
// some window that closed.
func (m *Manager) armWatch(name string) {
	now := m.now()
	if now.IsZero() {
		now = time.Now()
	}
	m.follow(name, now)
}

// drainTurns arms a drain for every harness that is up, as Manager.Close calls
// it before shutting anything down. Returns once no close is in flight.
func (m *Manager) drainTurns() {
	m.mu.Lock()
	armed, cap := m.drainArmed, m.drainCap
	m.mu.Unlock()
	if !armed {
		return
	}
	var names []string
	for _, s := range m.snapshotSupervisors() {
		if !snapUp(s.Snapshot().State) {
			continue
		}
		s.ArmDrain(cap)
		if s.Snapshot().Closing {
			names = append(names, s.Name())
		}
	}
	if len(names) == 0 {
		return
	}
	log.Info("draining turns before shutdown", "harnesses", len(names), "cap", gracefulCap(cap))
	for _, name := range names {
		m.armWatch(name)
	}
	m.stepCloses(names)
}

// stepCloses supplies the close steps the gate pass would otherwise supply,
// until none of names is closing. Each step passes the current time, so the
// supervisor's own deadline always ends the wait: a close cannot outlive this
// loop's patience, only the caller's patience with it.
func (m *Manager) stepCloses(names []string) {
	interval := m.closeTick
	if interval <= 0 {
		interval = DefaultCloseTick
	}
	tk := time.NewTicker(interval)
	defer tk.Stop()
	for {
		done := true
		for _, name := range names {
			m.CloseStep(name, m.now())
			if s := m.get(name); s != nil && s.Snapshot().Closing {
				done = false
			}
		}
		if done {
			return
		}
		select {
		case <-tk.C:
		case <-m.closed:
			return
		}
	}
}

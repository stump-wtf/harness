package core

// Governing: SPEC-0003 (harness-lifecycle) REQ "State Model"; ADR-0005
// (supervision). The seven-state machine every harness moves through under
// daemon supervision, plus the glyph table the TUI renders (SPEC-0001).

import "fmt"

// State is one of the seven lifecycle states a supervised harness can occupy.
// It is deliberately a string type so it serializes verbatim over the daemon
// protocol (SPEC-0002) and reads well in config/state files.
type State string

const (
	// StateStopped: not running, not wanted (or a disabled process exited).
	StateStopped State = "stopped"
	// StateStarting: spawn in progress, not yet confirmed up.
	StateStarting State = "starting"
	// StateRunning: process is up and healthy.
	StateRunning State = "running"
	// StateDegraded: crash-looping; restarts are escalating under backoff.
	StateDegraded State = "degraded"
	// StateRestarting: exited while enabled; waiting restart_delay before respawn.
	StateRestarting State = "restarting"
	// StateStopping: stop requested; SIGTERM sent, grace period running.
	StateStopping State = "stopping"
	// StateFailed: gave up after the backoff cap; needs a human (manual restart).
	StateFailed State = "failed"
)

// States is every valid state in canonical order.
var States = []State{
	StateStopped,
	StateStarting,
	StateRunning,
	StateDegraded,
	StateRestarting,
	StateStopping,
	StateFailed,
}

// glyphs is the exact glyph table from SPEC-0003 REQ "State Model". Three
// transient states (starting, restarting, stopping) intentionally share the
// hollow-dot glyph ◌.
var glyphs = map[State]string{
	StateStopped:    "○",
	StateStarting:   "◌",
	StateRunning:    "●",
	StateDegraded:   "◐",
	StateRestarting: "◌",
	StateStopping:   "◌",
	StateFailed:     "✖",
}

// Glyph returns the single-rune status glyph for the state (SPEC-0003).
func (s State) Glyph() string {
	return glyphs[s]
}

// Valid reports whether s is one of the seven defined states.
func (s State) Valid() bool {
	_, ok := glyphs[s]
	return ok
}

// String implements fmt.Stringer.
func (s State) String() string { return string(s) }

// ParseState parses a state string, rejecting anything outside the seven.
func ParseState(s string) (State, error) {
	st := State(s)
	if !st.Valid() {
		return "", fmt.Errorf("core: unknown lifecycle state %q", s)
	}
	return st, nil
}

// transitions encodes the legal edges of the SPEC-0003 state machine. Every
// scenario in the spec maps to an edge here:
//
//   - Autostart / manual start:        stopped    → starting
//   - Came up:                         starting   → running
//   - Exit while enabled (restart):    running|starting|degraded → restarting → starting
//   - Exit while disabled:             running|starting|degraded|restarting → stopped
//   - Crash-loop detected:             running    → degraded
//   - Recovery resets backoff:         degraded   → running
//   - Backoff give-up:                 degraded|starting → failed
//   - Graceful stop:                   running|degraded|restarting|starting → stopping → stopped
//   - Manual recovery from failed:     failed     → starting
var transitions = map[State]map[State]bool{
	StateStopped:    set(StateStarting),
	StateStarting:   set(StateRunning, StateRestarting, StateStopping, StateStopped, StateFailed),
	StateRunning:    set(StateDegraded, StateRestarting, StateStopping, StateStopped),
	StateDegraded:   set(StateRunning, StateRestarting, StateStopping, StateStopped, StateFailed),
	StateRestarting: set(StateStarting, StateStopping, StateStopped),
	StateStopping:   set(StateStopped),
	StateFailed:     set(StateStarting),
}

func set(states ...State) map[State]bool {
	m := make(map[State]bool, len(states))
	for _, s := range states {
		m[s] = true
	}
	return m
}

// CanTransitionTo reports whether a move from s to next is a legal edge of the
// SPEC-0003 state machine. A no-op (s == next) is always allowed.
func (s State) CanTransitionTo(next State) bool {
	if !s.Valid() || !next.Valid() {
		return false
	}
	if s == next {
		return true
	}
	return transitions[s][next]
}

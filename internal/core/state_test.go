package core

import "testing"

// TestStateGlyphs pins the exact glyph table from SPEC-0003 REQ "State Model".
// If a glyph changes, this test (and the spec) must change together.
func TestStateGlyphs(t *testing.T) {
	tests := []struct {
		state State
		glyph string
	}{
		{StateStopped, "○"},
		{StateStarting, "◌"},
		{StateRunning, "●"},
		{StateDegraded, "◐"},
		{StateRestarting, "◌"},
		{StateStopping, "◌"},
		{StateFailed, "✖"},
	}
	if len(tests) != len(States) {
		t.Fatalf("glyph table has %d entries, States has %d", len(tests), len(States))
	}
	for _, tt := range tests {
		if got := tt.state.Glyph(); got != tt.glyph {
			t.Errorf("%s glyph = %q, want %q", tt.state, got, tt.glyph)
		}
		if !tt.state.Valid() {
			t.Errorf("%s should be Valid()", tt.state)
		}
	}
}

func TestParseState(t *testing.T) {
	for _, s := range States {
		got, err := ParseState(string(s))
		if err != nil || got != s {
			t.Errorf("ParseState(%q) = %q, %v", s, got, err)
		}
	}
	if _, err := ParseState("nonsense"); err == nil {
		t.Error("ParseState(nonsense) should error")
	}
	if State("nonsense").Valid() {
		t.Error("bogus state should not be Valid()")
	}
	if State("nonsense").Glyph() != "" {
		t.Error("bogus state Glyph() should be empty")
	}
}

// TestTransitions covers every SPEC-0003 scenario as an explicit legal edge,
// plus a set of edges the machine must reject.
func TestTransitions(t *testing.T) {
	legal := []struct {
		name     string
		from, to State
	}{
		// Autostart / manual start (REQ Autostart, Manual recovery start path).
		{"start", StateStopped, StateStarting},
		// Came up.
		{"came up", StateStarting, StateRunning},
		// Restart on exit while enabled (REQ Restart On Exit).
		{"exit-while-enabled: running->restarting", StateRunning, StateRestarting},
		{"restart delay elapsed", StateRestarting, StateStarting},
		{"exit during start while enabled", StateStarting, StateRestarting},
		{"degraded keeps restarting", StateDegraded, StateRestarting},
		// Exit while disabled (REQ Restart On Exit, disabled path).
		{"exit-while-disabled: running->stopped", StateRunning, StateStopped},
		{"exit-while-disabled: starting->stopped", StateStarting, StateStopped},
		{"exit-while-disabled: restarting->stopped", StateRestarting, StateStopped},
		// Crash-loop detection + recovery (REQ Crash-Loop Detection).
		{"crash loop", StateRunning, StateDegraded},
		{"recovery resets", StateDegraded, StateRunning},
		// Backoff give-up (REQ Backoff Give-Up).
		{"give up", StateDegraded, StateFailed},
		{"hard fail on spawn", StateStarting, StateFailed},
		// Manual recovery from failed (REQ Backoff Give-Up, Manual recovery).
		{"manual restart clears failed", StateFailed, StateStarting},
		// Graceful stop (REQ Graceful Stop).
		{"stop running", StateRunning, StateStopping},
		{"stop degraded", StateDegraded, StateStopping},
		{"stop restarting", StateRestarting, StateStopping},
		{"stopping settles", StateStopping, StateStopped},
	}
	for _, tt := range legal {
		if !tt.from.CanTransitionTo(tt.to) {
			t.Errorf("%s: %s->%s should be legal", tt.name, tt.from, tt.to)
		}
	}

	illegal := []struct {
		from, to State
	}{
		{StateStopped, StateRunning},    // must go through starting
		{StateStopped, StateFailed},     // can't fail without trying
		{StateFailed, StateRunning},     // must restart first
		{StateStopping, StateRunning},   // stopping only settles to stopped
		{StateRunning, StateStarting},   // already up
		{StateStopped, StateStopping},   // nothing to stop
		{StateStarting, State("bogus")}, // unknown target
		{State("bogus"), StateRunning},  // unknown source
	}
	for _, tt := range illegal {
		if tt.from.CanTransitionTo(tt.to) {
			t.Errorf("%s->%s should be illegal", tt.from, tt.to)
		}
	}

	// A no-op transition is always allowed (idempotent set-state).
	for _, s := range States {
		if !s.CanTransitionTo(s) {
			t.Errorf("%s->%s (no-op) should be legal", s, s)
		}
	}
}

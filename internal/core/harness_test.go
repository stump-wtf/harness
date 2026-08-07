package core

import "testing"

// TestRestartPolicyValid pins the accepted policy set: the four named values
// plus the empty default; anything else is rejected at the config layer.
func TestRestartPolicyValid(t *testing.T) {
	for _, p := range []RestartPolicy{"", RestartAlways, RestartNo, RestartUnlessStopped, RestartOnFailure} {
		if !p.Valid() {
			t.Errorf("Valid(%q) = false, want true", p)
		}
	}
	for _, p := range []RestartPolicy{"until-pigs-fly", "Always", "on-failure:3", " no"} {
		if p.Valid() {
			t.Errorf("Valid(%q) = true, want false", p)
		}
	}
}

// TestRestartPolicyShouldRestart is the full policy × exit-code truth table,
// including the spawn-failure/signal code (-1) that the integration tests
// cannot produce on demand.
func TestRestartPolicyShouldRestart(t *testing.T) {
	tests := []struct {
		policy RestartPolicy
		code   int
		want   bool
	}{
		// Default (empty) and always/unless-stopped restart on any exit.
		{"", 0, true},
		{"", 1, true},
		{"", -1, true},
		{RestartAlways, 0, true},
		{RestartAlways, 1, true},
		{RestartAlways, -1, true},
		{RestartUnlessStopped, 0, true},
		{RestartUnlessStopped, 1, true},
		{RestartUnlessStopped, -1, true},
		// "no" never restarts, whatever the exit looked like.
		{RestartNo, 0, false},
		{RestartNo, 1, false},
		{RestartNo, -1, false},
		// "on-failure" skips clean exits only; spawn failure (-1) is a failure.
		{RestartOnFailure, 0, false},
		{RestartOnFailure, 1, true},
		{RestartOnFailure, -1, true},
	}
	for _, tt := range tests {
		if got := tt.policy.ShouldRestart(tt.code); got != tt.want {
			t.Errorf("RestartPolicy(%q).ShouldRestart(%d) = %v, want %v", tt.policy, tt.code, got, tt.want)
		}
	}
}

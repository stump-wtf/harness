package tui

import (
	"strings"
	"testing"
)

// TestNeedsConfirm verifies the SPEC-0001 REQ "Confirmation Guards": stop,
// restart, and delete are guarded; start is not; and the --yes-style setting
// skips every guard.
func TestNeedsConfirm(t *testing.T) {
	cases := []struct {
		a    Action
		skip bool
		want bool
	}{
		{ActionStop, false, true},
		{ActionRestart, false, true},
		{ActionDelete, false, true},
		{ActionStart, false, false},  // start is non-destructive
		{ActionStop, true, false},    // --yes skips
		{ActionRestart, true, false}, // --yes skips
	}
	for _, c := range cases {
		if got := needsConfirm(c.a, c.skip); got != c.want {
			t.Errorf("needsConfirm(%s, skip=%v) = %v, want %v", c.a, c.skip, got, c.want)
		}
	}
}

// TestConfirmPrompt spot-checks the human prompts name the target.
func TestConfirmPrompt(t *testing.T) {
	if !strings.Contains(confirmPrompt(ActionStop, "crush"), "crush") {
		t.Error("stop prompt should name the target")
	}
	if !strings.Contains(confirmPrompt(ActionDelete, "crush"), "harness.toml") {
		t.Error("delete prompt should mention the config file")
	}
}

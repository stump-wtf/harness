package config

import (
	"strings"
	"testing"
)

// TestScheduleRequiresPrompt verifies the schedule field requires prompt.
func TestScheduleRequiresPrompt(t *testing.T) {
	toml := `
[harness.scheduled]
cmd = "echo hi"
schedule = "0 */6 * * *"
`
	_, err := Parse([]byte(toml), "test.toml")
	if err == nil {
		t.Fatal("expected error for schedule without prompt")
	}
	if !strings.Contains(err.Error(), `"schedule" requires "prompt"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestScheduleMutuallyExclusiveWithEnabled verifies schedule and enabled=true
// are rejected together.
func TestScheduleMutuallyExclusiveWithEnabled(t *testing.T) {
	toml := `
[harness.scheduled]
prompt = "do the thing"
schedule = "0 */6 * * *"
enabled = true
`
	_, err := Parse([]byte(toml), "test.toml")
	if err == nil {
		t.Fatal("expected error for schedule + enabled = true")
	}
	if !strings.Contains(err.Error(), `"schedule" and "enabled = true" are mutually exclusive`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestScheduleAcceptedWithPrompt verifies a valid scheduled prompt harness
// parses cleanly.
func TestScheduleAcceptedWithPrompt(t *testing.T) {
	toml := `
[harness.scheduled]
prompt = "do the thing"
schedule = "0 */6 * * *"
`
	cfg, err := Parse([]byte(toml), "test.toml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	h, ok := cfg.Harnesses["scheduled"]
	if !ok {
		t.Fatal("harness not found")
	}
	if h.Schedule != "0 */6 * * *" {
		t.Errorf("schedule = %q, want %q", h.Schedule, "0 */6 * * *")
	}
	if h.Enabled {
		t.Error("scheduled harness should not be enabled")
	}
}

// TestScheduleDisabledWithEnabledFalse verifies schedule + enabled = false is
// accepted (the mutual exclusion is only with enabled = true).
func TestScheduleDisabledWithEnabledFalse(t *testing.T) {
	toml := `
[harness.scheduled]
prompt = "do the thing"
schedule = "0 */6 * * *"
enabled = false
`
	cfg, err := Parse([]byte(toml), "test.toml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	h, ok := cfg.Harnesses["scheduled"]
	if !ok {
		t.Fatal("harness not found")
	}
	if h.Schedule == "" {
		t.Error("schedule should be set")
	}
}

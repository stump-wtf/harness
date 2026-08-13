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

// TestScheduleBlankRejected verifies a present-but-whitespace schedule is a
// loud error, not a silent no-schedule (mirrors the blank-prompt/model idiom).
func TestScheduleBlankRejected(t *testing.T) {
	toml := `
[harness.scheduled]
prompt = "do the thing"
schedule = "  "
`
	_, err := Parse([]byte(toml), "test.toml")
	if err == nil {
		t.Fatal("expected error for blank schedule")
	}
	if !strings.Contains(err.Error(), `"schedule" must not be blank`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestScheduleInvalidCronRejected verifies the cron expression is validated at
// parse time — a typo fails the load with a located error instead of silently
// never firing.
func TestScheduleInvalidCronRejected(t *testing.T) {
	for _, spec := range []string{"not-a-cron", "0 */6 * *", "@evry 6h"} {
		toml := `
[harness.scheduled]
prompt = "do the thing"
schedule = "` + spec + `"
`
		_, err := Parse([]byte(toml), "test.toml")
		if err == nil {
			t.Fatalf("expected error for invalid schedule %q", spec)
		}
		if !strings.Contains(err.Error(), `invalid "schedule"`) {
			t.Fatalf("unexpected error for %q: %v", spec, err)
		}
	}
}

// TestScheduleRestartAlwaysRejected verifies schedule cannot combine with a
// restart policy that respawns on clean exit — that would turn the one-shot
// into a perpetual service and make the schedule meaningless.
func TestScheduleRestartAlwaysRejected(t *testing.T) {
	for _, policy := range []string{"always", "unless-stopped"} {
		toml := `
[harness.scheduled]
prompt = "do the thing"
schedule = "0 */6 * * *"
restart = "` + policy + `"
`
		_, err := Parse([]byte(toml), "test.toml")
		if err == nil {
			t.Fatalf("expected error for schedule + restart = %q", policy)
		}
		if !strings.Contains(err.Error(), `restart policy "no" or "on-failure"`) {
			t.Fatalf("unexpected error for %q: %v", policy, err)
		}
	}
}

// TestScheduleRestartOnFailureAccepted verifies the documented pairing —
// restart applies only to abnormal exit — still parses.
func TestScheduleRestartOnFailureAccepted(t *testing.T) {
	toml := `
[harness.scheduled]
prompt = "do the thing"
schedule = "0 */6 * * *"
restart = "on-failure"
`
	if _, err := Parse([]byte(toml), "test.toml"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestScheduleInProfileRejected verifies a scheduled harness cannot be a
// profile member: profile autostart / use-profile would fire the one-shot
// outside its schedule.
func TestScheduleInProfileRejected(t *testing.T) {
	toml := `
[harness.scheduled]
prompt = "do the thing"
schedule = "0 */6 * * *"

[profile.default]
harnesses = ["scheduled"]
autostart = true
`
	_, err := Parse([]byte(toml), "test.toml")
	if err == nil {
		t.Fatal("expected error for scheduled harness in profile")
	}
	if !strings.Contains(err.Error(), "profile membership are mutually exclusive") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestScheduleInProjectFileRejected verifies project files reject schedule
// outright: project harnesses never enter the daemon's config view, so a
// project schedule could never fire.
func TestScheduleInProjectFileRejected(t *testing.T) {
	toml := `
[harness.sweep]
prompt = "do the thing"
schedule = "0 */6 * * *"
`
	_, err := ParseProject([]byte(toml), "/tmp/proj/.harness.toml")
	if err == nil {
		t.Fatal("expected error for schedule in project file")
	}
	if !strings.Contains(err.Error(), `"schedule" is not supported in project files`) {
		t.Fatalf("unexpected error: %v", err)
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

package config

// Governing: ADR-0019, SPEC-0012 REQ "Operating Hours Key", REQ "Operating
// Hours Exclusions", REQ "Shutdown Mode" — every WHEN/THEN scenario for the
// three new config keys, as table tests in the same style as schedule_test.go.

import (
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// TestOperatingHoursAccepted verifies a valid expression parses and is
// exposed on the harness for #382 to consume, with the shutdown-mode
// defaults in place.
func TestOperatingHoursAccepted(t *testing.T) {
	toml := `
[harness.night-owl]
harness = "claude-code"
args = ["--remote-control"]
enabled = true
operating_hours = "TZ=America/Los_Angeles Mon-Fri 09:00-13:00"
`
	cfg, err := Parse([]byte(toml), "test.toml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	h, ok := cfg.Harnesses["night-owl"]
	if !ok {
		t.Fatal("harness not found")
	}
	if h.OperatingHours != "TZ=America/Los_Angeles Mon-Fri 09:00-13:00" {
		t.Errorf("OperatingHours = %q", h.OperatingHours)
	}
	loc, _ := time.LoadLocation("America/Los_Angeles")
	in, _, _ := h.HoursExpr.In(time.Date(2026, 9, 21, 9, 0, 0, 0, loc)) // Monday
	if !in {
		t.Error("HoursExpr should report in hours at Monday 09:00 Pacific")
	}
	if h.HoursShutdown != core.HoursShutdownGraceful {
		t.Errorf("HoursShutdown = %q, want graceful (default)", h.HoursShutdown)
	}
	if h.HoursShutdownTimeout != 15*time.Minute {
		t.Errorf("HoursShutdownTimeout = %v, want 15m (default)", h.HoursShutdownTimeout)
	}
}

// TestOperatingHoursBlankRejected mirrors TestScheduleBlankRejected — a
// present-but-whitespace value is a loud error, not a silent no-gate.
func TestOperatingHoursBlankRejected(t *testing.T) {
	toml := `
[harness.night-owl]
harness = "generic"
args = ["-c", "true"]
operating_hours = "   "
`
	_, err := Parse([]byte(toml), "test.toml")
	if err == nil {
		t.Fatal("expected error for blank operating_hours")
	}
	if !strings.Contains(err.Error(), `"operating_hours" must not be blank`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestOperatingHoursInvalidGrammarRejected verifies the expression is
// validated (and internal/hours' error surfaces) at load time.
func TestOperatingHoursInvalidGrammarRejected(t *testing.T) {
	for _, spec := range []string{"Mon 09:00-09:00", "TZ=Mars/Olympus 09:00-13:00", "not a grammar"} {
		toml := `
[harness.night-owl]
harness = "generic"
args = ["-c", "true"]
operating_hours = "` + spec + `"
`
		_, err := Parse([]byte(toml), "test.toml")
		if err == nil {
			t.Fatalf("expected error for operating_hours %q", spec)
		}
		if !strings.Contains(err.Error(), `invalid "operating_hours"`) {
			t.Fatalf("unexpected error for %q: %v", spec, err)
		}
	}
}

// Scenario: Equal start and end names the harness and operating_hours.
func TestOperatingHoursEqualStartEndNamesHarnessAndKey(t *testing.T) {
	toml := `
[harness.night-owl]
harness = "generic"
args = ["-c", "true"]
operating_hours = "Mon 09:00-09:00"
`
	_, err := Parse([]byte(toml), "test.toml")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, `"night-owl"`) || !strings.Contains(msg, `"operating_hours"`) {
		t.Fatalf("error does not name the harness and key: %v", err)
	}
}

// Scenario: Unknown zone names the harness and the zone.
func TestOperatingHoursUnknownZoneNamesHarnessAndZone(t *testing.T) {
	toml := `
[harness.night-owl]
harness = "generic"
args = ["-c", "true"]
operating_hours = "TZ=Mars/Olympus 09:00-13:00"
`
	_, err := Parse([]byte(toml), "test.toml")
	if err == nil {
		t.Fatal("expected error")
	}
	msg := err.Error()
	if !strings.Contains(msg, `"night-owl"`) || !strings.Contains(msg, "Mars/Olympus") {
		t.Fatalf("error does not name the harness and zone: %v", err)
	}
}

// Scenario: Hours on a scheduled harness.
func TestOperatingHoursMutuallyExclusiveWithSchedule(t *testing.T) {
	toml := `
[harness.sweep]
harness = "crush"
prompt = "do the thing"
schedule = "0 */6 * * *"
operating_hours = "09:00-13:00"
`
	_, err := Parse([]byte(toml), "test.toml")
	if err == nil {
		t.Fatal("expected error for schedule + operating_hours")
	}
	if !strings.Contains(err.Error(), `"schedule" and "operating_hours" are mutually exclusive`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Scenario: Hours on a project harness.
func TestOperatingHoursInProjectFileRejected(t *testing.T) {
	toml := `
[harness.night-owl]
harness = "generic"
args = ["-c", "true"]
operating_hours = "09:00-13:00"
`
	_, err := ParseProject([]byte(toml), "/tmp/proj/harness.toml")
	if err == nil {
		t.Fatal("expected error for operating_hours in project file")
	}
	if !strings.Contains(err.Error(), `"operating_hours" is not supported in project files`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Scenario: Hours on a profile member — the config loads; the profile
// decides `enabled`, the hours decide when it runs.
func TestOperatingHoursAcceptedOnProfileMember(t *testing.T) {
	toml := `
[harness.night-owl]
harness = "claude-code"
args = ["--remote-control"]
operating_hours = "09:00-13:00"

[profile.default]
harnesses = ["night-owl"]
autostart = true
`
	cfg, err := Parse([]byte(toml), "test.toml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Harnesses["night-owl"].OperatingHours == "" {
		t.Error("expected operating_hours to be set")
	}
}

// operating_hours accepted alongside enabled, any restart, cmd, and a prompt
// harness without schedule (REQ "Operating Hours Exclusions").
func TestOperatingHoursAcceptedWithEnabledRestartAndCmd(t *testing.T) {
	toml := `
[harness.night-owl]
harness = "generic"
args = ["-c", "while true; do sleep 1; done"]
enabled = true
restart = "unless-stopped"
operating_hours = "09:00-13:00"
`
	if _, err := Parse([]byte(toml), "test.toml"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestOperatingHoursAcceptedOnPromptHarnessWithoutSchedule(t *testing.T) {
	toml := `
[harness.night-owl]
harness = "crush"
prompt = "stay up during business hours"
operating_hours = "09:00-13:00"
`
	if _, err := Parse([]byte(toml), "test.toml"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Scenario: Shutdown mode with no hours.
func TestHoursShutdownRequiresOperatingHours(t *testing.T) {
	toml := `
[harness.night-owl]
harness = "generic"
args = ["-c", "true"]
hours_shutdown = "immediate"
`
	_, err := Parse([]byte(toml), "test.toml")
	if err == nil {
		t.Fatal("expected error for hours_shutdown without operating_hours")
	}
	if !strings.Contains(err.Error(), `"hours_shutdown" requires "operating_hours"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHoursShutdownTimeoutRequiresOperatingHours(t *testing.T) {
	toml := `
[harness.night-owl]
harness = "generic"
args = ["-c", "true"]
hours_shutdown_timeout = "10m"
`
	_, err := Parse([]byte(toml), "test.toml")
	if err == nil {
		t.Fatal("expected error for hours_shutdown_timeout without operating_hours")
	}
	if !strings.Contains(err.Error(), `"hours_shutdown_timeout" requires "operating_hours"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// hours_shutdown = "graceful" (== the default) without operating_hours is
// still rejected — presence, not value, drives the exclusion.
func TestHoursShutdownExplicitDefaultStillRequiresOperatingHours(t *testing.T) {
	toml := `
[harness.night-owl]
harness = "generic"
args = ["-c", "true"]
hours_shutdown = "graceful"
`
	_, err := Parse([]byte(toml), "test.toml")
	if err == nil {
		t.Fatal("expected error even though \"graceful\" is the default value")
	}
	if !strings.Contains(err.Error(), `"hours_shutdown" requires "operating_hours"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Scenario: Default mode.
func TestHoursShutdownDefault(t *testing.T) {
	toml := `
[harness.night-owl]
harness = "generic"
args = ["-c", "true"]
operating_hours = "09:00-13:00"
`
	cfg, err := Parse([]byte(toml), "test.toml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	h := cfg.Harnesses["night-owl"]
	if h.HoursShutdown != core.HoursShutdownGraceful {
		t.Errorf("HoursShutdown = %q, want graceful", h.HoursShutdown)
	}
	if h.HoursShutdownTimeout != 15*time.Minute {
		t.Errorf("HoursShutdownTimeout = %v, want 15m", h.HoursShutdownTimeout)
	}
}

// Scenario: Immediate mode.
func TestHoursShutdownImmediate(t *testing.T) {
	toml := `
[harness.night-owl]
harness = "generic"
args = ["-c", "true"]
operating_hours = "09:00-13:00"
hours_shutdown = "immediate"
`
	cfg, err := Parse([]byte(toml), "test.toml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Harnesses["night-owl"].HoursShutdown != core.HoursShutdownImmediate {
		t.Errorf("HoursShutdown = %q, want immediate", cfg.Harnesses["night-owl"].HoursShutdown)
	}
}

func TestHoursShutdownInvalidValueRejected(t *testing.T) {
	toml := `
[harness.night-owl]
harness = "generic"
args = ["-c", "true"]
operating_hours = "09:00-13:00"
hours_shutdown = "whenever"
`
	_, err := Parse([]byte(toml), "test.toml")
	if err == nil {
		t.Fatal("expected error for invalid hours_shutdown")
	}
	if !strings.Contains(err.Error(), `invalid "hours_shutdown"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// Scenario: Invalid timeout.
func TestHoursShutdownTimeoutZeroRejected(t *testing.T) {
	toml := `
[harness.night-owl]
harness = "generic"
args = ["-c", "true"]
operating_hours = "09:00-13:00"
hours_shutdown_timeout = "0"
`
	_, err := Parse([]byte(toml), "test.toml")
	if err == nil {
		t.Fatal("expected error for hours_shutdown_timeout = 0")
	}
	if !strings.Contains(err.Error(), `invalid "hours_shutdown_timeout"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHoursShutdownTimeoutNegativeRejected(t *testing.T) {
	toml := `
[harness.night-owl]
harness = "generic"
args = ["-c", "true"]
operating_hours = "09:00-13:00"
hours_shutdown_timeout = "-5m"
`
	_, err := Parse([]byte(toml), "test.toml")
	if err == nil {
		t.Fatal("expected error for negative hours_shutdown_timeout")
	}
	if !strings.Contains(err.Error(), `invalid "hours_shutdown_timeout"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHoursShutdownTimeoutUnparseableRejected(t *testing.T) {
	toml := `
[harness.night-owl]
harness = "generic"
args = ["-c", "true"]
operating_hours = "09:00-13:00"
hours_shutdown_timeout = "soon"
`
	_, err := Parse([]byte(toml), "test.toml")
	if err == nil {
		t.Fatal("expected error for unparseable hours_shutdown_timeout")
	}
	if !strings.Contains(err.Error(), `invalid "hours_shutdown_timeout"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHoursShutdownTimeoutCustomAccepted(t *testing.T) {
	toml := `
[harness.night-owl]
harness = "generic"
args = ["-c", "true"]
operating_hours = "09:00-13:00"
hours_shutdown_timeout = "30m"
`
	cfg, err := Parse([]byte(toml), "test.toml")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := cfg.Harnesses["night-owl"].HoursShutdownTimeout; got != 30*time.Minute {
		t.Errorf("HoursShutdownTimeout = %v, want 30m", got)
	}
}

package tui

// Governing: SPEC-0008 REQ "Schedule Round-Trip Through Config Writers", REQ
// "Missed Window Handling"; issue #117.

import (
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/harness/internal/config"
)

// TestHarnessFormRoundTripCatchUp: a zone-prefixed schedule with catch_up
// serializes both keys and re-parses into the same harness.
func TestHarnessFormRoundTripCatchUp(t *testing.T) {
	f := NewHarnessForm()
	f.Name = "sweep"
	f.Harness = "crush"
	f.Prompt = "sweep the fleet"
	f.Schedule = "CRON_TZ=UTC 0 9 * * *"
	f.CatchUp = true
	if err := f.Validate(); err != nil {
		t.Fatalf("valid form rejected: %v", err)
	}
	body := f.TOML()
	if !strings.Contains(body, "catch_up = true\n") {
		t.Fatalf("TOML missing catch_up:\n%s", body)
	}
	cfg, err := config.Parse([]byte(body), "harness.toml")
	if err != nil {
		t.Fatalf("rendered TOML does not parse: %v\n%s", err, body)
	}
	h := cfg.Harnesses["sweep"]
	if !h.CatchUp || h.Schedule != "CRON_TZ=UTC 0 9 * * *" {
		t.Errorf("round-trip = schedule %q catch_up %v", h.Schedule, h.CatchUp)
	}

	f.CatchUp = false
	if strings.Contains(f.TOML(), "catch_up") {
		t.Error("catch_up = false (the default) was emitted")
	}
}

// TestHarnessFormCatchUpRequiresSchedule: the form mirrors the parser, so it
// cannot write a catch_up the daemon would refuse to load.
func TestHarnessFormCatchUpRequiresSchedule(t *testing.T) {
	f := NewHarnessForm()
	f.Name = "sweep"
	f.Harness = "crush"
	f.Prompt = "sweep the fleet"
	f.CatchUp = true
	err := f.Validate()
	if err == nil || !strings.Contains(err.Error(), "catch_up requires schedule") {
		t.Errorf("Validate() = %v, want catch_up requires schedule", err)
	}
}

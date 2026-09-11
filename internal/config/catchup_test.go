package config

// Governing: ADR-0013; SPEC-0008 REQ "Missed Window Handling", REQ "Schedule
// Time Zone"; issue #117.

import (
	"strings"
	"testing"
	_ "time/tzdata" // named zones must not depend on the runner's zoneinfo
)

// TestCatchUpParses verifies catch_up is carried onto the harness, defaulting
// to false.
func TestCatchUpParses(t *testing.T) {
	for _, tc := range []struct {
		name, line string
		want       bool
	}{
		{"absent defaults to false", "", false},
		{"explicit true", "catch_up = true", true},
		{"explicit false", "catch_up = false", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			toml := "[harness.sweep]\nharness = \"crush\"\nprompt = \"sweep\"\nschedule = \"0 3 * * *\"\n" + tc.line + "\n"
			cfg, err := Parse([]byte(toml), "test.toml")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got := cfg.Harnesses["sweep"].CatchUp; got != tc.want {
				t.Errorf("CatchUp = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestCatchUpRequiresSchedule verifies catch_up without a schedule is a
// located error, whether it is true or false — a key that does nothing is a
// mistake, not a no-op.
func TestCatchUpRequiresSchedule(t *testing.T) {
	for _, value := range []string{"true", "false"} {
		toml := "[harness.sweep]\nharness = \"crush\"\nprompt = \"sweep\"\ncatch_up = " + value + "\n"
		_, err := Parse([]byte(toml), "test.toml")
		if err == nil {
			t.Fatalf("catch_up = %s without schedule parsed", value)
		}
		if !strings.Contains(err.Error(), `"catch_up" requires "schedule"`) || !strings.Contains(err.Error(), `"sweep"`) {
			t.Errorf("catch_up = %s: unexpected error: %v", value, err)
		}
	}
}

// TestCatchUpInProjectFileRejected verifies project files reject catch_up for
// the same reason they reject schedule.
func TestCatchUpInProjectFileRejected(t *testing.T) {
	toml := "[harness.sweep]\nharness = \"crush\"\nprompt = \"sweep\"\ncatch_up = true\n"
	_, err := ParseProject([]byte(toml), "/tmp/proj/.harness.toml")
	if err == nil {
		t.Fatal("expected error for catch_up in a project file")
	}
	if !strings.Contains(err.Error(), `"catch_up" is not supported in project files`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestScheduleZonePrefixAccepted verifies robfig/cron's CRON_TZ= and TZ=
// prefixes validate and are kept verbatim, so the scheduler reads the zone
// from the same string the operator wrote.
func TestScheduleZonePrefixAccepted(t *testing.T) {
	for _, spec := range []string{
		"CRON_TZ=UTC 0 9 * * *",
		"TZ=UTC 0 9 * * *",
		"CRON_TZ=America/New_York 30 2 * * *",
		"CRON_TZ=UTC @every 6h",
		"CRON_TZ=UTC @daily",
	} {
		toml := "[harness.sweep]\nharness = \"crush\"\nprompt = \"sweep\"\nschedule = \"" + spec + "\"\n"
		cfg, err := Parse([]byte(toml), "test.toml")
		if err != nil {
			t.Errorf("%q: unexpected error: %v", spec, err)
			continue
		}
		if got := cfg.Harnesses["sweep"].Schedule; got != spec {
			t.Errorf("Schedule = %q, want %q verbatim", got, spec)
		}
	}
}

// TestScheduleUnknownZoneRejected verifies a zone that does not exist fails
// the load with the same located error as any other bad expression.
func TestScheduleUnknownZoneRejected(t *testing.T) {
	toml := "[harness.sweep]\nharness = \"crush\"\nprompt = \"sweep\"\nschedule = \"CRON_TZ=Mars/Olympus_Mons 0 9 * * *\"\n"
	_, err := Parse([]byte(toml), "test.toml")
	if err == nil {
		t.Fatal("expected error for an unknown zone")
	}
	if !strings.Contains(err.Error(), `invalid "schedule"`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

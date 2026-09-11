package config

// Governing: ADR-0013; SPEC-0008 REQ "Run Timeout", REQ "Overlap Policy", REQ
// "Run History"; issue #119.

import (
	"strings"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

func scheduledTOML(extra string) string {
	return "[harness.sweep]\nharness = \"crush\"\nprompt = \"sweep\"\nschedule = \"0 3 * * *\"\n" + extra + "\n"
}

// TestRunKeysDefaultOnScheduledHarness: every scheduled harness has a bounded
// run and a bounded history without asking; an unscheduled one has neither.
func TestRunKeysDefaultOnScheduledHarness(t *testing.T) {
	cfg, err := Parse([]byte(scheduledTOML("")), "test.toml")
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.Harnesses["sweep"]
	if h.Timeout != core.DefaultRunTimeout || h.OnOverlap != core.OverlapSkip || h.KeepRuns != core.DefaultKeepRuns {
		t.Errorf("defaults = timeout %v, on_overlap %q, keep_runs %d", h.Timeout, h.OnOverlap, h.KeepRuns)
	}

	cfg, err = Parse([]byte("[harness.agent]\nharness = \"crush\"\nprompt = \"once\"\n"), "test.toml")
	if err != nil {
		t.Fatal(err)
	}
	if a := cfg.Harnesses["agent"]; a.Timeout != 0 || a.OnOverlap != "" || a.KeepRuns != 0 {
		t.Errorf("unscheduled harness got run settings: %+v", a)
	}
}

// TestRunKeysParse covers the accepted spellings.
func TestRunKeysParse(t *testing.T) {
	cases := []struct {
		line  string
		check func(core.Harness) bool
	}{
		{`timeout = "45m"`, func(h core.Harness) bool { return h.Timeout == 45*time.Minute }},
		{`timeout = "2h30m"`, func(h core.Harness) bool { return h.Timeout == 150*time.Minute }},
		{`timeout = "0"`, func(h core.Harness) bool { return h.Timeout == 0 }},
		{`on_overlap = "skip"`, func(h core.Harness) bool { return h.OnOverlap == core.OverlapSkip }},
		{`on_overlap = "queue"`, func(h core.Harness) bool { return h.OnOverlap == core.OverlapQueue }},
		{`on_overlap = "replace"`, func(h core.Harness) bool { return h.OnOverlap == core.OverlapReplace }},
		{`keep_runs = 5`, func(h core.Harness) bool { return h.KeepRuns == 5 }},
	}
	for _, tc := range cases {
		cfg, err := Parse([]byte(scheduledTOML(tc.line)), "test.toml")
		if err != nil {
			t.Errorf("%s: %v", tc.line, err)
			continue
		}
		if h := cfg.Harnesses["sweep"]; !tc.check(h) {
			t.Errorf("%s: parsed as %+v", tc.line, h)
		}
	}
}

// TestRunKeysRejected covers located errors for bad values.
func TestRunKeysRejected(t *testing.T) {
	cases := []struct{ line, want string }{
		{`timeout = "soon"`, `invalid "timeout" "soon"`},
		{`timeout = "-5m"`, `invalid "timeout" "-5m"`},
		{`timeout = ""`, `invalid "timeout" ""`},
		{`on_overlap = "stack"`, `invalid "on_overlap" "stack"`},
		{`on_overlap = ""`, `invalid "on_overlap" ""`},
		{`keep_runs = 0`, `"keep_runs" must be at least 1`},
		{`keep_runs = -3`, `"keep_runs" must be at least 1`},
	}
	for _, tc := range cases {
		_, err := Parse([]byte(scheduledTOML(tc.line)), "test.toml")
		if err == nil || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), `"sweep"`) {
			t.Errorf("%s: error = %v, want it to contain %q and name the harness", tc.line, err, tc.want)
		}
	}
}

// TestRunKeysRequireSchedule: each key is a mistake without a schedule.
func TestRunKeysRequireSchedule(t *testing.T) {
	for key, line := range map[string]string{
		"timeout":    `timeout = "1h"`,
		"on_overlap": `on_overlap = "queue"`,
		"keep_runs":  `keep_runs = 5`,
	} {
		toml := "[harness.sweep]\nharness = \"crush\"\nprompt = \"sweep\"\n" + line + "\n"
		_, err := Parse([]byte(toml), "test.toml")
		if err == nil || !strings.Contains(err.Error(), `"`+key+`" requires "schedule"`) {
			t.Errorf("%s without schedule: error = %v", key, err)
		}
	}
}

// TestRunKeysInProjectFileRejected: project files cannot schedule, so they
// cannot shape scheduled runs either.
func TestRunKeysInProjectFileRejected(t *testing.T) {
	for key, line := range map[string]string{
		"timeout":    `timeout = "1h"`,
		"on_overlap": `on_overlap = "queue"`,
		"keep_runs":  `keep_runs = 5`,
	} {
		toml := "[harness.sweep]\nharness = \"crush\"\nprompt = \"sweep\"\n" + line + "\n"
		_, err := ParseProject([]byte(toml), "/tmp/proj/.harness.toml")
		if err == nil || !strings.Contains(err.Error(), `"`+key+`" is not supported in project files`) {
			t.Errorf("%s in a project file: error = %v", key, err)
		}
	}
}

package tui

// Governing: SPEC-0008 REQ "Schedule Round-Trip Through Config Writers", REQ
// "Run Timeout", REQ "Overlap Policy", REQ "Run History"; issue #119.

import (
	"strings"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

func scheduledForm() HarnessForm {
	f := NewHarnessForm()
	f.Name = "sweep"
	f.Harness = "crush"
	f.Prompt = "sweep the fleet"
	f.Schedule = "CRON_TZ=UTC 0 3 * * *"
	return f
}

// TestHarnessFormRoundTripRunKeys: non-default run keys serialize and re-parse;
// defaults are not written at all.
func TestHarnessFormRoundTripRunKeys(t *testing.T) {
	f := scheduledForm()
	f.Timeout = "45m"
	f.OnOverlap = "replace"
	f.KeepRuns = 5
	if err := f.Validate(); err != nil {
		t.Fatalf("valid form rejected: %v", err)
	}
	body := f.TOML()
	cfg, err := config.Parse([]byte(body), "harness.toml")
	if err != nil {
		t.Fatalf("rendered TOML does not parse: %v\n%s", err, body)
	}
	h := cfg.Harnesses["sweep"]
	if h.Timeout != 45*time.Minute || h.OnOverlap != core.OverlapReplace || h.KeepRuns != 5 {
		t.Errorf("round-trip = timeout %v, on_overlap %q, keep_runs %d", h.Timeout, h.OnOverlap, h.KeepRuns)
	}

	defaults := scheduledForm()
	defaults.OnOverlap = "skip"
	defaults.KeepRuns = core.DefaultKeepRuns
	for _, key := range []string{"timeout", "on_overlap", "keep_runs"} {
		if strings.Contains(defaults.TOML(), key) {
			t.Errorf("default %s was written:\n%s", key, defaults.TOML())
		}
	}
}

// TestHarnessFormRunKeysValidate: the form mirrors the parser, so it cannot
// write a run key the daemon would refuse to load.
func TestHarnessFormRunKeysValidate(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*HarnessForm)
		want   string
	}{
		{"timeout without schedule", func(f *HarnessForm) { f.Schedule = ""; f.Timeout = "1h" }, "timeout requires schedule"},
		{"bad timeout", func(f *HarnessForm) { f.Timeout = "soon" }, "invalid timeout"},
		{"negative timeout", func(f *HarnessForm) { f.Timeout = "-1m" }, "invalid timeout"},
		{"on_overlap without schedule", func(f *HarnessForm) { f.Schedule = ""; f.OnOverlap = "queue" }, "on_overlap requires schedule"},
		{"bad on_overlap", func(f *HarnessForm) { f.OnOverlap = "stack" }, "on_overlap must be"},
		{"keep_runs without schedule", func(f *HarnessForm) { f.Schedule = ""; f.KeepRuns = 3 }, "keep_runs requires schedule"},
		{"negative keep_runs", func(f *HarnessForm) { f.KeepRuns = -1 }, "keep_runs must be at least 1"},
	}
	for _, tc := range cases {
		f := scheduledForm()
		tc.mutate(&f)
		if err := f.Validate(); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: Validate() = %v, want %q", tc.name, err, tc.want)
		}
	}

	// A default value on an unscheduled harness is not a run key: the
	// on_overlap widget always holds something.
	plain := NewHarnessForm()
	plain.Name, plain.Harness, plain.Prompt = "once", "crush", "do it"
	plain.OnOverlap = "skip"
	if err := plain.Validate(); err != nil {
		t.Errorf("default on_overlap on an unscheduled harness rejected: %v", err)
	}
}

// TestFormatRunTimeout: pre-filled timeouts read the way an operator types them.
func TestFormatRunTimeout(t *testing.T) {
	for d, want := range map[time.Duration]string{
		30 * time.Minute: "30m",
		90 * time.Minute: "1h30m",
		2 * time.Hour:    "2h",
		45 * time.Second: "45s",
		0:                "0s",
	} {
		if got := formatRunTimeout(d); got != want {
			t.Errorf("formatRunTimeout(%v) = %q, want %q", d, got, want)
		}
	}
}

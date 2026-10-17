package config

// Governing: ADR-0045 (proposed); stump.wtf/harness#960.

import (
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

func workerTOML(extra string) string {
	return "[harness.worker]\nharness = \"generic\"\nargs = [\"-c\", \"sleep 1000\"]\n" + extra + "\n"
}

func TestHealthKeysParse(t *testing.T) {
	cfg, err := Parse([]byte(workerTOML("health_cmd = \"/usr/local/bin/probe.sh\"")), "test.toml")
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.Harnesses["worker"]
	if h.HealthCmd != "/usr/local/bin/probe.sh" {
		t.Errorf("health_cmd = %q", h.HealthCmd)
	}
	if h.HealthInterval != core.DefaultHealthInterval || h.HealthFailures != core.DefaultHealthFailures || h.HealthAction != core.HealthRestart {
		t.Errorf("defaults = interval %v, failures %d, action %q", h.HealthInterval, h.HealthFailures, h.HealthAction)
	}

	cfg, err = Parse([]byte(workerTOML("health_cmd = \"probe\"\nhealth_interval = \"90s\"\nhealth_failures = 5\nhealth_action = \"notify\"")), "test.toml")
	if err != nil {
		t.Fatal(err)
	}
	h = cfg.Harnesses["worker"]
	if h.HealthInterval != 90*time.Second || h.HealthFailures != 5 || h.HealthAction != core.HealthNotify {
		t.Errorf("got interval %v, failures %d, action %q", h.HealthInterval, h.HealthFailures, h.HealthAction)
	}
}

func TestHealthKeysWithoutProbeRejected(t *testing.T) {
	for _, extra := range []string{
		`health_interval = "5m"`,
		`health_failures = 2`,
		`health_action = "notify"`,
	} {
		_, err := Parse([]byte(workerTOML(extra)), "test.toml")
		if err == nil || !strings.Contains(err.Error(), "requires health_cmd") {
			t.Errorf("%s loaded; want a requires-health_cmd error (got %v)", extra, err)
		}
	}
}

func TestHealthKeyValuesRejected(t *testing.T) {
	cases := []struct {
		extra, want string
	}{
		{`health_cmd = "  "`, "must not be blank"},
		{`health_cmd = "probe"` + "\n" + `health_interval = "soon"`, "invalid health_interval"},
		{`health_cmd = "probe"` + "\n" + `health_interval = "0s"`, "must be positive"},
		{`health_cmd = "probe"` + "\n" + `health_failures = 0`, "at least 1"},
		{`health_cmd = "probe"` + "\n" + `health_action = "restart-kill"`, "invalid health_action"},
	}
	for _, tc := range cases {
		_, err := Parse([]byte(workerTOML(tc.extra)), "test.toml")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s loaded; want error %q (got %v)", tc.extra, tc.want, err)
		}
	}
}

// TestNoProbeLeavesHealthFieldsZero: a harness with no probe keeps every
// health field at its zero value — the defaults are for a probe that runs.
func TestNoProbeLeavesHealthFieldsZero(t *testing.T) {
	cfg, err := Parse([]byte(workerTOML("")), "test.toml")
	if err != nil {
		t.Fatal(err)
	}
	if h := cfg.Harnesses["worker"]; h.HealthCmd != "" || h.HealthInterval != 0 || h.HealthFailures != 0 || h.HealthAction != "" {
		t.Errorf("no-probe harness got health settings: %+v", h)
	}
}

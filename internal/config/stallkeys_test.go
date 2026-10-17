package config

// Governing: ADR-0045; issue #959.

import (
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// workerTOML is a persistent agent harness — the case the watchdog exists for
// — with the extra keys appended.
func workerTOML(extra string) string {
	return "[harness.worker]\nharness = \"crush\"\nprompt = \"work\"\n" + extra + "\n"
}

func TestStallKeysDefaultOff(t *testing.T) {
	cfg, err := Parse([]byte(workerTOML("")), "test.toml")
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.Harnesses["worker"]
	// A watchdog that cannot trip reports no action either: `stall_action`
	// only answers "what do you do about a finding".
	if h.StallTimeout != 0 || h.StallAction != "" {
		t.Errorf("no stall keys: timeout %v, action %q; want 0 and nothing", h.StallTimeout, h.StallAction)
	}
}

func TestStallKeysParse(t *testing.T) {
	cases := []struct {
		lines string
		check func(core.Harness) bool
	}{
		{`stall_timeout = "45m"`, func(h core.Harness) bool {
			return h.StallTimeout == 45*time.Minute && h.StallAction == core.StallNotify
		}},
		{"stall_timeout = \"2h\"\nstall_action = \"restart\"", func(h core.Harness) bool {
			return h.StallTimeout == 2*time.Hour && h.StallAction == core.StallRestart
		}},
		{"stall_timeout = \"0\"", func(h core.Harness) bool {
			return h.StallTimeout == 0 && h.StallAction == ""
		}},
	}
	for _, tc := range cases {
		cfg, err := Parse([]byte(workerTOML(tc.lines)), "test.toml")
		if err != nil {
			t.Errorf("%s: %v", tc.lines, err)
			continue
		}
		if h := cfg.Harnesses["worker"]; !tc.check(h) {
			t.Errorf("%s: parsed as %+v", tc.lines, h)
		}
	}
}

func TestStallKeyErrors(t *testing.T) {
	cases := []struct {
		name, lines, want string
	}{
		{"bad duration", `stall_timeout = "45"`, `invalid "stall_timeout"`},
		{"negative duration", `stall_timeout = "-5m"`, `invalid "stall_timeout"`},
		{"bad action", "stall_timeout = \"30m\"\nstall_action = \" reboot \"", "invalid \"stall_action\""},
		{"action lists values", "stall_timeout = \"30m\"\nstall_action = \"reboot\"", "restart | notify | none"},
		{"action without timeout", `stall_action = "restart"`, `requires a non-zero "stall_timeout"`},
		{"action with timeout 0", "stall_timeout = \"0\"\nstall_action = \"restart\"", `requires a non-zero "stall_timeout"`},
		{"action with a disabled watchdog", "stall_timeout = \"0\"\nstall_action = \"none\"", `requires a non-zero "stall_timeout"`},
	}
	for _, tc := range cases {
		_, err := Parse([]byte(workerTOML(tc.lines)), "test.toml")
		if err == nil {
			t.Errorf("%s: parsed clean, want an error saying %q", tc.name, tc.want)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error = %v, want it to say %q", tc.name, err, tc.want)
		}
	}
}

// TestStallKeysRefusedOnTriggeredHarness: a harness whose runs come from a
// clock or an event already bounds each run with `timeout`, so the stall
// watchdog would only restart the same firing twice.
func TestStallKeysRefusedOnTriggeredHarness(t *testing.T) {
	sources := map[string]string{
		"schedule": "[harness.sweep]\nharness = \"crush\"\nprompt = \"sweep\"\nschedule = \"0 3 * * *\"\n",
		"trigger":  "[channel.sb]\nurl = \"https://sb.example.invalid/mcp/x\"\n[harness.sweep]\nharness = \"crush\"\nprompt = \"sweep\"\ntriggers = [\"channel.sb\"]\n",
	}
	for _, key := range []string{"stall_timeout = \"45m\"", `stall_action = "restart"`} {
		for kind, base := range sources {
			_, err := Parse([]byte(base+key+"\n"), "test.toml")
			if err == nil {
				t.Errorf("%s harness with %s: parsed clean, want it refused", kind, key)
				continue
			}
			if !strings.Contains(err.Error(), "is refused on a scheduled or triggered harness") {
				t.Errorf("%s harness with %s: error = %v", kind, key, err)
			}
		}
	}
}

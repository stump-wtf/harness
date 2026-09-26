package config

// Notify Table Tests
//
// Governing tests: SPEC-0003 REQ "Operator Notification"; issue #725 — the
// table parses, omitted keys take their defaults, every refusal names its key
// and line, and neither a project file nor a harness_d drop-in may carry it.

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

func TestNotifyAbsentIsOff(t *testing.T) {
	cfg, err := Parse([]byte("[harness.a]\nharness = \"crush\"\n"), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Notify.Enabled() || cfg.Notify.Wants(core.NotifyFailed) {
		t.Fatalf("notify on with no [notify] table: %+v", cfg.Notify)
	}
}

func TestNotifyDefaults(t *testing.T) {
	cfg, err := Parse([]byte("[notify]\ncommand = [\"/usr/local/bin/notify\"]\n"), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	nc := cfg.Notify
	if !nc.Enabled() || !slices.Equal(nc.Command, []string{"/usr/local/bin/notify"}) {
		t.Fatalf("command = %+v", nc.Command)
	}
	if !slices.Equal(nc.Events, core.DefaultNotifyEvents) || nc.Timeout != 15*time.Second || nc.Cooldown != 15*time.Minute {
		t.Fatalf("defaults = %+v", nc)
	}
	if nc.Wants(core.NotifyRunFailed) {
		t.Error("run_failed is opt-in, but the default set includes it")
	}
	if !nc.Wants(core.NotifyTest) {
		t.Error("the test event must always run the hook")
	}
}

func TestNotifyParses(t *testing.T) {
	src := `
[notify]
command  = ["/home/joe/.config/harness/notify.sh", "--channel", "ops"]
events   = ["failed", "run_failed"]
timeout  = "30s"
cooldown = "0s"
`
	cfg, err := Parse([]byte(src), "t.toml")
	if err != nil {
		t.Fatal(err)
	}
	nc := cfg.Notify
	if !slices.Equal(nc.Command, []string{"/home/joe/.config/harness/notify.sh", "--channel", "ops"}) ||
		!slices.Equal(nc.Events, []string{"failed", "run_failed"}) ||
		nc.Timeout != 30*time.Second || nc.Cooldown != 0 {
		t.Fatalf("parsed = %+v", nc)
	}
	if nc.Wants(core.NotifyFlapping) {
		t.Error("flapping runs the hook though events leaves it out")
	}
}

func TestNotifyRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"no command", `events = ["failed"]`, `"command": is required`},
		{"empty command", `command = []`, `"command": is required`},
		{"relative path", `command = ["notify.sh"]`, "absolute path"},
		{"blank argv0", `command = [" "]`, "must not be blank"},
		{"empty events", "command = [\"/bin/true\"]\nevents = []", "at least one"},
		{"unknown event", "command = [\"/bin/true\"]\nevents = [\"exploded\"]", `unknown event "exploded"`},
		{"test is not listable", "command = [\"/bin/true\"]\nevents = [\"test\"]", `unknown event "test"`},
		{"duplicate event", "command = [\"/bin/true\"]\nevents = [\"failed\", \"failed\"]", "twice"},
		{"bad timeout", "command = [\"/bin/true\"]\ntimeout = \"soon\"", `"timeout"`},
		{"timeout too short", "command = [\"/bin/true\"]\ntimeout = \"10ms\"", `"timeout"`},
		{"timeout too long", "command = [\"/bin/true\"]\ntimeout = \"1h\"", `"timeout"`},
		{"negative cooldown", "command = [\"/bin/true\"]\ncooldown = \"-1m\"", `"cooldown"`},
		{"unknown key", "command = [\"/bin/true\"]\nurl = \"https://example\"", `unknown key "url"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte("[notify]\n"+tc.body+"\n"), "t.toml")
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

// The error must point at the key, not just the table, so a long file is
// fixable from the message alone.
func TestNotifyErrorNamesTheLine(t *testing.T) {
	src := "[harness.a]\nharness = \"crush\"\n\n[notify]\ncommand = [\"/bin/true\"]\ntimeout = \"soon\"\n"
	_, err := Parse([]byte(src), "t.toml")
	if err == nil || !strings.Contains(err.Error(), ":6") {
		t.Fatalf("err = %v, want line 6", err)
	}
}

func TestNotifyDuplicateTable(t *testing.T) {
	src := "[notify]\ncommand = [\"/bin/true\"]\n[notify]\ncommand = [\"/bin/false\"]\n"
	if _, err := Parse([]byte(src), "t.toml"); err == nil {
		t.Fatal("a duplicate [notify] table loaded")
	}
}

func TestNotifyNotInProjectFile(t *testing.T) {
	_, err := ParseProject([]byte("[notify]\ncommand = [\"/bin/true\"]\n"), "harness.toml")
	if err == nil || !strings.Contains(err.Error(), "notify") {
		t.Fatalf("project [notify] err = %v, want a refusal", err)
	}
}

func TestNotifyNotInDropIn(t *testing.T) {
	dir := t.TempDir()
	main := filepath.Join(dir, "harness.toml")
	dropins := filepath.Join(dir, "harness.d")
	if err := os.MkdirAll(dropins, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(main, []byte("[server]\nharness_d = \"harness.d\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dropins, "a.toml"), []byte("[notify]\ncommand = [\"/bin/true\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(main); err == nil || !strings.Contains(err.Error(), "[notify]") {
		t.Fatalf("drop-in [notify] err = %v, want a refusal", err)
	}
}

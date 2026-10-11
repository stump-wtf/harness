// Governing: ADR-0044 (amendment of 2026-10-09); SPEC-0026 REQ-3 (the
// [defaults] table: keys, validation, the never-suggested keys), REQ-6 (an
// override is parsed into the manifest shape), REQ-8 (the upgrade report),
// Error Handling Standards.
package agentpkg

import (
	"reflect"
	"strings"
	"testing"
)

const defaultsHead = "[package]\nname = \"issue-triager\"\n[harness]\nharness = \"claude-code\"\n"

// Every key REQ-3 admits parses through the real ParseManifest into the
// manifest's own shape, in DefaultsKeyOrder order regardless of how the
// author wrote them.
func TestParseManifestDefaultsValid(t *testing.T) {
	src := defaultsHead + `
[defaults]
restart_delay   = 30
schedule        = "CRON_TZ=America/New_York 0 9 * * 1-5"
triggers        = ["channel.matrix", "webhook.deploy"]
timeout         = "45m"
on_overlap      = "queue"
catch_up        = true
operating_hours = "mon-fri 09:00-17:00"
restart         = "on-failure"
workdir         = "/srv/triage"
`
	m, err := ParseManifest([]byte(src), "package.toml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := map[string]any{
		"schedule":        "CRON_TZ=America/New_York 0 9 * * 1-5",
		"triggers":        []string{"channel.matrix", "webhook.deploy"},
		"timeout":         "45m",
		"on_overlap":      "queue",
		"catch_up":        true,
		"operating_hours": "mon-fri 09:00-17:00",
		"restart":         "on-failure",
		"restart_delay":   int64(30),
		"workdir":         "/srv/triage",
	}
	if !reflect.DeepEqual(m.Defaults.Values, want) {
		t.Errorf("Defaults = %#v, want %#v", m.Defaults.Values, want)
	}
	if got := m.Defaults.Ordered(); !reflect.DeepEqual(got, DefaultsKeyOrder) {
		t.Errorf("Ordered() = %v, want the REQ-3 order %v", got, DefaultsKeyOrder)
	}
	triggers, ok := m.Defaults.Get("triggers")
	if !ok {
		t.Fatal("no triggers suggestion")
	}
	if got := TriggerRefs(triggers); !reflect.DeepEqual(got, []string{"channel.matrix", "webhook.deploy"}) {
		t.Errorf("TriggerRefs(triggers) = %v", got)
	}
}

// A manifest with no [defaults] suggests nothing, and one with an empty
// table suggests nothing either — neither is an error.
func TestParseManifestDefaultsAbsent(t *testing.T) {
	for _, src := range []string{defaultsHead, defaultsHead + "\n[defaults]\n"} {
		m, err := ParseManifest([]byte(src), "package.toml")
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(m.Defaults.Ordered()) != 0 {
			t.Errorf("Ordered() = %v, want none for %q", m.Defaults.Ordered(), src)
		}
	}
}

func TestParseManifestDefaultsViolations(t *testing.T) {
	tests := []struct {
		name, want string
		body       string
	}{
		{"unknown key names the key and the admitted set",
			"[defaults].nope is not a suggestible key", "[defaults]\nnope = \"x\"\n"},
		{"env_file is refused as a credential reference",
			"env_file can never be suggested", "[defaults]\nenv_file = \"/etc/secrets.env\"\n"},
		{"secrets_env is refused as a credential reference",
			"secrets_env can never be suggested", "[defaults]\nsecrets_env = \"VAULT\"\n"},
		{"enabled is refused as autostart intent",
			"enabled can never be suggested", "[defaults]\nenabled = true\n"},
		{"blank schedule", "must not be blank", "[defaults]\nschedule = \"  \"\n"},
		{"unparsable schedule", `invalid schedule "9 5 * *"`, "[defaults]\nschedule = \"9 5 * *\""},
		{"empty triggers list", `"triggers" must not be empty`, "[defaults]\ntriggers = []\n"},
		{"trigger of the wrong shape", `want "channel.<name>" or "webhook.<name>"`, "[defaults]\ntriggers = [\"matrix.room\"]\n"},
		{"duplicate trigger", `names "channel.a" twice`, "[defaults]\ntriggers = [\"channel.a\", \"channel.a\"]\n"},
		{"blank trigger entry", "entries must not be blank", "[defaults]\ntriggers = [\"channel.a\", \" \"]\n"},
		{"unparsable timeout", `invalid timeout "45"`, "[defaults]\ntimeout = \"45\"\n"},
		{"negative timeout", "must not be negative", "[defaults]\ntimeout = \"-5m\"\n"},
		{"unknown on_overlap", `invalid on_overlap "drop"`, "[defaults]\non_overlap = \"drop\"\n"},
		{"catch_up must be a bool", "must be a boolean", "[defaults]\ncatch_up = \"sometimes\"\n"},
		{"blank operating_hours", "must not be blank", "[defaults]\noperating_hours = \"\"\n"},
		{"unparsable operating_hours", "invalid operating_hours", "[defaults]\noperating_hours = \"weekdays-ish\"\n"},
		{"unknown restart policy", `invalid restart "maybe"`, "[defaults]\nrestart = \"maybe\"\n"},
		{"negative restart_delay", "must not be negative", "[defaults]\nrestart_delay = -1\n"},
		{"restart_delay must be a whole number", "must be an integer", "[defaults]\nrestart_delay = \"5m\"\n"},
		{"blank workdir", "must not be blank", "[defaults]\nworkdir = \"\"\n"},
		{"the ${ grammar applies here too", "secret references are not permitted", "[defaults]\nworkdir = \"${HOME}\"\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(defaultsHead+"\n"+tc.body), "package.toml")
			if err == nil {
				t.Fatalf("want a violation naming %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to name %q", err, tc.want)
			}
			if !strings.Contains(err.Error(), "[defaults]") {
				t.Errorf("err = %v, want it to name the key under [defaults]", err)
			}
		})
	}
}

// A table where a table belongs fails naming what was expected, and a key
// written after [harness] belongs to [harness] — where defaults is not an
// allowed key, so it fails as a stray key rather than as a bad table.
func TestParseManifestDefaultsMalformed(t *testing.T) {
	if _, err := ParseManifest([]byte("defaults = \"nope\"\n"+defaultsHead), "package.toml"); err == nil {
		t.Error("a top-level defaults that is not a table must fail")
	} else if !strings.Contains(err.Error(), "[defaults] must be a table") {
		t.Errorf("err = %v", err)
	}
	if _, err := ParseManifest([]byte(defaultsHead+"\ndefaults = 3\n"), "package.toml"); err == nil {
		t.Error("a defaults key under [harness] must fail")
	} else if !strings.Contains(err.Error(), "[harness].defaults is not an allowed key") {
		t.Errorf("err = %v", err)
	}
}

// workdir is the operator's path, so the package-relative path rule that
// anchors every other manifest path on the pin directory deliberately does
// not apply: an absolute path is the whole point.
func TestParseManifestDefaultsWorkdirIsNotAPackagePath(t *testing.T) {
	m, err := ParseManifest([]byte(defaultsHead+"\n[defaults]\nworkdir = \"/srv/triage\"\n"), "package.toml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v, _ := m.Defaults.Get("workdir"); v != "/srv/triage" {
		t.Errorf("workdir = %v", v)
	}
}

// An interactive override arrives as typed text; NormalizeDefault puts it in
// the same shape the manifest would have carried, so the written key reads
// identically whichever hand wrote it, and refuses a value the loader would
// refuse.
func TestNormalizeDefault(t *testing.T) {
	tests := []struct {
		name, key, raw string
		want           any
		bad            bool
	}{
		{name: "schedule", key: "schedule", raw: " 0 9 * * 1-5 ", want: "0 9 * * 1-5"},
		{name: "timeout", key: "timeout", raw: "2h", want: "2h"},
		{name: "catch_up true", key: "catch_up", raw: "true", want: true},
		{name: "catch_up yes", key: "catch_up", raw: "yes", want: true},
		{name: "catch_up false", key: "catch_up", raw: "false", want: false},
		{name: "catch_up junk", key: "catch_up", raw: "maybe", bad: true},
		{name: "restart_delay", key: "restart_delay", raw: "45", want: int64(45)},
		{name: "restart_delay negative", key: "restart_delay", raw: "-5", bad: true},
		{name: "restart_delay duration", key: "restart_delay", raw: "5m", bad: true},
		{name: "triggers list", key: "triggers", raw: "channel.matrix, webhook.deploy", want: []string{"channel.matrix", "webhook.deploy"}},
		{name: "triggers junk", key: "triggers", raw: "matrix", bad: true},
		{name: "on_overlap junk", key: "on_overlap", raw: "later", bad: true},
		{name: "workdir", key: "workdir", raw: " /srv/x ", want: "/srv/x"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NormalizeDefault(tc.key, tc.raw)
			if tc.bad {
				if err == nil {
					t.Fatalf("NormalizeDefault(%q, %q) = %v, want an error", tc.key, tc.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("NormalizeDefault(%q, %q): %v", tc.key, tc.raw, err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("NormalizeDefault(%q, %q) = %#v, want %#v", tc.key, tc.raw, got, tc.want)
			}
		})
	}
}

// The report lines say suggests, never runs with — [defaults] is inert at
// config load, and info and the confirmation report must not imply it took
// effect.
func TestRenderDefaults(t *testing.T) {
	m, err := ParseManifest([]byte(defaultsHead+"\n[defaults]\ntimeout = \"45m\"\n"), "package.toml")
	if err != nil {
		t.Fatal(err)
	}
	lines := RenderDefaults(m)
	if len(lines) != 1 {
		t.Fatalf("lines = %v", lines)
	}
	if !strings.Contains(lines[0], "suggests timeout = 45m") || !strings.Contains(lines[0], "suggestion") {
		t.Errorf("lines = %q, want a marked suggestion", lines[0])
	}
	if got := RenderDefaults(nil); len(got) != 1 || !strings.Contains(got[0], "no supervision defaults") {
		t.Errorf("RenderDefaults(nil) = %q", got)
	}
}

// The upgrade report names every added, changed and dropped suggestion with
// its old and new value, and says plainly that a dropped one leaves the
// operator's table alone.
func TestDiffLines(t *testing.T) {
	oldMan, err := ParseManifest([]byte(defaultsHead+"\n[defaults]\ntimeout = \"45m\"\ncatch_up = true\n"), "package.toml")
	if err != nil {
		t.Fatal(err)
	}
	newMan, err := ParseManifest([]byte(defaultsHead+"\n[defaults]\ntimeout = \"2h\"\non_overlap = \"queue\"\n"), "package.toml")
	if err != nil {
		t.Fatal(err)
	}
	got := DiffLines(oldMan.Defaults, newMan.Defaults)
	want := []string{
		"changed suggestion timeout: 45m -> 2h",
		"new suggestion on_overlap: queue",
		"dropped suggestion catch_up: true (your table keeps what it holds)",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DiffLines = %q, want %q", got, want)
	}
	d := DiffDefaults(oldMan.Defaults, newMan.Defaults)
	if !reflect.DeepEqual(d.Added, []string{"on_overlap"}) || !reflect.DeepEqual(d.Changed, []string{"timeout"}) || !reflect.DeepEqual(d.Removed, []string{"catch_up"}) {
		t.Errorf("DiffDefaults = %+v", d)
	}
	if DiffLines(nil, nil) != nil {
		t.Errorf("two absent tables must diff to nothing")
	}
}

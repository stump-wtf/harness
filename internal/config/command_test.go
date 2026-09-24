package config

// Command Harness Kind Tests
//
// `harness = "command"` declares `argv`, exec'd without a shell. These tests
// pin the load-time rules: argv is required and its executable is a literal,
// argv replaces args on this kind and is refused on every other, and the keys
// that fold flags into a synthesized agent argv are refused because the
// operator owns this argv. Every refusal must be a located *config.Error.
//
// Governing: ADR-0023, SPEC-0017 REQ-2 "Command Harness Kind", REQ-3
// "Command Harness Modes And Exclusions".

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/core"
)

// assertLocated checks err is a *config.Error at file:line whose message
// contains every want.
func assertLocated(t *testing.T, err error, file string, line int, want ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("config loaded; want a validation error")
	}
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("error is %T, want *config.Error: %v", err, err)
	}
	if ce.File != file || ce.Line != line {
		t.Errorf("error located at %s:%d, want %s:%d (err: %v)", ce.File, ce.Line, file, line, err)
	}
	for _, w := range want {
		if !strings.Contains(ce.Msg, w) {
			t.Errorf("message %q does not contain %q", ce.Msg, w)
		}
	}
}

// TestCommandHarnessLoadsResident: a resident command harness loads with its
// argv verbatim — element boundaries, shell metacharacters and all — with
// generic's resident defaults, and takes the resident keys REQ-3 names.
func TestCommandHarnessLoadsResident(t *testing.T) {
	cfg, err := Parse([]byte(`[harness.report]
harness = "command"
argv = ["/bin/echo", "a b", "$(id)", ";", "rm -rf /", ""]
workdir = "/srv/report"
restart_delay = 3
operating_hours = "Mon-Fri 09:00-17:00"
`), "t.toml")
	if err != nil {
		t.Fatalf("resident command harness did not load: %v", err)
	}
	h := cfg.Harnesses["report"]
	if h.Adapter != core.AdapterCommand {
		t.Errorf("Adapter = %q, want command", h.Adapter)
	}
	if want := []string{"/bin/echo", "a b", "$(id)", ";", "rm -rf /", ""}; !slices.Equal(h.Argv, want) {
		t.Errorf("Argv = %q, want %q verbatim", h.Argv, want)
	}
	if h.Args != nil || h.IsAgent() || h.Triggered() {
		t.Errorf("command harness grew args/prompt/firing: %+v", h)
	}
	// generic's resident defaults: always restart, not enabled unless asked.
	if h.Restart != core.RestartAlways || h.Enabled || h.Quiet {
		t.Errorf("Restart/Enabled/Quiet = %q/%v/%v, want always/false/false", h.Restart, h.Enabled, h.Quiet)
	}

	cfg, err = Parse([]byte("[harness.r]\nharness = \"command\"\nargv = [\"report\"]\nenabled = true\nrestart = \"on-failure\"\n"), "t.toml")
	if err != nil {
		t.Fatalf("enabled/restart on a command harness: %v", err)
	}
	if h := cfg.Harnesses["r"]; !h.Enabled || h.Restart != core.RestartOnFailure {
		t.Errorf("enabled/restart not honoured: %+v", h)
	}
}

// TestCommandHarnessRejections covers REQ-2's "A placeholder cannot choose the
// executable", "args on a command harness", "argv on another kind" and
// "Missing argv", and REQ-3's "Adapter flags are rejected" and "model without
// a placeholder", plus the keys this resident slice does not accept yet.
func TestCommandHarnessRejections(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		want       []string
	}{
		{"placeholder argv0", `harness = "command"
argv = ["{{event.repo}}", "x"]`, []string{`"argv[0]"`, "placeholder cannot choose"}},
		{"placeholder argv0 with whitespace", `harness = "command"
argv = ["{{ model }}"]`, []string{`"argv[0]"`}},
		{"blank argv0", `harness = "command"
argv = ["  ", "x"]`, []string{`"argv[0]" must not be blank`}},
		{"template in argv1 before templates exist", `harness = "command"
argv = ["/usr/local/bin/report", "--run", "{{run.id}}"]`, []string{`"argv[2]"`, "not supported yet"}},
		{"missing argv", `harness = "command"`, []string{`requires "argv"`}},
		{"empty argv", `harness = "command"
argv = []`, []string{`requires "argv"`}},
		{"args and argv", `harness = "command"
argv = ["/bin/echo"]
args = ["hi"]`, []string{`"args" is not accepted`, `"argv"`}},
		{"args without argv", `harness = "command"
args = ["-c", "true"]`, []string{`"args" is not accepted`, `"argv"`}},
		{"argv on claude-code", `harness = "claude-code"
argv = ["claude"]`, []string{`"argv" is only accepted on harness = "command"`, `"claude-code"`}},
		{"empty argv on generic", `harness = "generic"
argv = []`, []string{`"argv" is only accepted`}},
		{"auto_accept true", `harness = "command"
argv = ["x"]
auto_accept = true`, []string{`"auto_accept"`, "owns its argv"}},
		{"auto_accept false", `harness = "command"
argv = ["x"]
auto_accept = false`, []string{`"auto_accept"`, "owns its argv"}},
		{"max_turns", `harness = "command"
argv = ["x"]
max_turns = 3`, []string{`"max_turns"`, "owns its argv"}},
		{"quiet", `harness = "command"
argv = ["x"]
quiet = false`, []string{`"quiet"`, "owns its argv"}},
		{"model without placeholder", `harness = "command"
argv = ["x"]
model = "x/y"`, []string{`"model" is unused`, "{{model}}"}},
		{"prompt", `harness = "command"
argv = ["x"]
prompt = "triage"`, []string{`"prompt" is not supported on a command harness yet`}},
		{"schedule", `harness = "command"
argv = ["x"]
schedule = "0 6 * * *"`, []string{`"schedule" is not supported on a command harness yet`}},
		{"triggers", `harness = "command"
argv = ["x"]
triggers = ["webhook.ci"]`, []string{`"triggers" is not supported on a command harness yet`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A valid harness first, so the refusal's line proves it is
			// located at the offending table, not at the top of the file.
			body := "[harness.ok]\nharness = \"command\"\nargv = [\"true\"]\n\n[harness.bad]\n" + tc.body + "\n"
			_, err := Parse([]byte(body), "t.toml")
			assertLocated(t, err, "t.toml", 5, append([]string{`harness "bad"`}, tc.want...)...)
		})
	}
}

// TestCommandPromptFileRefusedBeforeItIsRead: a command harness naming a
// prompt_file is refused for its kind, not reported as a missing file.
func TestCommandPromptFileRefusedBeforeItIsRead(t *testing.T) {
	_, err := Parse([]byte("[harness.x]\nharness = \"command\"\nargv = [\"x\"]\nprompt_file = \"missing.md\"\n"), "t.toml")
	assertLocated(t, err, "t.toml", 1, `"prompt_file" is not supported on a command harness yet`)
}

// TestGenericPromptErrorNamesCommand is REQ-1's "once REQ-2 has shipped": the
// generic refusal names the command kind as the way to run another program.
func TestGenericPromptErrorNamesCommand(t *testing.T) {
	_, err := Parse([]byte("[harness.x]\nharness = \"generic\"\nprompt = \"hi\"\n"), "t.toml")
	assertLocated(t, err, "t.toml", 1, `harness = "command"`)
}

// TestCommandHarnessInProjectFile: a project file declares a command harness
// through the same registerHarness, and its argv is carried verbatim.
func TestCommandHarnessInProjectFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "harness.toml")
	if err := os.WriteFile(path, []byte("[harness.report]\nharness = \"command\"\nargv = [\"./bin/report\", \"a b\"]\nworkdir = \"sub\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	proj, err := LoadProject(path)
	if err != nil {
		t.Fatalf("project command harness did not load: %v", err)
	}
	h := proj.Config.Harnesses["report"]
	if !slices.Equal(h.Argv, []string{"./bin/report", "a b"}) {
		t.Errorf("Argv = %q, want it verbatim (argv[0] resolves against workdir at spawn, not here)", h.Argv)
	}
	if !h.Enabled {
		t.Error("project harness not enabled by default")
	}

	if err := os.WriteFile(path, []byte("[harness.report]\nharness = \"command\"\nargv = [\"{{event.repo}}\"]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = LoadProject(path)
	assertLocated(t, err, path, 1, `"argv[0]"`)
}

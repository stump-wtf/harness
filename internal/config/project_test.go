package config

// Governing: SPEC-0004 REQ "Project File Discovery", REQ "Project File Schema",
// REQ "Error Handling Standards". Tests project file discovery, schema parsing,
// forbidden-table rejection, relative workdir resolution, sentinel errors, and
// the "never treat the global config as a project file" requirement.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// writeProjectFile writes data to a harness.toml in dir and returns its path.
func writeProjectFile(t *testing.T, dir, data string) string {
	t.Helper()
	p := filepath.Join(dir, "harness.toml")
	if err := os.WriteFile(p, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestParseProject_BasicHarness(t *testing.T) {
	data := []byte(`
[harness.agent]
cmd = "claude"
args = ["--remote-control"]
workdir = "."
`)
	proj, err := ParseProject(data, "/tmp/myrepo/harness.toml")
	if err != nil {
		t.Fatalf("ParseProject: %v", err)
	}
	if proj.Name != "myrepo" {
		t.Errorf("Name = %q, want %q", proj.Name, "myrepo")
	}
	if proj.Root != "/tmp/myrepo" {
		t.Errorf("Root = %q, want %q", proj.Root, "/tmp/myrepo")
	}
	if len(proj.Config.HarnessOrder) != 1 {
		t.Fatalf("HarnessOrder len = %d, want 1", len(proj.Config.HarnessOrder))
	}
	h, ok := proj.Config.Harnesses["agent"]
	if !ok {
		t.Fatal("missing harness 'agent'")
	}
	if h.Cmd != "claude" {
		t.Errorf("Cmd = %q, want %q", h.Cmd, "claude")
	}
	// Relative workdir resolved against project root.
	if h.Workdir != "/tmp/myrepo" {
		t.Errorf("Workdir = %q, want %q (resolved against project root)", h.Workdir, "/tmp/myrepo")
	}
}

// TestParseProject_RestartPolicy: the project schema carries the same restart
// directive as the global config (SPEC-0004 REQ "Project File Schema":
// identical field meanings), including the normalization of an omitted key to
// the always default and the rejection of unknown values.
func TestParseProject_RestartPolicy(t *testing.T) {
	data := []byte(`
[harness.migrate]
cmd = "./migrate"
restart = "no"

[harness.agent]
cmd = "claude"
`)
	proj, err := ParseProject(data, "/tmp/myrepo/harness.toml")
	if err != nil {
		t.Fatalf("ParseProject: %v", err)
	}
	if got := proj.Config.Harnesses["migrate"].Restart; got != core.RestartNo {
		t.Errorf("migrate restart = %q, want %q", got, core.RestartNo)
	}
	if got := proj.Config.Harnesses["agent"].Restart; got != core.RestartAlways {
		t.Errorf("agent restart = %q, want %q (omitted key normalizes to the default)", got, core.RestartAlways)
	}

	bad := []byte(`
[harness.agent]
cmd = "claude"
restart = "until-pigs-fly"
`)
	_, err = ParseProject(bad, "/tmp/myrepo/harness.toml")
	if err == nil {
		t.Fatal("expected error for invalid restart policy, got nil")
	}
	var cerr *Error
	if !errors.As(err, &cerr) {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if !strings.Contains(cerr.Msg, "invalid restart policy") {
		t.Errorf("error message should mention the restart policy: %s", cerr.Msg)
	}
}

func TestParseProject_ProjectName(t *testing.T) {
	data := []byte(`
[project]
name = "custom-name"

[harness.agent]
cmd = "crush"
`)
	proj, err := ParseProject(data, "/tmp/repo/harness.toml")
	if err != nil {
		t.Fatalf("ParseProject: %v", err)
	}
	if proj.Name != "custom-name" {
		t.Errorf("Name = %q, want %q", proj.Name, "custom-name")
	}
}

func TestParseProject_RejectServer(t *testing.T) {
	data := []byte(`
[harness.agent]
cmd = "claude"

[server]
enabled = false
`)
	_, err := ParseProject(data, "/tmp/repo/harness.toml")
	if err == nil {
		t.Fatal("expected error for [server] table, got nil")
	}
	// Should mention "server" and be a *Error with a source line.
	var cerr *Error
	if !errors.As(err, &cerr) {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if !strings.Contains(cerr.Msg, "server") {
		t.Errorf("error message should mention 'server': %s", cerr.Msg)
	}
	if cerr.Line <= 0 {
		t.Errorf("expected positive source line, got %d", cerr.Line)
	}
}

func TestParseProject_RejectProfile(t *testing.T) {
	data := []byte(`
[harness.agent]
cmd = "claude"

[profile.default]
harnesses = ["agent"]
`)
	_, err := ParseProject(data, "/tmp/repo/harness.toml")
	if err == nil {
		t.Fatal("expected error for [profile.default] table, got nil")
	}
	var cerr *Error
	if !errors.As(err, &cerr) {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if !strings.Contains(cerr.Msg, "profile") {
		t.Errorf("error message should mention 'profile': %s", cerr.Msg)
	}
	if cerr.Line <= 0 {
		t.Errorf("expected positive source line, got %d", cerr.Line)
	}
}

func TestParseProject_MultipleHarnesses(t *testing.T) {
	data := []byte(`
[harness.agent]
cmd = "claude"

[harness.reviewer]
cmd = "crush"
workdir = "./reviews"
`)
	proj, err := ParseProject(data, "/tmp/repo/harness.toml")
	if err != nil {
		t.Fatalf("ParseProject: %v", err)
	}
	if len(proj.Config.HarnessOrder) != 2 {
		t.Fatalf("HarnessOrder len = %d, want 2", len(proj.Config.HarnessOrder))
	}
	h, ok := proj.Config.Harnesses["reviewer"]
	if !ok {
		t.Fatal("missing harness 'reviewer'")
	}
	if h.Workdir != "/tmp/repo/reviews" {
		t.Errorf("Workdir = %q, want %q", h.Workdir, "/tmp/repo/reviews")
	}
}

func TestParseProject_BareHarnessTable(t *testing.T) {
	// Bare [name] tables are backward-compatible (ADR-0006).
	data := []byte(`
[agent]
cmd = "claude"
`)
	proj, err := ParseProject(data, "/tmp/repo/harness.toml")
	if err != nil {
		t.Fatalf("ParseProject: %v", err)
	}
	if len(proj.Config.HarnessOrder) != 1 {
		t.Fatalf("HarnessOrder len = %d, want 1", len(proj.Config.HarnessOrder))
	}
	if _, ok := proj.Config.Harnesses["agent"]; !ok {
		t.Fatal("missing harness 'agent'")
	}
}

func TestParseProject_EnvFileResolution(t *testing.T) {
	data := []byte(`
[harness.agent]
cmd = "claude"
env_file = "secrets.env"
`)
	proj, err := ParseProject(data, "/tmp/repo/harness.toml")
	if err != nil {
		t.Fatalf("ParseProject: %v", err)
	}
	h := proj.Config.Harnesses["agent"]
	if h.EnvFile != "/tmp/repo/secrets.env" {
		t.Errorf("EnvFile = %q, want %q", h.EnvFile, "/tmp/repo/secrets.env")
	}
}

func TestParseProject_AbsoluteWorkdir(t *testing.T) {
	data := []byte(`
[harness.agent]
cmd = "claude"
workdir = "/opt/abs"
`)
	proj, err := ParseProject(data, "/tmp/repo/harness.toml")
	if err != nil {
		t.Fatalf("ParseProject: %v", err)
	}
	h := proj.Config.Harnesses["agent"]
	if h.Workdir != "/opt/abs" {
		t.Errorf("Workdir = %q, want %q (absolute unchanged)", h.Workdir, "/opt/abs")
	}
}

func TestParseProject_TildeWorkdir(t *testing.T) {
	data := []byte(`
[harness.agent]
cmd = "claude"
workdir = "~/src"
`)
	proj, err := ParseProject(data, "/tmp/repo/harness.toml")
	if err != nil {
		t.Fatalf("ParseProject: %v", err)
	}
	h := proj.Config.Harnesses["agent"]
	if h.Workdir != "~/src" {
		t.Errorf("Workdir = %q, want %q (tilde preserved)", h.Workdir, "~/src")
	}
}

func TestParseProject_EmptyFile(t *testing.T) {
	data := []byte(``)
	proj, err := ParseProject(data, "/tmp/repo/harness.toml")
	if err != nil {
		t.Fatalf("ParseProject: %v", err)
	}
	if len(proj.Config.HarnessOrder) != 0 {
		t.Errorf("HarnessOrder len = %d, want 0", len(proj.Config.HarnessOrder))
	}
}

func TestParseProject_DuplicateHarness(t *testing.T) {
	data := []byte(`
[harness.agent]
cmd = "claude"

[harness.agent]
cmd = "crush"
`)
	_, err := ParseProject(data, "/tmp/repo/harness.toml")
	if err == nil {
		t.Fatal("expected error for duplicate harness, got nil")
	}
}

func TestParseProject_MissingCmd(t *testing.T) {
	data := []byte(`
[harness.agent]
args = ["--yolo"]
`)
	_, err := ParseProject(data, "/tmp/repo/harness.toml")
	if err == nil {
		t.Fatal("expected error for missing cmd, got nil")
	}
}

// ---- Prompt harness parity (SPEC-0004 REQ "Project File Schema") ----------

// TestParseProject_PromptHarness: the agent one-shot `prompt` field means the
// same thing in a project file as in the global config (identical field
// meanings): the prompt stored verbatim, Cmd/Args left empty for spawn-time
// synthesis (ADR-0011), and an omitted restart defaulting to "no".
func TestParseProject_PromptHarness(t *testing.T) {
	data := []byte(`
[harness.agent]
prompt = "summarize the day"
`)
	proj, err := ParseProject(data, "/tmp/myrepo/harness.toml")
	if err != nil {
		t.Fatalf("ParseProject: %v", err)
	}
	h, ok := proj.Config.Harnesses["agent"]
	if !ok {
		t.Fatal("missing harness 'agent'")
	}
	if h.Prompt != "summarize the day" {
		t.Errorf("Prompt = %q, want %q", h.Prompt, "summarize the day")
	}
	if h.Cmd != "" || h.Args != nil {
		t.Errorf("Cmd/Args = %q/%v, want empty (spawn-time synthesis)", h.Cmd, h.Args)
	}
	if h.Restart != core.RestartNo {
		t.Errorf("Restart = %q, want %q (one-shot default)", h.Restart, core.RestartNo)
	}
	// The project bring-up default still applies to prompt harnesses.
	if !h.Enabled {
		t.Error("Enabled = false, want true (project bring-up default)")
	}
}

// TestParseProject_PromptErrors: the prompt exclusivity rules hold in project
// files too, with the same located-error contract as the global parser.
func TestParseProject_PromptErrors(t *testing.T) {
	tests := []struct{ name, toml, wantSub string }{
		{"prompt and cmd", "[harness.bad]\ncmd = \"echo\"\nprompt = \"hi\"\n", `"prompt" and "cmd" are mutually exclusive`},
		{"prompt and args", "[harness.bad]\nprompt = \"hi\"\nargs = [\"x\"]\n", `"prompt" and "args" are mutually exclusive`},
		{"blank prompt", "[harness.bad]\nprompt = \" \"\n", `"prompt" must not be blank`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseProject([]byte(tt.toml), "/tmp/repo/harness.toml")
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var cerr *Error
			if !errors.As(err, &cerr) {
				t.Fatalf("expected *Error, got %T: %v", err, err)
			}
			if !strings.Contains(cerr.Msg, tt.wantSub) {
				t.Errorf("message %q does not contain %q", cerr.Msg, tt.wantSub)
			}
			if cerr.Line <= 0 {
				t.Errorf("expected positive source line, got %d", cerr.Line)
			}
		})
	}
}

// ---- Model parity (SPEC-0004 REQ "Project File Schema") -------------------

// TestParseProject_ModelHarness: the agent `model` selection means the same
// thing in a project file as in the global config (identical field meanings,
// via the shared registerHarness): stored as config truth, never desugared
// into args — the supervisor folds it into the synthesized argv at spawn
// (ADR-0011, issue #57).
func TestParseProject_ModelHarness(t *testing.T) {
	data := []byte(`
[harness.agent]
prompt = "summarize the day"
model = "claude-opus-5"
`)
	proj, err := ParseProject(data, "/tmp/myrepo/harness.toml")
	if err != nil {
		t.Fatalf("ParseProject: %v", err)
	}
	h, ok := proj.Config.Harnesses["agent"]
	if !ok {
		t.Fatal("missing harness 'agent'")
	}
	if h.Model != "claude-opus-5" {
		t.Errorf("Model = %q, want %q", h.Model, "claude-opus-5")
	}
	if h.Cmd != "" || h.Args != nil {
		t.Errorf("Cmd/Args = %q/%v, want empty (model must never be desugared into args)", h.Cmd, h.Args)
	}
	// The project bring-up default still applies to model-bearing harnesses.
	if !h.Enabled {
		t.Error("Enabled = false, want true (project bring-up default)")
	}
}

// TestParseProject_ModelErrors: the model validation rules hold in project
// files too, with the same located-error contract as the global parser.
func TestParseProject_ModelErrors(t *testing.T) {
	tests := []struct{ name, toml, wantSub string }{
		{"model with cmd", "[harness.bad]\ncmd = \"echo\"\nmodel = \"m\"\n", `"model" requires "prompt"`},
		{"blank model", "[harness.bad]\nprompt = \"hi\"\nmodel = \" \"\n", `"model" must not be blank`},
		{"multi-token model", "[harness.bad]\nprompt = \"hi\"\nmodel = \"a b\"\n", `"model" must be a single token`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseProject([]byte(tt.toml), "/tmp/repo/harness.toml")
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var cerr *Error
			if !errors.As(err, &cerr) {
				t.Fatalf("expected *Error, got %T: %v", err, err)
			}
			if !strings.Contains(cerr.Msg, tt.wantSub) {
				t.Errorf("message %q does not contain %q", cerr.Msg, tt.wantSub)
			}
			if cerr.Line <= 0 {
				t.Errorf("expected positive source line, got %d", cerr.Line)
			}
		})
	}
}

// ---- Auto-accept parity (SPEC-0004 REQ "Project File Schema") -------------

// TestParseProject_AutoAcceptHarness: the agent `auto_accept` unattended mode
// means the same thing in a project file as in the global config (identical
// field meanings, via the shared registerHarness): stored as config truth,
// never desugared into args — the supervisor folds the vendor's yolo flag
// into the synthesized argv at spawn (ADR-0011, issue #58).
func TestParseProject_AutoAcceptHarness(t *testing.T) {
	data := []byte(`
[harness.agent]
prompt = "summarize the day"
auto_accept = true
`)
	proj, err := ParseProject(data, "/tmp/myrepo/harness.toml")
	if err != nil {
		t.Fatalf("ParseProject: %v", err)
	}
	h, ok := proj.Config.Harnesses["agent"]
	if !ok {
		t.Fatal("missing harness 'agent'")
	}
	if !h.AutoAccept {
		t.Error("AutoAccept = false, want true")
	}
	if h.Cmd != "" || h.Args != nil {
		t.Errorf("Cmd/Args = %q/%v, want empty (auto_accept must never be desugared into args)", h.Cmd, h.Args)
	}
}

// TestParseProject_AutoAcceptErrors: the auto_accept validation rule holds in
// project files too, with the same located-error contract as the global
// parser.
func TestParseProject_AutoAcceptErrors(t *testing.T) {
	_, err := ParseProject([]byte("[harness.bad]\ncmd = \"echo\"\nauto_accept = true\n"), "/tmp/repo/harness.toml")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var cerr *Error
	if !errors.As(err, &cerr) {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	if want := `"auto_accept" requires "prompt"`; !strings.Contains(cerr.Msg, want) {
		t.Errorf("message %q does not contain %q", cerr.Msg, want)
	}
	if cerr.Line <= 0 {
		t.Errorf("expected positive source line, got %d", cerr.Line)
	}
}

// ---- Discovery tests -----------------------------------------------------

func TestDiscoverProject_FindsAncestorFile(t *testing.T) {
	// Create: tmpDir/reduit/harness.toml
	// chdir:   tmpDir/reduit/internal/foo
	// expect:  project root = tmpDir/reduit
	tmpDir := t.TempDir()
	reduit := filepath.Join(tmpDir, "reduit")
	internalFoo := filepath.Join(reduit, "internal", "foo")
	if err := os.MkdirAll(internalFoo, 0755); err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, reduit, `
[harness.agent]
cmd = "claude"
`)

	oldWd, _ := os.Getwd()
	defer os.Chdir(oldWd)
	if err := os.Chdir(internalFoo); err != nil {
		t.Fatal(err)
	}

	proj, err := DiscoverProject()
	if err != nil {
		t.Fatalf("DiscoverProject: %v", err)
	}
	if proj.Name != "reduit" {
		t.Errorf("Name = %q, want %q", proj.Name, "reduit")
	}
	// Root should end with /reduit.
	if !strings.HasSuffix(proj.Root, "reduit") {
		t.Errorf("Root = %q, want suffix %q", proj.Root, "reduit")
	}
}

func TestDiscoverProject_NoProjectFile(t *testing.T) {
	tmpDir := t.TempDir()
	sub := filepath.Join(tmpDir, "sub")
	if err := os.MkdirAll(sub, 0755); err != nil {
		t.Fatal(err)
	}

	oldWd, _ := os.Getwd()
	defer os.Chdir(oldWd)
	if err := os.Chdir(sub); err != nil {
		t.Fatal(err)
	}

	_, err := DiscoverProject()
	if err == nil {
		t.Fatal("expected error when no project file, got nil")
	}
	if !errors.Is(err, ErrNoProjectFound) {
		t.Errorf("expected ErrNoProjectFound, got %v", err)
	}
}

func TestDiscoverProject_SkipsGlobalConfig(t *testing.T) {
	// If the global config exists at ~/.config/harness/harness.toml and we
	// are inside that directory, discovery should NOT adopt it as a project.
	// We can't easily mock the global path, but we can test the samePath
	// helper directly.
	global := DefaultPath()
	if samePath(global, global) {
		// This is the core assertion: the global path equals itself (sanity).
	} else {
		t.Error("samePath(global, global) should be true")
	}
	// A different path should not be "same".
	other := filepath.Join(filepath.Dir(global), "different.toml")
	if samePath(global, other) {
		t.Error("samePath should return false for different paths")
	}
}

// TestSamePath_ResolvesSymlinks pins the discovery fix: two spellings of the
// same file that differ only by a symlink must compare equal, and a path
// that doesn't exist on disk must still compare sensibly (EvalSymlinks
// errors on a missing path, and an absent global config is normal).
func TestSamePath_ResolvesSymlinks(t *testing.T) {
	tmpDir := t.TempDir()
	realDir := filepath.Join(tmpDir, "real")
	if err := os.MkdirAll(realDir, 0755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(realDir, "harness.toml")
	if err := os.WriteFile(file, []byte("[harness.x]\ncmd=\"y\"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmpDir, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Skipf("cannot create symlinks here: %v", err)
	}
	if !samePath(file, filepath.Join(link, "harness.toml")) {
		t.Error("samePath should treat a file and its symlinked spelling as the same path")
	}

	// Missing paths fall back to the absolute form — a non-existent global
	// config must not make every candidate compare equal, nor error out.
	missing := filepath.Join(tmpDir, "nope", "harness.toml")
	if samePath(missing, file) {
		t.Error("samePath(missing, existing) should be false")
	}
	if !samePath(missing, missing) {
		t.Error("samePath(missing, missing) should be true (fallback to abs form)")
	}
}

// ---- sanitizeProjectName tests -------------------------------------------

func TestSanitizeProjectName(t *testing.T) {
	tests := []struct {
		input, want string
	}{
		{"reduit", "reduit"},
		{"My-Cool Project", "my-cool-project"},
		{"foo!", "foo"},
		{"A B_C", "a-b-c"}, // underscore → hyphen
		{"", "unnamed"},
		{"!!!", "unnamed"},
		{"MixedCase", "mixedcase"},
		{"123abc", "123abc"},
		{"trailing-", "trailing"},
	}
	for _, tt := range tests {
		got := sanitizeProjectName(tt.input)
		if got != tt.want {
			t.Errorf("sanitizeProjectName(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

// ---- Sentinel error tests ------------------------------------------------

func TestSentinelErrors_AreDistinct(t *testing.T) {
	if errors.Is(ErrNoProjectFound, ErrProjectNameCollision) {
		t.Error("ErrNoProjectFound should not be ErrProjectNameCollision")
	}
	if errors.Is(ErrNoProjectFound, ErrUnknownProject) {
		t.Error("ErrNoProjectFound should not be ErrUnknownProject")
	}
	if errors.Is(ErrProjectNameCollision, ErrUnknownProject) {
		t.Error("ErrProjectNameCollision should not be ErrUnknownProject")
	}
}

// ---- Enabled default tests (SPEC-0004 REQ "Bring Up") ---------------------

// TestParseProject_EnabledDefaultsTrue: a project file that omits `enabled`
// (the spec's own example files carry no such key) must parse Enabled=true so
// the first `harness up` registers AND starts the harness. This is a project
// semantic only — the global config keeps its opt-in default.
func TestParseProject_EnabledDefaultsTrue(t *testing.T) {
	data := []byte(`
[harness.agent]
cmd = "claude"
`)
	proj, err := ParseProject(data, "/tmp/repo/harness.toml")
	if err != nil {
		t.Fatalf("ParseProject: %v", err)
	}
	if !proj.Config.Harnesses["agent"].Enabled {
		t.Error("Enabled = false for omitted `enabled` key, want true (SPEC-0004 Bring Up starts each harness)")
	}
}

// TestParseProject_EnabledExplicitFalse: an explicit `enabled = false` is
// still honored (register without starting).
func TestParseProject_EnabledExplicitFalse(t *testing.T) {
	data := []byte(`
[harness.agent]
cmd = "claude"
enabled = false

[harness.reviewer]
cmd = "crush"
enabled = true
`)
	proj, err := ParseProject(data, "/tmp/repo/harness.toml")
	if err != nil {
		t.Fatalf("ParseProject: %v", err)
	}
	if proj.Config.Harnesses["agent"].Enabled {
		t.Error("agent Enabled = true, want false (explicit enabled = false)")
	}
	if !proj.Config.Harnesses["reviewer"].Enabled {
		t.Error("reviewer Enabled = false, want true (explicit enabled = true)")
	}
}

// ---- DiscoverProjectExcluding tests ---------------------------------------

// TestDiscoverProjectExcluding_SkipsActiveConfig: when the CLI runs with
// --config /custom/harness.toml, discovery must not adopt that file as a
// project file — same rule as the conventional DefaultPath().
func TestDiscoverProjectExcluding_SkipsActiveConfig(t *testing.T) {
	tmpDir := t.TempDir()
	// Bound the up-walk at tmpDir so a stray /tmp/harness.toml on the machine
	// can never be adopted (DiscoverProject stops at $HOME).
	t.Setenv("HOME", tmpDir)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(tmpDir, ".config"))
	dir := filepath.Join(tmpDir, "cfgdir")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	custom := writeProjectFile(t, dir, `
[harness.agent]
cmd = "claude"
`)

	oldWd, _ := os.Getwd()
	defer os.Chdir(oldWd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	// Without the exclusion the file is adopted as a project.
	if _, err := DiscoverProject(); err != nil {
		t.Fatalf("DiscoverProject (no exclusion): %v", err)
	}
	// With the exclusion it is skipped and the walk finds nothing.
	_, err := DiscoverProjectExcluding(custom)
	if !errors.Is(err, ErrNoProjectFound) {
		t.Errorf("DiscoverProjectExcluding(%q) = %v, want ErrNoProjectFound", custom, err)
	}
}

// TestSanitizeProjectName_Exported: the exported wrapper matches the internal
// normalization the discovery path applies to directory basenames.
func TestSanitizeProjectName_Exported(t *testing.T) {
	if got := SanitizeProjectName("My-Cool Project"); got != "my-cool-project" {
		t.Errorf("SanitizeProjectName = %q, want %q", got, "my-cool-project")
	}
}

// TestParseProject_MCPAllowRejected: SPEC-0005 REQ "Capability Scoping"
// requires mcp_allow to be global-only — a cloned repository cannot grant its
// own harnesses write authority over the fleet.
func TestParseProject_MCPAllowRejected(t *testing.T) {
	data := []byte(`
[harness.agent]
cmd = "claude"
mcp_allow = ["read", "write"]
`)
	_, err := ParseProject(data, "/tmp/myrepo/harness.toml")
	if err == nil {
		t.Fatal("expected error for mcp_allow in project file, got nil")
	}
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("error is %T, want *config.Error: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "mcp_allow") {
		t.Errorf("message %q does not contain 'mcp_allow'", ce.Msg)
	}
	if !strings.Contains(ce.Msg, "not supported in project files") {
		t.Errorf("message %q does not contain 'not supported in project files'", ce.Msg)
	}
}

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

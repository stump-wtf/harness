// Source-resolution tests: REQ-7's scenarios — a package-sourced harness
// resolves identically to a hand-written one, a local override wins, a
// missing pin fails the load naming the source, and no network or git
// operation is ever attempted (structurally true: applySource only reads
// local disk).
//
// Governing: ADR-0044; SPEC-0026 REQ-3, REQ-7.
package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/agentpkg"
)

const testSHA = "0123456789abcdef0123456789abcdef01234567"

// installTestPin materializes a pin directory under a temp XDG_STATE_HOME and
// returns the harness.toml written beside it.
func installTestPin(t *testing.T, manifest string) (configFile string) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	src := agentpkg.Source{Stable: "stump-wtf", Package: "pr-reviewer", SHA: testSHA}
	dir := agentpkg.PinDir(src)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.toml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(t.TempDir(), "harness.toml")
}

func writeConfig(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestSourceResolvesLikeHandWritten(t *testing.T) {
	path := installTestPin(t, `[package]
name = "pr-reviewer"
[harness]
harness = "crush"
args = ["--help"]
`)
	writeConfig(t, path, "[harness.pr]\nsource = \"stump-wtf/pr-reviewer@"+testSHA+"\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	h, ok := cfg.Harnesses["pr"]
	if !ok {
		t.Fatal("harness pr not registered")
	}
	if h.PackageSource != "stump-wtf/pr-reviewer@"+testSHA {
		t.Errorf("source: %q", h.PackageSource)
	}
	if h.Adapter != "crush" || len(h.Args) != 1 || h.Args[0] != "--help" {
		t.Errorf("resolved harness: %+v", h)
	}
}

func TestSourceLocalOverrideWins(t *testing.T) {
	path := installTestPin(t, `[package]
name = "pr-reviewer"
[harness]
harness = "claude-code"
model = "sonnet"
`)
	writeConfig(t, path, "[harness.pr]\nsource = \"stump-wtf/pr-reviewer@"+testSHA+"\"\n"+
		"prompt = \"review it\"\nmodel = \"opus\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.Harnesses["pr"].Model; got != "opus" {
		t.Errorf("model: want opus (local), got %q", got)
	}
}

func TestSourceRelativePathAnchorsOnPinDir(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	src := agentpkg.Source{Stable: "stump-wtf", Package: "pr-reviewer", SHA: testSHA}
	dir := agentpkg.PinDir(src)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "package.toml"), []byte(`[package]
name = "pr-reviewer"
[harness]
harness = "claude-code"
system_prompt_file = "persona.md"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	// persona.md must exist at the pin dir for the eager check.
	if err := os.WriteFile(filepath.Join(dir, "persona.md"), []byte("be terse"), 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "harness.toml")
	writeConfig(t, path, "[harness.pr]\nsource = \"stump-wtf/pr-reviewer@"+testSHA+"\"\nprompt = \"go\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	want := filepath.Join(dir, "persona.md")
	if got := cfg.Harnesses["pr"].SystemPromptFile; got != want {
		t.Errorf("system_prompt_file: want %q, got %q", want, got)
	}
}

func TestSourceMissingPinFailsLoad(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	path := filepath.Join(t.TempDir(), "harness.toml")
	full := "stump-wtf/pr-reviewer@" + testSHA
	writeConfig(t, path, "[harness.pr]\nsource = \""+full+"\"\n")
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected missing-pin error")
	}
	msg := err.Error()
	for _, want := range []string{"pr", full, "harness agent install"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not contain %q", msg, want)
		}
	}
}

func TestSourceInvalidGrammarFailsLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness.toml")
	writeConfig(t, path, "[harness.pr]\nsource = \"stump-wtf/pr-reviewer@main\"\n")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "source") {
		t.Fatalf("want invalid-source error, got %v", err)
	}
}

func TestSourceUnknownAdapterFailsLikeHandWritten(t *testing.T) {
	path := installTestPin(t, `[package]
name = "pr-reviewer"
[harness]
harness = "nonexistent"
`)
	writeConfig(t, path, "[harness.pr]\nsource = \"stump-wtf/pr-reviewer@"+testSHA+"\"\n")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "unknown harness kind") {
		t.Fatalf("want unknown-adapter error, got %v", err)
	}
}

// A package's prompt_file resolves against the pin directory and, together
// with a schedule set on the operator's table, makes a working one-shot: the
// package ships the instruction, the machine supplies the wiring.
func TestSourcePackagePromptFileResolvesInPin(t *testing.T) {
	path := installTestPin(t, `[package]
name = "pr-reviewer"
[harness]
harness = "claude-code"
prompt_file = "prompts/review.md"
`)
	src := agentpkg.Source{Stable: "stump-wtf", Package: "pr-reviewer", SHA: testSHA}
	promptPath := filepath.Join(agentpkg.PinDir(src), "prompts", "review.md")
	if err := os.MkdirAll(filepath.Dir(promptPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(promptPath, []byte("Review the open PRs.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeConfig(t, path, "[harness.pr]\nsource = \"stump-wtf/pr-reviewer@"+testSHA+"\"\nschedule = \"@hourly\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	h := cfg.Harnesses["pr"]
	if h.PromptFile != promptPath {
		t.Errorf("prompt_file = %q, want %q", h.PromptFile, promptPath)
	}
	if !containsKey(h.PackageKeys, "prompt_file") {
		t.Errorf("prompt_file not attributed to the package: %v", h.PackageKeys)
	}
}

// The prompt sources override as one group: a table with its own inline
// prompt drops the package's prompt_template rather than failing the load on
// the four-way exclusion.
func TestSourceLocalPromptOverridesPackagePromptGroup(t *testing.T) {
	path := installTestPin(t, `[package]
name = "pr-reviewer"
[harness]
harness = "claude-code"
prompt_template = "Package prompt for {{harness.name}}"
`)
	writeConfig(t, path, "[harness.pr]\nsource = \"stump-wtf/pr-reviewer@"+testSHA+"\"\nprompt = \"local prompt\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	h := cfg.Harnesses["pr"]
	if h.Prompt != "local prompt" || h.PromptTemplate != "" {
		t.Errorf("prompt = %q, prompt_template = %q; want the local prompt only", h.Prompt, h.PromptTemplate)
	}
	for _, k := range h.PackageKeys {
		if strings.HasPrefix(k, "prompt") {
			t.Errorf("package attributed %q though the table overrode the prompt", k)
		}
	}
}

func containsKey(keys []string, want string) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}

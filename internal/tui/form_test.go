package tui

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// TestHarnessFormRoundTrip verifies the SPEC-0001 REQ "Harness Form" write path:
// a completed form serializes to TOML that config.Parse (the daemon's parser,
// ADR-0006) accepts, yielding an equivalent harness — so "the new harness lands
// in harness.toml, the daemon reloads, and it appears on the dashboard".
func TestHarnessFormRoundTrip(t *testing.T) {
	f := HarnessForm{
		Name:         "reduit-agent",
		Cmd:          "crush",
		Args:         []string{"--yolo", "--data-dir", "/tmp/x"},
		Workdir:      "~/.local/share/reduit",
		EnvFile:      "~/.config/vault/secrets.env",
		RestartDelay: 5,
		Restart:      "on-failure",
		Backend:      "native",
		Description:  "the reduit agent",
		Enabled:      true,
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("valid form rejected: %v", err)
	}

	body := AppendHarness([]byte("[harness.existing]\ncmd = \"true\"\n"), f)
	cfg, err := config.Parse(body, "harness.toml")
	if err != nil {
		t.Fatalf("config.Parse rejected form TOML: %v\n---\n%s", err, body)
	}

	h, ok := cfg.Harnesses["reduit-agent"]
	if !ok {
		t.Fatalf("harness not present after parse; got %v", cfg.HarnessOrder)
	}
	if h.Cmd != "crush" {
		t.Errorf("cmd = %q, want crush", h.Cmd)
	}
	if len(h.Args) != 3 || h.Args[0] != "--yolo" {
		t.Errorf("args round-trip wrong: %v", h.Args)
	}
	if h.RestartDelay.Seconds() != 5 {
		t.Errorf("restart_delay = %v, want 5s", h.RestartDelay)
	}
	if h.Restart != core.RestartOnFailure {
		t.Errorf("restart = %q, want on-failure", h.Restart)
	}
	if !h.Enabled {
		t.Error("enabled did not round-trip")
	}
	// The pre-existing harness must survive the append (non-destructive write).
	if _, ok := cfg.Harnesses["existing"]; !ok {
		t.Error("append clobbered the existing harness")
	}
}

// TestHarnessFormRoundTripPrompt: `n` can author a prompt-only harness — the
// form emits `prompt` and no cmd/args (ADR-0011 spawn-time synthesis), and the
// TOML re-parses into a prompt harness with the one-shot restart="no" default
// intact.
func TestHarnessFormRoundTripPrompt(t *testing.T) {
	f := HarnessForm{
		Name:        "deploy-check",
		Prompt:      "check the deployments and report anything unhealthy",
		Workdir:     "~/src/my-project",
		Restart:     string(core.RestartNo),
		Backend:     "native",
		Description: "one-shot agent run",
	}
	if err := f.Validate(); err != nil {
		t.Fatalf("valid prompt form rejected: %v", err)
	}
	body := f.TOML()
	if !strings.Contains(body, "prompt = ") {
		t.Fatalf("TOML missing prompt key:\n%s", body)
	}
	if strings.Contains(body, "cmd = ") || strings.Contains(body, "args = ") {
		t.Fatalf("prompt harness TOML must not carry cmd/args:\n%s", body)
	}
	cfg, err := config.Parse([]byte(body), "harness.toml")
	if err != nil {
		t.Fatalf("config.Parse rejected form TOML: %v\n---\n%s", err, body)
	}
	h, ok := cfg.Harnesses["deploy-check"]
	if !ok {
		t.Fatalf("harness not present after parse; got %v", cfg.HarnessOrder)
	}
	if h.Prompt != f.Prompt {
		t.Errorf("Prompt = %q, want %q (multi-word prompt must survive verbatim)", h.Prompt, f.Prompt)
	}
	if h.Cmd != "" || h.Args != nil {
		t.Errorf("Cmd/Args = %q/%v, want empty (spawn-time synthesis)", h.Cmd, h.Args)
	}
	if h.Restart != core.RestartNo {
		t.Errorf("Restart = %q, want %q (one-shot default)", h.Restart, core.RestartNo)
	}
}

// TestEditPromptHarnessRoundTrip: editing a prompt harness with `e` and saving
// unchanged round-trips the `prompt` key losslessly — parse no longer desugars
// prompt into cmd/args, so the pre-fill sees the real field and the rewrite
// emits it back.
func TestEditPromptHarnessRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "harness.toml")
	original := "[harness.deploy-check]\nprompt = \"check the deployments and report anything unhealthy\"\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	sel := protocol.HarnessInfo{
		Name:   "deploy-check",
		Prompt: "check the deployments and report anything unhealthy",
	}
	fi := editInputsFor(path, sel)
	if fi.prompt != "check the deployments and report anything unhealthy" {
		t.Fatalf("prompt not pre-filled: %q", fi.prompt)
	}
	if fi.cmd != "" {
		t.Fatalf("cmd pre-filled for a prompt harness: %q", fi.cmd)
	}
	if fi.restart != string(core.RestartNo) {
		t.Errorf("restart pre-fill = %q, want %q (one-shot default)", fi.restart, core.RestartNo)
	}

	// Save unchanged: the rewritten table must carry the prompt key and parse
	// back to the identical harness.
	form := fi.toForm()
	if err := form.Validate(); err != nil {
		t.Fatalf("unchanged edit failed validation: %v", err)
	}
	body := []byte(removeHarnessTOML(original, form.Name))
	body = AppendHarness(body, form)
	if !strings.Contains(string(body), "prompt = ") {
		t.Fatalf("prompt key lost on save:\n%s", body)
	}

	cfg, err := config.Parse(body, "harness.toml")
	if err != nil {
		t.Fatalf("edited config did not parse: %v\n%s", err, body)
	}
	before, err := config.Parse([]byte(original), "harness.toml")
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cfg.Harnesses["deploy-check"], before.Harnesses["deploy-check"]) {
		t.Errorf("unchanged edit not lossless:\n got %+v\nwant %+v",
			cfg.Harnesses["deploy-check"], before.Harnesses["deploy-check"])
	}
}

// TestEditPreservesOmittedFields is the regression guard for the SPEC-0001 REQ
// "Harness Form" scenario "e SHALL pre-fill from the existing harness": editing a
// harness must NOT drop the keys the daemon's HarnessInfo projection omits
// (args/workdir/env_file/restart_delay). The edit save path rewrites the whole
// `[harness.<name>]` table, so a partial pre-fill silently wiped those keys
// (data loss). editInputsFor loads the full table from the config file
// (file-is-truth, ADR-0006) to guarantee a lossless round-trip.
func TestEditPreservesOmittedFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "harness.toml")
	original := strings.Join([]string{
		"[harness.reduit-agent]",
		`cmd = "crush"`,
		`args = ["--yolo", "--data-dir", "/tmp/x"]`,
		`workdir = "~/.local/share/reduit"`,
		`env_file = "~/.config/vault/secrets.env"`,
		"restart_delay = 5",
		`restart = "on-failure"`,
		`description = "the reduit agent"`,
		"enabled = true",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	// Simulate the lossy daemon projection the TUI would have on the dashboard:
	// name/cmd/backend/description/enabled only — no args/workdir/env_file/delay.
	sel := protocol.HarnessInfo{
		Name:        "reduit-agent",
		Cmd:         "crush",
		Description: "the reduit agent",
		Enabled:     true,
	}
	fi := editInputsFor(path, sel)
	if fi.args != "--yolo --data-dir /tmp/x" {
		t.Errorf("args not pre-filled from file: %q", fi.args)
	}
	if fi.workdir != "~/.local/share/reduit" {
		t.Errorf("workdir not pre-filled: %q", fi.workdir)
	}
	if fi.envFile != "~/.config/vault/secrets.env" {
		t.Errorf("env_file not pre-filled: %q", fi.envFile)
	}
	if fi.delay != "5" {
		t.Errorf("restart_delay not pre-filled: %q", fi.delay)
	}
	if fi.restart != "on-failure" {
		t.Errorf("restart not pre-filled: %q", fi.restart)
	}

	// Now drive the full edit-save path (change only the description) and confirm
	// the omitted keys survive into the reparsed config.
	form := fi.toForm()
	form.Description = "edited description"
	body := []byte(removeHarnessTOML(original, form.Name))
	body = AppendHarness(body, form)

	cfg, err := config.Parse(body, "harness.toml")
	if err != nil {
		t.Fatalf("edited config did not parse: %v\n%s", err, body)
	}
	h, ok := cfg.Harnesses["reduit-agent"]
	if !ok {
		t.Fatalf("harness lost after edit; got %v", cfg.HarnessOrder)
	}
	if len(h.Args) != 3 || h.Args[0] != "--yolo" {
		t.Errorf("args wiped by edit: %v", h.Args)
	}
	if h.Workdir != "~/.local/share/reduit" {
		t.Errorf("workdir wiped by edit: %q", h.Workdir)
	}
	if h.EnvFile != "~/.config/vault/secrets.env" {
		t.Errorf("env_file wiped by edit: %q", h.EnvFile)
	}
	if h.RestartDelay.Seconds() != 5 {
		t.Errorf("restart_delay wiped by edit: %v", h.RestartDelay)
	}
	if h.Restart != core.RestartOnFailure {
		t.Errorf("restart wiped by edit: %q", h.Restart)
	}
	if h.Description != "edited description" {
		t.Errorf("description edit did not take: %q", h.Description)
	}
}

// TestFormValidate covers the pre-write guard rails.
func TestFormValidate(t *testing.T) {
	if err := (HarnessForm{Cmd: "x"}).Validate(); err == nil {
		t.Error("missing name should fail")
	}
	if err := (HarnessForm{Name: "x"}).Validate(); err == nil {
		t.Error("missing cmd should fail")
	}
	if err := (HarnessForm{Name: "x", Cmd: "y", Backend: "bogus"}).Validate(); err == nil {
		t.Error("bad backend should fail")
	}
	if err := (HarnessForm{Name: "x", Cmd: "y", RestartDelay: -1}).Validate(); err == nil {
		t.Error("negative delay should fail")
	}
	if err := (HarnessForm{Name: "x", Cmd: "y", Restart: "until-pigs-fly"}).Validate(); err == nil {
		t.Error("bad restart policy should fail")
	}
	if err := (HarnessForm{Name: "x", Cmd: "y", Prompt: "z"}).Validate(); err == nil {
		t.Error("cmd+prompt should fail (mutually exclusive)")
	}
	if err := (HarnessForm{Name: "x", Prompt: "z", Args: []string{"a"}}).Validate(); err == nil {
		t.Error("prompt+args should fail (args belong to cmd)")
	}
	if err := (HarnessForm{Name: "x", Prompt: "do the thing"}).Validate(); err != nil {
		t.Errorf("prompt-only form should validate: %v", err)
	}
}

// TestRemoveHarnessTOML verifies delete drops exactly the target table and keeps
// the rest of the file (ADR-0006 file-is-truth; SPEC-0001 delete guard).
func TestRemoveHarnessTOML(t *testing.T) {
	src := strings.Join([]string{
		"[harness.keep]",
		"cmd = \"a\"",
		"",
		"[harness.drop]",
		"cmd = \"b\"",
		"description = \"gone\"",
		"",
		"[profile.p]",
		"harnesses = [\"keep\"]",
		"",
	}, "\n")

	out := removeHarnessTOML(src, "drop")
	if strings.Contains(out, "harness.drop") || strings.Contains(out, "gone") {
		t.Fatalf("drop table survived:\n%s", out)
	}
	cfg, err := config.Parse([]byte(out), "harness.toml")
	if err != nil {
		t.Fatalf("post-delete config invalid: %v\n%s", err, out)
	}
	if _, ok := cfg.Harnesses["keep"]; !ok {
		t.Error("keep harness was lost")
	}
	if _, ok := cfg.Harnesses["drop"]; ok {
		t.Error("drop harness still parsed")
	}
	if _, ok := cfg.Profiles["p"]; !ok {
		t.Error("profile p was lost")
	}
}

// TestToFormParsesArgsAndDelay verifies the Huh string inputs convert to the
// typed form (space-split args, integer delay).
func TestToFormParsesArgsAndDelay(t *testing.T) {
	fi := formInputs{name: " n ", cmd: " c ", args: "a b  c", delay: "7", backend: "native"}
	f := fi.toForm()
	if f.Name != "n" || f.Cmd != "c" {
		t.Errorf("trim failed: %+v", f)
	}
	if len(f.Args) != 3 {
		t.Errorf("args = %v, want 3", f.Args)
	}
	if f.RestartDelay != 7 {
		t.Errorf("delay = %d, want 7", f.RestartDelay)
	}
}

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// writePromptFile drops a prompt file in a fresh temp dir and returns its
// absolute path. Absolute keeps these tests independent of the process cwd,
// which is what a bare filename in Parse would otherwise resolve against.
func writePromptFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sweep.prompt.md")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestParsePromptFile covers the happy path of the ADR-0018 external prompt
// source: the resolved PATH lands on the harness, the file's CONTENTS do not
// (they are read at spawn), args stay empty, and the one-shot restart default
// applies exactly as it does for an inline prompt.
// Governing: SPEC-0006 REQ "Prompt Source".
func TestParsePromptFile(t *testing.T) {
	body := "Read the deploy logs and report anything unhealthy.\n"
	path := writePromptFile(t, body)
	toml := "[harness.sweep]\nharness = \"claude-code\"\nprompt_file = \"" + path + "\"\n"

	cfg, err := Parse([]byte(toml), "test.toml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	h, ok := cfg.Harnesses["sweep"]
	if !ok {
		t.Fatalf("sweep harness missing; order = %v", cfg.HarnessOrder)
	}
	if h.PromptFile != path {
		t.Errorf("PromptFile = %q, want %q", h.PromptFile, path)
	}
	// The load must not read the document into config truth: doing so is what
	// would inline it into harness.toml on the next writer round-trip.
	if h.Prompt != "" {
		t.Errorf("Prompt = %q, want empty (contents are read at spawn, not parse)", h.Prompt)
	}
	if strings.Contains(h.Prompt, "deploy logs") {
		t.Error("Prompt carries the file's contents; only the path belongs on the harness")
	}
	if h.Args != nil {
		t.Errorf("Args = %v, want empty (argv is synthesized at spawn)", h.Args)
	}
	if h.Restart != core.RestartNo {
		t.Errorf("Restart = %q, want %q (a one-shot must not respawn)", h.Restart, core.RestartNo)
	}
	if !h.IsAgent() {
		t.Error("IsAgent() = false, want true for a prompt_file harness")
	}
}

// TestParsePromptFileSatisfiesPromptDependentKeys pins the predicate widening:
// model/auto_accept/max_turns/quiet/schedule all require "a prompt", and
// prompt_file supplies one. A check left as `prompt == ""` would reject each of
// these with a misleading "requires prompt".
func TestParsePromptFileSatisfiesPromptDependentKeys(t *testing.T) {
	path := writePromptFile(t, "sweep the fleet")
	toml := strings.Join([]string{
		"[harness.sweep]",
		`harness = "claude-code"`,
		`prompt_file = "` + path + `"`,
		`model = "claude-opus-5"`,
		"auto_accept = true",
		"max_turns = 40",
		"quiet = false",
		`schedule = "0 9 * * 1"`,
		"",
	}, "\n")

	cfg, err := Parse([]byte(toml), "test.toml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	h := cfg.Harnesses["sweep"]
	if h.Model != "claude-opus-5" || !h.AutoAccept || h.MaxTurns != 40 || h.Quiet {
		t.Errorf("prompt-dependent keys not carried: %+v", h)
	}
	if h.Schedule != "0 9 * * 1" {
		t.Errorf("Schedule = %q, want the cron expression", h.Schedule)
	}
}

// TestParsePromptFileResolution: a relative prompt_file resolves against the
// directory holding the config file, matching workdir and env_file — the daemon
// runs from systemd, where the process cwd is not the config's directory.
func TestParsePromptFileResolution(t *testing.T) {
	dir := t.TempDir()
	prompt := filepath.Join(dir, "sweep.prompt.md")
	if err := os.WriteFile(prompt, []byte("sweep"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(dir, "harness.toml")
	body := "[harness.sweep]\nharness = \"claude-code\"\nprompt_file = \"sweep.prompt.md\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.Harnesses["sweep"].PromptFile; got != prompt {
		t.Errorf("PromptFile = %q, want it resolved against the config dir (%q)", got, prompt)
	}
}

// TestParsePromptFileErrors is the located-error contract for every way a
// prompt_file can be wrong. The missing/empty cases are the point of validating
// eagerly at all: a scheduled one-shot pointed at a file that is not there must
// fail the load, not fire on cron into a run with no instructions.
// Governing: SPEC-0006 REQ "Prompt Source".
func TestParsePromptFileErrors(t *testing.T) {
	good := writePromptFile(t, "sweep")
	empty := writePromptFile(t, "   \n\t\n")
	dir := t.TempDir()
	missing := filepath.Join(dir, "nope.md")

	tests := []struct {
		name     string
		toml     string
		wantLine int
		wantSub  string
	}{
		{
			name:     "blank prompt_file is named as such",
			toml:     "[harness.bad]\nharness = \"crush\"\nprompt_file = \"   \"\n",
			wantLine: 1,
			wantSub:  `"prompt_file" must not be blank`,
		},
		{
			name:     "prompt and prompt_file are mutually exclusive",
			toml:     "[harness.bad]\nharness = \"crush\"\nprompt = \"sweep\"\nprompt_file = \"" + good + "\"\n",
			wantLine: 1,
			wantSub:  `"prompt" and "prompt_file" are mutually exclusive`,
		},
		{
			name:     "prompt_file and args are mutually exclusive",
			toml:     "[harness.bad]\nharness = \"generic\"\nargs = [\"-c\", \"sleep 1\"]\nprompt_file = \"" + good + "\"\n",
			wantLine: 1,
			wantSub:  `"prompt_file" and "args" are mutually exclusive`,
		},
		{
			name:     "missing file fails the load",
			toml:     "[harness.bad]\nharness = \"crush\"\nprompt_file = \"" + missing + "\"\n",
			wantLine: 1,
			wantSub:  "does not exist",
		},
		{
			name:     "a directory is not a prompt",
			toml:     "[harness.bad]\nharness = \"crush\"\nprompt_file = \"" + dir + "\"\n",
			wantLine: 1,
			wantSub:  "is a directory",
		},
		{
			name:     "an empty file is an error, not an empty prompt",
			toml:     "[harness.bad]\nharness = \"crush\"\nprompt_file = \"" + empty + "\"\n",
			wantLine: 1,
			wantSub:  "is empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.toml), "test.toml")
			if err == nil {
				t.Fatal("Parse succeeded, want an error")
			}
			ce, ok := err.(*Error)
			if !ok {
				t.Fatalf("error is %T, want *config.Error (callers rely on file:line)", err)
			}
			if ce.Line != tt.wantLine {
				t.Errorf("Line = %d, want %d", ce.Line, tt.wantLine)
			}
			if !strings.Contains(ce.Msg, tt.wantSub) {
				t.Errorf("message %q does not contain %q", ce.Msg, tt.wantSub)
			}
			if !strings.Contains(ce.Msg, "bad") {
				t.Errorf("message %q does not name the harness", ce.Msg)
			}
		})
	}
}

// TestParsePromptFileErrorNamesResolvedPath: the error must quote the path the
// daemon would actually open, not the relative spelling in the file — otherwise
// "./sweep.md does not exist" sends the operator looking in the wrong directory.
func TestParsePromptFileErrorNamesResolvedPath(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "harness.toml")
	body := "[harness.sweep]\nharness = \"claude-code\"\nprompt_file = \"sweep.prompt.md\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := Load(cfgPath)
	if err == nil {
		t.Fatal("Load succeeded, want an error for the missing prompt file")
	}
	want := filepath.Join(dir, "sweep.prompt.md")
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not name the resolved path %q", err, want)
	}
}

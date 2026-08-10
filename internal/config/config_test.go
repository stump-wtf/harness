package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// TestParseZshHarnessdExample is the acceptance criterion from issue #4:
// "Today's bare-table zsh-harnessd config parses unchanged." The fixture is a
// verbatim copy of examples/harnessd.toml from the zsh-harnessd plugin.
func TestParseZshHarnessdExample(t *testing.T) {
	cfg, err := Load(filepath.Join("testdata", "zsh-harnessd.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if got, want := len(cfg.Harnesses), 1; got != want {
		t.Fatalf("harness count = %d, want %d", got, want)
	}
	if got := len(cfg.Profiles); got != 0 {
		t.Fatalf("profile count = %d, want 0", got)
	}

	h, ok := cfg.Harnesses["crush-signal-channel"]
	if !ok {
		t.Fatalf("crush-signal-channel harness missing; got order %v", cfg.HarnessOrder)
	}
	want := core.Harness{
		Name:         "crush-signal-channel",
		Cmd:          "crush",
		Args:         []string{"--yolo", "--data-dir", "{workdir}", "--channels", "server:signal"},
		Workdir:      "~/.local/share/crush-signal-channel",
		EnvFile:      "~/.config/vault/secrets-static.env",
		RestartDelay: 5 * time.Second,
		Restart:      core.RestartAlways, // defaulted, not present in the file
		Backend:      core.BackendNative, // defaulted, not present in the file
		Enabled:      false,              // defaulted
	}
	if !reflect.DeepEqual(h, want) {
		t.Errorf("harness mismatch:\n got %+v\nwant %+v", h, want)
	}
}

// TestParseProfiles exercises the new [harness.*] + [profile.*] schema and
// order preservation (ADR-0006).
func TestParseProfiles(t *testing.T) {
	cfg, err := Load(filepath.Join("testdata", "profiles.toml"))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	wantHarnessOrder := []string{"claude-src", "crush-signal", "reduit-agent"}
	if !reflect.DeepEqual(cfg.HarnessOrder, wantHarnessOrder) {
		t.Errorf("HarnessOrder = %v, want %v", cfg.HarnessOrder, wantHarnessOrder)
	}
	wantProfileOrder := []string{"default", "signal-ops", "reduit"}
	if !reflect.DeepEqual(cfg.ProfileOrder, wantProfileOrder) {
		t.Errorf("ProfileOrder = %v, want %v", cfg.ProfileOrder, wantProfileOrder)
	}

	// tmux backend + socket round-trips.
	if h := cfg.Harnesses["reduit-agent"]; h.Backend != core.BackendTmux || h.TmuxSocket != "reduit" {
		t.Errorf("reduit-agent backend/socket = %q/%q, want tmux/reduit", h.Backend, h.TmuxSocket)
	}

	// default profile autostarts; signal-ops does not.
	if p := cfg.Profiles["default"]; !p.Autostart {
		t.Error("default profile should autostart")
	}
	if p := cfg.Profiles["signal-ops"]; p.Autostart {
		t.Error("signal-ops profile should not autostart")
	}
	if p := cfg.Profiles["signal-ops"]; p.Description != "Headless agents wired to Signal" {
		t.Errorf("signal-ops description = %q", p.Description)
	}

	// AutostartHarnesses = only default's members (claude-src).
	if got, want := cfg.AutostartHarnesses(), []string{"claude-src"}; !reflect.DeepEqual(got, want) {
		t.Errorf("AutostartHarnesses = %v, want %v", got, want)
	}
}

// TestBareEqualsNamespaced asserts the ADR-0006 back-compat promise: a bare
// [name] table and an explicit [harness.name] table decode identically.
func TestBareEqualsNamespaced(t *testing.T) {
	bare := "[foo]\ncmd = \"claude\"\nworkdir = \"~/src\"\n"
	namespaced := "[harness.foo]\ncmd = \"claude\"\nworkdir = \"~/src\"\n"

	a, err := Parse([]byte(bare), "bare.toml")
	if err != nil {
		t.Fatalf("bare parse: %v", err)
	}
	b, err := Parse([]byte(namespaced), "ns.toml")
	if err != nil {
		t.Fatalf("namespaced parse: %v", err)
	}
	if !reflect.DeepEqual(a.Harnesses["foo"], b.Harnesses["foo"]) {
		t.Errorf("bare vs namespaced differ:\n%+v\n%+v", a.Harnesses["foo"], b.Harnesses["foo"])
	}
}

// TestMultilineStringWithBracketLine guards the source-scan against mistaking a
// bracketed line inside a multi-line string value for a real table header. The
// decoder's key set is authoritative; a [line] inside a string is not a table.
func TestMultilineStringWithBracketLine(t *testing.T) {
	src := "[harness.foo]\ncmd = \"echo\"\ndescription = \"\"\"\n[not a table]\nstill the description\n\"\"\"\n"
	cfg, err := Parse([]byte(src), "t.toml")
	if err != nil {
		t.Fatalf("valid TOML with bracketed line in a string was rejected: %v", err)
	}
	h, ok := cfg.Harnesses["foo"]
	if !ok {
		t.Fatalf("foo harness missing; order = %v", cfg.HarnessOrder)
	}
	if !strings.Contains(h.Description, "[not a table]") {
		t.Errorf("description lost its bracketed line: %q", h.Description)
	}
	if got := cfg.HarnessOrder; !reflect.DeepEqual(got, []string{"foo"}) {
		t.Errorf("HarnessOrder = %v, want [foo] (no phantom table)", got)
	}
}

// TestValidationErrors is table-driven over every validation rule, asserting
// both that parsing fails and that the error carries the offending line
// (SPEC-0001 reload banner needs the location).
func TestValidationErrors(t *testing.T) {
	tests := []struct {
		name     string
		toml     string
		wantLine int
		wantSub  string
	}{
		{
			name:     "missing cmd",
			toml:     "[foo]\nworkdir = \"~/src\"\n",
			wantLine: 1,
			wantSub:  `missing required key "cmd"`,
		},
		{
			name:     "invalid backend",
			toml:     "[harness.foo]\ncmd = \"x\"\nbackend = \"screen\"\n",
			wantLine: 1,
			wantSub:  "invalid backend",
		},
		{
			name:     "negative restart_delay",
			toml:     "# header\n\n[foo]\ncmd = \"x\"\nrestart_delay = -3\n",
			wantLine: 3,
			wantSub:  "restart_delay must not be negative",
		},
		{
			name:     "profile references unknown harness",
			toml:     "[harness.a]\ncmd = \"x\"\n\n[profile.p]\nharnesses = [\"a\", \"ghost\"]\n",
			wantLine: 4,
			wantSub:  `references unknown harness "ghost"`,
		},
		{
			name:     "duplicate harness",
			toml:     "[harness.a]\ncmd = \"x\"\n\n[a]\ncmd = \"y\"\n",
			wantLine: 4,
			wantSub:  `duplicate harness "a"`,
		},
		{
			// "/" is reserved for the <project>/<harness> namespace (ADR-0009;
			// SPEC-0004): a quoted-key global name carrying it could clobber a
			// registered project harness.
			name:     "slash in quoted harness name",
			toml:     "[harness.\"reduit/agent\"]\ncmd = \"x\"\n",
			wantLine: 1,
			wantSub:  `must not contain "/"`,
		},
		{
			name:     "slash in bare quoted table name",
			toml:     "[\"a/b\"]\ncmd = \"x\"\n",
			wantLine: 1,
			wantSub:  `must not contain "/"`,
		},
		{
			// A redefined table is illegal TOML, so the decoder rejects it
			// with a location before our own duplicate-profile guard is
			// reached — either way the failure carries the line.
			name:     "duplicate profile table",
			toml:     "[harness.a]\ncmd = \"x\"\n\n[profile.p]\nharnesses = [\"a\"]\n\n[profile.p]\nharnesses = [\"a\"]\n",
			wantLine: 7,
			wantSub:  "already been defined",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.toml), "test.toml")
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			var ce *Error
			if !errors.As(err, &ce) {
				t.Fatalf("error is %T, want *config.Error: %v", err, err)
			}
			if ce.LineNumber() != tt.wantLine {
				t.Errorf("line = %d, want %d (err: %v)", ce.LineNumber(), tt.wantLine, ce)
			}
			if !strings.Contains(ce.Msg, tt.wantSub) {
				t.Errorf("message %q does not contain %q", ce.Msg, tt.wantSub)
			}
		})
	}
}

// TestSyntaxErrorCarriesLine confirms a malformed TOML surfaces as a
// location-carrying *Error, not a bare decoder error.
func TestSyntaxErrorCarriesLine(t *testing.T) {
	// Line 2 has a dangling key with no value — a syntax error.
	src := "[foo]\ncmd =\n"
	_, err := Parse([]byte(src), "bad.toml")
	if err == nil {
		t.Fatal("expected syntax error")
	}
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("error is %T, want *config.Error", err)
	}
	if ce.LineNumber() <= 0 {
		t.Errorf("syntax error should carry a line, got %d (%v)", ce.LineNumber(), ce)
	}
}

// TestLoadMissingFile confirms a missing file returns the os error, not a parse
// error (the TUI distinguishes "no config" from "bad config").
func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "does-not-exist.toml"))
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("want os.ErrNotExist, got %v", err)
	}
}

// TestParseRestartPolicy exercises the restart = "..." directive. An omitted
// key normalizes to the always default so the config layer emits exactly one
// in-memory spelling per behavior.
func TestParseRestartPolicy(t *testing.T) {
	tests := []struct {
		toml string
		want core.RestartPolicy
	}{
		{`[harness.a]` + "\n" + `cmd = "x"` + "\n", core.RestartAlways},
		{`[harness.a]` + "\n" + `cmd = "x"` + "\n" + `restart = "no"` + "\n", core.RestartNo},
		{`[harness.a]` + "\n" + `cmd = "x"` + "\n" + `restart = "always"` + "\n", core.RestartAlways},
		{`[harness.a]` + "\n" + `cmd = "x"` + "\n" + `restart = "unless-stopped"` + "\n", core.RestartUnlessStopped},
		{`[harness.a]` + "\n" + `cmd = "x"` + "\n" + `restart = "on-failure"` + "\n", core.RestartOnFailure},
	}
	for _, tt := range tests {
		cfg, err := Parse([]byte(tt.toml), "test.toml")
		if err != nil {
			t.Fatalf("Parse: %v", err)
		}
		h, ok := cfg.Harnesses["a"]
		if !ok {
			t.Fatal("harness \"a\" not registered")
		}
		if h.Restart != tt.want {
			t.Errorf("restart = %q, want %q", h.Restart, tt.want)
		}
	}
}

// TestParseInvalidRestartPolicy confirms an unknown restart policy is rejected
// at parse time with a location-carrying error.
func TestParseInvalidRestartPolicy(t *testing.T) {
	src := "[harness.foo]\ncmd = \"x\"\nrestart = \"until-pigs-fly\"\n"
	_, err := Parse([]byte(src), "test.toml")
	if err == nil {
		t.Fatal("expected error for invalid restart policy")
	}
	var ce *Error
	if !errors.As(err, &ce) {
		t.Fatalf("error is %T, want *config.Error: %v", err, err)
	}
	if !strings.Contains(ce.Msg, "invalid restart policy") {
		t.Errorf("message %q does not contain \"invalid restart policy\"", ce.Msg)
	}
}

// TestParsePromptHarness covers the agent one-shot `prompt` field (ADR-0011):
// the prompt is stored verbatim and Cmd/Args stay EMPTY — the supervisor
// synthesizes the argv at spawn time, so parse must not desugar. An omitted
// restart defaults to "no" (a one-shot exiting 0 must not respawn); an
// explicit restart still wins.
func TestParsePromptHarness(t *testing.T) {
	tests := []struct {
		name        string
		toml        string
		wantPrompt  string
		wantRestart core.RestartPolicy
	}{
		{
			name:        "prompt stored, restart defaults to no",
			toml:        "[harness.agent]\nprompt = \"check deployments\"\n",
			wantPrompt:  "check deployments",
			wantRestart: core.RestartNo,
		},
		{
			name:        "explicit restart overrides the one-shot default",
			toml:        "[harness.agent]\nprompt = \"check deployments\"\nrestart = \"on-failure\"\n",
			wantPrompt:  "check deployments",
			wantRestart: core.RestartOnFailure,
		},
		{
			name:        "surrounding whitespace is trimmed",
			toml:        "[harness.agent]\nprompt = \"  check deployments \"\n",
			wantPrompt:  "check deployments",
			wantRestart: core.RestartNo,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.toml), "test.toml")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			h, ok := cfg.Harnesses["agent"]
			if !ok {
				t.Fatalf("agent harness missing; order = %v", cfg.HarnessOrder)
			}
			if h.Prompt != tt.wantPrompt {
				t.Errorf("Prompt = %q, want %q", h.Prompt, tt.wantPrompt)
			}
			if h.Cmd != "" || h.Args != nil {
				t.Errorf("Cmd/Args = %q/%v, want empty (argv is synthesized at spawn, not parse)", h.Cmd, h.Args)
			}
			if h.Restart != tt.wantRestart {
				t.Errorf("Restart = %q, want %q", h.Restart, tt.wantRestart)
			}
		})
	}
}

// TestParsePromptErrors is table-driven over the prompt validation rules with
// the same located-error contract as TestValidationErrors: exactly one of
// cmd/prompt, args belong to cmd only, and a blank prompt is named as such
// rather than reported as a missing cmd.
func TestParsePromptErrors(t *testing.T) {
	tests := []struct {
		name     string
		toml     string
		wantLine int
		wantSub  string
	}{
		{
			name:     "prompt and cmd are mutually exclusive",
			toml:     "[harness.bad]\ncmd = \"echo\"\nprompt = \"hello\"\n",
			wantLine: 1,
			wantSub:  `"prompt" and "cmd" are mutually exclusive`,
		},
		{
			name:     "prompt and args are mutually exclusive",
			toml:     "[harness.bad]\nprompt = \"hello\"\nargs = [\"run\"]\n",
			wantLine: 1,
			wantSub:  `"prompt" and "args" are mutually exclusive`,
		},
		{
			name:     "neither cmd nor prompt mentions both options",
			toml:     "# header\n\n[harness.empty]\ndescription = \"no cmd or prompt\"\n",
			wantLine: 3,
			wantSub:  `missing required key "cmd" (or set "prompt"`,
		},
		{
			name:     "whitespace-only prompt names the blank prompt",
			toml:     "[harness.blank]\nprompt = \"   \"\n",
			wantLine: 1,
			wantSub:  `"prompt" must not be blank`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.toml), "test.toml")
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var ce *Error
			if !errors.As(err, &ce) {
				t.Fatalf("error is %T, want *config.Error: %v", err, err)
			}
			if ce.LineNumber() != tt.wantLine {
				t.Errorf("line = %d, want %d (err: %v)", ce.LineNumber(), tt.wantLine, ce)
			}
			if !strings.Contains(ce.Msg, tt.wantSub) {
				t.Errorf("message %q does not contain %q", ce.Msg, tt.wantSub)
			}
		})
	}
}

// TestParseModelHarness covers the agent `model` selection (issue #57): the
// model is stored as config truth on a prompt harness, and Args are NEVER
// touched — no parse-time --model desugaring; the supervisor folds the value
// into the synthesized argv at spawn (core.AgentCommand, ADR-0011).
func TestParseModelHarness(t *testing.T) {
	tests := []struct {
		name      string
		toml      string
		wantModel string
		wantCmd   string
		wantArgs  []string
	}{
		{
			name:      "model stored on a prompt harness, args untouched",
			toml:      "[harness.agent]\nprompt = \"check deployments\"\nmodel = \"claude-opus-5\"\n",
			wantModel: "claude-opus-5",
		},
		{
			name:     "cmd harness without model keeps args verbatim",
			toml:     "[harness.agent]\ncmd = \"echo\"\nargs = [\"hello\"]\n",
			wantCmd:  "echo",
			wantArgs: []string{"hello"},
		},
		{
			name:      "surrounding whitespace is trimmed",
			toml:      "[harness.agent]\nprompt = \"check deployments\"\nmodel = \" claude-opus-5 \"\n",
			wantModel: "claude-opus-5",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.toml), "test.toml")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			h, ok := cfg.Harnesses["agent"]
			if !ok {
				t.Fatalf("agent harness missing; order = %v", cfg.HarnessOrder)
			}
			if h.Model != tt.wantModel {
				t.Errorf("Model = %q, want %q", h.Model, tt.wantModel)
			}
			if h.Cmd != tt.wantCmd {
				t.Errorf("Cmd = %q, want %q", h.Cmd, tt.wantCmd)
			}
			if !reflect.DeepEqual(h.Args, tt.wantArgs) {
				t.Errorf("Args = %v, want %v (model must never be desugared into args)", h.Args, tt.wantArgs)
			}
		})
	}
}

// TestParseModelErrors is table-driven over the model validation rules with
// the same located-error contract as TestParsePromptErrors: `model` requires
// `prompt` (there is no vendor-agnostic injection point in an arbitrary cmd's
// argv — a cmd harness passes --model through its own args), the value is a
// single whitespace-free token, and a blank model is named as such.
func TestParseModelErrors(t *testing.T) {
	tests := []struct {
		name     string
		toml     string
		wantLine int
		wantSub  string
	}{
		{
			name:     "model with cmd points at args",
			toml:     "[harness.bad]\ncmd = \"crush\"\nargs = [\"run\"]\nmodel = \"claude-opus-5\"\n",
			wantLine: 1,
			wantSub:  `"model" requires "prompt"`,
		},
		{
			name:     "whitespace-only model names the blank model",
			toml:     "[harness.bad]\nprompt = \"hi\"\nmodel = \"   \"\n",
			wantLine: 1,
			wantSub:  `"model" must not be blank`,
		},
		{
			name:     "internal whitespace is rejected",
			toml:     "[harness.bad]\nprompt = \"hi\"\nmodel = \"claude opus 5\"\n",
			wantLine: 1,
			wantSub:  `"model" must be a single token`,
		},
		{
			name:     "model alone still reports the missing cmd/prompt first",
			toml:     "# header\n\n[harness.bad]\nmodel = \"claude-opus-5\"\n",
			wantLine: 3,
			wantSub:  `missing required key "cmd" (or set "prompt"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.toml), "test.toml")
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var ce *Error
			if !errors.As(err, &ce) {
				t.Fatalf("error is %T, want *config.Error: %v", err, err)
			}
			if ce.LineNumber() != tt.wantLine {
				t.Errorf("line = %d, want %d (err: %v)", ce.LineNumber(), tt.wantLine, ce)
			}
			if !strings.Contains(ce.Msg, tt.wantSub) {
				t.Errorf("message %q does not contain %q", ce.Msg, tt.wantSub)
			}
		})
	}
}

// TestParseAutoAcceptHarness covers the agent `auto_accept` unattended mode
// (issue #58): the flag is stored as config truth on a prompt harness, and
// Args are NEVER touched — no parse-time --yolo desugaring; the supervisor
// folds the vendor's flag into the synthesized argv at spawn
// (core.AgentCommand, ADR-0011).
func TestParseAutoAcceptHarness(t *testing.T) {
	tests := []struct {
		name           string
		toml           string
		wantAutoAccept bool
		wantCmd        string
		wantArgs       []string
		wantRestart    core.RestartPolicy
	}{
		{
			name:           "auto_accept stored on a prompt harness, args untouched",
			toml:           "[harness.agent]\nprompt = \"check deployments\"\nauto_accept = true\n",
			wantAutoAccept: true,
			wantRestart:    core.RestartNo,
		},
		{
			// The motivating one-shot use case (issue #58): auto_accept
			// requires prompt, and a prompt harness inherits the "no" restart
			// default — so an unattended run exiting 0 must NOT respawn into
			// repeated billed yolo runs. An explicit `restart = ...` still
			// wins.
			name:           "auto_accept prompt harness defaults to restart no",
			toml:           "[harness.agent]\nprompt = \"summarize the day\"\nauto_accept = true\n",
			wantAutoAccept: true,
			wantRestart:    core.RestartNo,
		},
		{
			name:        "absent auto_accept defaults to false",
			toml:        "[harness.agent]\ncmd = \"echo\"\nargs = [\"hello\"]\n",
			wantCmd:     "echo",
			wantArgs:    []string{"hello"},
			wantRestart: core.RestartAlways,
		},
		{
			name:        "explicit false is allowed on a cmd harness",
			toml:        "[harness.agent]\ncmd = \"echo\"\nargs = [\"hello\"]\nauto_accept = false\n",
			wantCmd:     "echo",
			wantArgs:    []string{"hello"},
			wantRestart: core.RestartAlways,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, err := Parse([]byte(tt.toml), "test.toml")
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			h, ok := cfg.Harnesses["agent"]
			if !ok {
				t.Fatalf("agent harness missing; order = %v", cfg.HarnessOrder)
			}
			if h.AutoAccept != tt.wantAutoAccept {
				t.Errorf("AutoAccept = %v, want %v", h.AutoAccept, tt.wantAutoAccept)
			}
			if h.Cmd != tt.wantCmd {
				t.Errorf("Cmd = %q, want %q", h.Cmd, tt.wantCmd)
			}
			if !reflect.DeepEqual(h.Args, tt.wantArgs) {
				t.Errorf("Args = %v, want %v (auto_accept must never be desugared into args)", h.Args, tt.wantArgs)
			}
			if h.Restart != tt.wantRestart {
				t.Errorf("Restart = %q, want %q", h.Restart, tt.wantRestart)
			}
		})
	}
}

// TestParseAutoAcceptErrors: `auto_accept = true` requires `prompt` with the
// same located-error contract as the model rules — there is no vendor-agnostic
// injection point in an arbitrary cmd's argv, so a cmd harness passes its
// tool's flag through its own args.
func TestParseAutoAcceptErrors(t *testing.T) {
	tests := []struct {
		name     string
		toml     string
		wantLine int
		wantSub  string
	}{
		{
			name:     "auto_accept with cmd points at args",
			toml:     "[harness.bad]\ncmd = \"crush\"\nargs = [\"run\"]\nauto_accept = true\n",
			wantLine: 1,
			wantSub:  `"auto_accept" requires "prompt"`,
		},
		{
			name:     "auto_accept alone still reports the missing cmd/prompt first",
			toml:     "# header\n\n[harness.bad]\nauto_accept = true\n",
			wantLine: 3,
			wantSub:  `missing required key "cmd" (or set "prompt"`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.toml), "test.toml")
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			var ce *Error
			if !errors.As(err, &ce) {
				t.Fatalf("error is %T, want *config.Error: %v", err, err)
			}
			if ce.LineNumber() != tt.wantLine {
				t.Errorf("line = %d, want %d (err: %v)", ce.LineNumber(), tt.wantLine, ce)
			}
			if !strings.Contains(ce.Msg, tt.wantSub) {
				t.Errorf("message %q does not contain %q", ce.Msg, tt.wantSub)
			}
		})
	}
}

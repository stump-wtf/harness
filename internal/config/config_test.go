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

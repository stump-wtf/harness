package promptd

import (
	"testing"
	"time"
)

func TestMatchCuratedPrompts(t *testing.T) {
	tests := []struct {
		name  string
		rows  []string
		label string
	}{
		{
			name:  "crush read-outside-workdir dialog",
			rows:  []string{"", "  Read outside the working directories  ", "  /tmp/notes.md", "  ❯ Allow once   Allow for session   Deny"},
			label: "read outside workdir",
		},
		{
			name:  "y/n confirmation tail",
			rows:  []string{"Overwrite config.toml? (y/n) ", "not a match line"},
			label: "y/n prompt",
		},
		{
			name:  "bracketed y/n",
			rows:  []string{"Proceed? [Y/n]"},
			label: "y/n prompt",
		},
		{
			name:  "do you want to",
			rows:  []string{"Do you want to allow MCP server \"github\" to start?", "  Yes   No"},
			label: "do you want",
		},
		{
			name:  "workspace trust",
			rows:  []string{"Do you trust the files in this folder?", "/home/me/proj"},
			label: "trust prompt",
		},
		{
			name:  "press enter wall",
			rows:  []string{"Press Enter to continue"},
			label: "press enter",
		},
		{
			name:  "explicit waiting declaration",
			rows:  []string{"Waiting for your input…"},
			label: "waiting for input",
		},
		{
			name:  "selection menu cursor",
			rows:  []string{"❯ Yes", "  No", "  Cancel"},
			label: "select prompt",
		},
		{
			name:  "case-insensitive",
			rows:  []string{"APPROVE this command? [y/N]"},
			label: "y/n prompt",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m, ok := Match(tc.rows)
			if !ok {
				t.Fatalf("Match(%q) = none, want %q", tc.rows, tc.label)
			}
			if m.Label != tc.label {
				t.Fatalf("label = %q, want %q", m.Label, tc.label)
			}
		})
	}
}

func TestMatchRejectsOrdinaryOutput(t *testing.T) {
	rows := []string{
		"2026-09-26 14:02:11 INFO building 3 targets",
		"error: file main.go, line 42: undefined: Foo",
		"The tool asked for files outside the sandbox in a prior run — noted.",
		"✓ 42 tests, 0 failures",
		"make check  3.2s",
	}
	if m, ok := Match(rows); ok {
		t.Fatalf("ordinary output matched %q (%q)", m.Label, m.Line)
	}
}

func TestMatchSkipsBlankRows(t *testing.T) {
	if _, ok := Match([]string{"", "   ", ""}); ok {
		t.Fatal("blank screen matched")
	}
}

func TestWaitingNeedsIdleAndMatch(t *testing.T) {
	rows := []string{"Proceed? (y/n)"}
	if _, ok := Waiting(rows, 5*time.Second, 30*time.Second); ok {
		t.Fatal("fresh prompt reported waiting before the idle threshold")
	}
	if _, ok := Waiting([]string{"just output"}, 60*time.Second, 30*time.Second); ok {
		t.Fatal("idle screen without a prompt reported waiting")
	}
	m, ok := Waiting(rows, 60*time.Second, 30*time.Second)
	if !ok || m.Label != "y/n prompt" {
		t.Fatalf("idle prompt not reported waiting: ok=%v match=%+v", ok, m)
	}
}

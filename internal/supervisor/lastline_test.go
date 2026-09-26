package supervisor

// Governing tests: SPEC-0003 REQ "Operator Notification"; issue #725 — the
// cause an alert quotes is what the program printed, never the daemon's own
// lifecycle lines around it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLastOutputLineSkipsDaemonLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude-rc.log")
	log := strings.Join([]string{
		"2026/09/25 20:15:10 INFO state changed from=starting to=running",
		"Remote Control starting…",
		"Error: You must be logged in to use Remote Control.",
		"",
		"   ",
		"2026/09/25 20:15:11 INFO exited code=1",
		"2026/09/25 20:15:11 ERRO runaway tool loop: stopped by the daemon tool=x",
		"[harness] dropped a frame the terminal emulator could not render",
		"2026/09/25 20:15:16 INFO state changed from=restarting to=failed",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(log), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, want := LastOutputLine(path), "Error: You must be logged in to use Remote Control."; got != want {
		t.Fatalf("LastOutputLine = %q, want %q", got, want)
	}
}

func TestLastOutputLineReadsOnlyTheTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.log")
	var b strings.Builder
	b.WriteString("the first line, far outside the window\n")
	for b.Len() < 3*lastLineWindow {
		b.WriteString("2026/09/25 20:15:10 INFO state changed from=starting to=running\n")
	}
	b.WriteString("panic: " + strings.Repeat("x", 500) + "\n")
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	got := LastOutputLine(path)
	if !strings.HasPrefix(got, "panic: xxx") || len([]rune(got)) != maxLastLine || !strings.HasSuffix(got, "…") {
		t.Fatalf("LastOutputLine = %q (%d runes), want the capped panic line", got, len([]rune(got)))
	}
}

func TestLastOutputLineNothingToSay(t *testing.T) {
	dir := t.TempDir()
	if got := LastOutputLine(filepath.Join(dir, "missing.log")); got != "" {
		t.Fatalf("missing log = %q", got)
	}
	only := filepath.Join(dir, "only.log")
	if err := os.WriteFile(only, []byte("2026/09/25 20:15:16 INFO state changed from=restarting to=failed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := LastOutputLine(only); got != "" {
		t.Fatalf("daemon-lines-only log = %q, want empty", got)
	}
}

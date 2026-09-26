package main

// End-to-end coverage for the styled `harness logs`.
//
// The styling decision keys off stdout being a terminal, and `go test` never
// gives the code one, so every in-process test would stay green if the
// wiring in cmdLogs/cmdLogEvents were deleted. These exec the real binary
// against a real daemon — once through a pipe, once on a PTY — and assert on
// the bytes each one writes.

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
)

func TestLogsStyledOnTTYAndPlainOtherwise(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)
	socket, _ := startDaemonOnSocket(t, bin, env)
	if out, code := runCLI(t, bin, env, "--socket", socket, "start", "demo"); code != 0 {
		t.Fatalf("start demo exited %d\n%s", code, out)
	}

	// The first run after a daemon boot opens its durable log at starting →
	// running (supervisor.RunSpans).
	const plainLine = "INFO state changed from=starting to=running"
	var piped string
	deadline := time.Now().Add(15 * time.Second)
	for {
		piped, _ = runCLI(t, bin, env, "--socket", socket, "logs", "demo", "--raw")
		if strings.Contains(piped, plainLine) || time.Now().After(deadline) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !strings.Contains(piped, plainLine) {
		t.Fatalf("piped raw log never showed %q\n%s", plainLine, piped)
	}
	if strings.ContainsRune(piped, 0x1b) {
		t.Errorf("piped `harness logs --raw` carries escape sequences; a pipe gets the plain log\n%q", piped)
	}

	const styledLine = "INFO state changed from=◌ starting to=● running"
	for _, args := range [][]string{{"logs", "demo", "--raw"}, {"logs", "demo"}} {
		out := string(runOnPTY(t, bin, append([]string{"--socket", socket}, args...)...))
		if !hasColour(out) {
			t.Errorf("`harness %v` on a TTY is not coloured\n%q", args, out)
		}
		if got := strings.ReplaceAll(ansi.Strip(out), "\r", ""); !strings.Contains(got, styledLine) {
			t.Errorf("`harness %v` on a TTY does not read %q\n%s", args, styledLine, got)
		}

		out = string(runOnPTY(t, bin, append([]string{"--socket", socket, "--json"}, args...)...))
		if strings.ContainsRune(out, 0x1b) {
			t.Errorf("`harness --json %v` on a TTY carries escape sequences\n%q", args, out)
		}

		out = string(runOnPTYEnv(t, bin, []string{"NO_COLOR=1"}, append([]string{"--socket", socket}, args...)...))
		if hasColour(out) {
			t.Errorf("`harness %v` under NO_COLOR is coloured\n%q", args, out)
		}
		if got := strings.ReplaceAll(ansi.Strip(out), "\r", ""); !strings.Contains(got, styledLine) {
			t.Errorf("`harness %v` under NO_COLOR lost its glyphs\n%s", args, got)
		}
	}
}

package main

// End-to-end coverage for "--json suppresses styled help".
//
// TestHasJSONArg covers the pure scan, but the scan is only useful because
// main() calls cliui.SetJSON with it before flag.Parse. Deleting that call
// leaves every in-process test green, because the styling decision also needs
// a TTY on stderr — and `go test` never gives the code one. So these tests
// exec the real binary on a real PTY and assert on the bytes it writes, which
// is the only place the wiring is observable.
//
// The two cases are deliberately different code paths:
//
//   - `harness --json --help` is the pre-parse seed. The flag package calls
//     Usage from inside Parse and then exits, so the authoritative
//     SetJSON(*jsonOut) after Parse never runs; only the seed can suppress
//     the SGR sequences.
//   - `harness daemon --json --help` is the daemon dispatch. Parse halts at
//     the non-flag `daemon`, so *jsonOut is false and the seed has already
//     been overwritten; the help branch has to recover --json from the
//     daemon args itself.

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/charmbracelet/x/xpty"
)

// runOnPTY execs bin with args attached to a pseudo-terminal and returns
// everything it wrote. A PTY is required: the help renderer only styles when
// stderr is a terminal, so a pipe would make every case look plain and the
// assertions vacuous.
func runOnPTY(t *testing.T, bin string, args ...string) []byte {
	t.Helper()
	pty, err := xpty.NewPty(100, 40)
	if err != nil {
		t.Skipf("no PTY available: %v", err)
	}
	defer func() { _ = pty.Close() }()

	cmd := exec.Command(bin, args...)
	// Advertise a color-capable terminal so the styled path is actually
	// reachable; without it a "plain" result would prove nothing. NO_COLOR is
	// dropped rather than overridden — the convention is that any value,
	// including the empty string, disables color.
	cmd.Env = append(colorlessEnvStripped(), "TERM=xterm-256color", "CLICOLOR_FORCE=1")
	if err := pty.Start(cmd); err != nil {
		t.Fatalf("start %s %v on pty: %v", bin, args, err)
	}

	// Drain concurrently with the child. The PTY must be read while the child
	// writes (a full buffer would block it), and the master never sees EOF on
	// its own because this process still holds the slave open — so the close
	// after Wait is what ends the copy.
	var buf bytes.Buffer
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		_, _ = io.Copy(&buf, pty)
	}()
	_ = cmd.Wait()
	_ = pty.Close()
	<-drained
	return buf.Bytes()
}

// colorlessEnvStripped returns the environment minus the variables that
// suppress color, so a developer running with NO_COLOR set does not silently
// turn TestHelpIsStyledOnTTY into a false failure.
func colorlessEnvStripped() []string {
	env := os.Environ()
	out := env[:0]
	for _, kv := range env {
		if strings.HasPrefix(kv, "NO_COLOR=") || strings.HasPrefix(kv, "CLICOLOR=") {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// countCSI counts ANSI control sequence introducers, i.e. the styling this
// asserts on. Counting is more informative than a bool when a case fails.
func countCSI(b []byte) int { return bytes.Count(b, []byte("\x1b[")) }

// TestHelpIsStyledOnTTY is the control: without --json the help must be
// styled, so a "plain" result in the tests below means --json did the work
// rather than the styling being broken or the PTY not being detected.
func TestHelpIsStyledOnTTY(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}
	bin := buildHarnessBinary(t)
	for _, args := range [][]string{{"--help"}, {"daemon", "--help"}} {
		out := runOnPTY(t, bin, args...)
		if countCSI(out) == 0 {
			t.Errorf("`harness %v` on a TTY emitted no SGR sequences; the styled path is not being exercised, so the --json tests below prove nothing\noutput: %q", args, out)
		}
	}
}

// TestJSONSuppressesStyledHelp is the regression: --json must reach the help
// renderer on every path that prints help. Deleting either
// cliui.SetJSON(hasJSONArg(...)) in main() turns one of these red.
func TestJSONSuppressesStyledHelp(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}
	bin := buildHarnessBinary(t)
	for _, tt := range []struct {
		name string
		args []string
	}{
		{"pre-parse seed", []string{"--json", "--help"}},
		{"before the daemon token", []string{"--json", "daemon", "--help"}},
		{"after the daemon token", []string{"daemon", "--json", "--help"}},
		{"after daemon, short form", []string{"daemon", "--json", "-h"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			out := runOnPTY(t, bin, tt.args...)
			if n := countCSI(out); n != 0 {
				t.Errorf("`harness %v` emitted %d ANSI sequences; --json asks for machine-readable output, so help must be plain\noutput: %q", tt.args, n, out)
			}
			// Guard against a vacuous pass from an empty read.
			if !bytes.Contains(out, []byte("harness")) {
				t.Errorf("`harness %v` printed no help at all\noutput: %q", tt.args, out)
			}
		})
	}
}

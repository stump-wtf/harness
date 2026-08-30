package main

// Regression coverage for issue #292.
//
// `harness daemon start --detach` (and the `run` alias, and bare `daemon`)
// never started a daemon: runDaemonCmd handed detachDaemon os.Args[1:], which
// still carries the `daemon start` verb tokens, and detachDaemon re-prepended
// `daemon run`. The child was invoked as `harness daemon run daemon start …`
// and Cobra rejected the second `daemon`, so the child died on an argv error
// and the parent reported "child exited before readiness".
//
// The fix builds the child argv from the resolved daemon settings instead of
// the caller's argv tail. These tests exec the real binary through every
// alias of the daemon command and assert the child actually reaches
// readiness (the socket answers `harness list`).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestDetachChildReachesReadiness execs each `daemon` verb form with --detach
// and asserts the detached child binds the socket. Without the fix every form
// fails with "detach: child exited before readiness" and a log line reading
// `unknown command "daemon" for "harness daemon run"`.
func TestDetachChildReachesReadiness_Regression_Issue292(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}
	bin := buildHarnessBinary(t)
	env, emptyDefault := isolatedEnv(t)

	for _, form := range [][]string{
		{"daemon", "start"},
		{"daemon", "run"},
		{"daemon"},
	} {
		name := strings.Join(form, "-")
		form := form
		t.Run(name, func(t *testing.T) {
			dir := shortSockDir(t)
			socket := filepath.Join(dir, "h.sock")
			cfg := filepath.Join(dir, "harness.toml")
			if err := os.WriteFile(cfg, []byte("[harness.demo]\nharness = \"generic\"\nargs = [\"-c\", \"sleep 600\"]\nenabled = false\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			logFile := filepath.Join(dir, "d.log")

			args := append(append([]string{}, form...),
				"--socket", socket, "--config", cfg,
				"--log-file", logFile, "--detach")
			cmd := exec.Command(bin, args...)
			cmd.Env = env
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("parent %v exited %v:\n%s", args, err, out)
			}
			t.Cleanup(func() {
				stop := exec.Command(bin, "daemon", "--socket", socket, "stop")
				stop.Env = env
				_ = stop.Run()
			})

			// The parent returning success is not enough — the whole point of
			// the bug is that the child died after the fork. Assert readiness.
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				if daemonAnswers(bin, env, socket) {
					return
				}
				time.Sleep(100 * time.Millisecond)
			}
			log, _ := os.ReadFile(logFile)
			t.Fatalf("child started via %v never reached readiness\nlog:\n%s", form, log)
		})
	}

	// The default-socket guard from the #149 tests: the detached children must
	// never have wandered off to the default socket.
	if _, err := os.Stat(filepath.Join(emptyDefault, "harness.sock")); err == nil {
		t.Errorf("detached child touched the DEFAULT socket")
	}
}

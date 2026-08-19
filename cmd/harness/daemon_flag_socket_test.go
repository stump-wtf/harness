package main

// Regression coverage for issue #149, added during review of PR #151.
//
// PR #151 makes `harness daemon --socket X` reach runDaemon, which fixes the
// reported symptom. It does not fix `stop` and `status`: main's dispatch hands
// those branches `verbOpts{socket: *socket, ...}` — the GLOBAL flag set's
// value — and silently discards the daemonArgs parseDaemonArgs just carefully
// extracted. So `harness daemon --socket X stop` connects to the DEFAULT
// socket instead of X.
//
// This is not cosmetic. The command reports success and names the PID it
// killed, so it reads as though it worked while having stopped an unrelated
// daemon. During review it stopped a live daemon on the reviewer's machine
// (SIGTERM to a running agent harness) while the scratch daemon it named
// stayed up. #149's acceptance criteria list `harness daemon --socket X stop`
// explicitly, and its gotchas call out the two-flag-sets trap this fell into.
//
// These tests exec the real binary, because the defect lives in main()'s
// dispatch and is invisible to any in-process test of parseDaemonArgs.
//
// NOTE: both tests point XDG_RUNTIME_DIR and XDG_STATE_HOME at a temp dir, so
// the "default socket" they fall back to is guaranteed empty. Without that
// isolation a failing run would reach for whatever real daemon the developer
// (or CI runner) has listening — which is precisely the hazard under test.

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// buildHarnessBinary compiles cmd/harness once for the exec-based tests.
func buildHarnessBinary(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(shortSockDir(t), "harness")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build harness: %v\n%s", err, out)
	}
	return bin
}

// isolatedEnv returns an env whose default-socket lookup lands in an empty
// temp dir, so a misrouted command cannot touch a real daemon.
func isolatedEnv(t *testing.T) (env []string, emptyDefaultDir string) {
	t.Helper()
	dir := shortSockDir(t)
	env = append(os.Environ(),
		"XDG_RUNTIME_DIR="+dir,
		"XDG_STATE_HOME="+dir,
	)
	return env, dir
}

// startDaemonOnSocket boots a daemon on an explicit socket and waits for it.
func startDaemonOnSocket(t *testing.T, bin string, env []string) (socket string, stop func()) {
	t.Helper()
	dir := shortSockDir(t)
	socket = filepath.Join(dir, "h.sock")
	cfg := filepath.Join(dir, "harness.toml")
	if err := os.WriteFile(cfg, []byte("[harness.demo]\nharness = \"generic\"\nargs = [\"-c\", \"sleep 600\"]\nenabled = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin, "daemon", "start", "--socket", socket, "--config", cfg)
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		t.Fatalf("start daemon: %v", err)
	}
	stop = func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
	t.Cleanup(stop)

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if daemonAnswers(bin, env, socket) {
			return socket, stop
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("daemon on %s never came up", socket)
	return "", stop
}

// daemonAnswers reports whether a daemon is reachable on socket.
func daemonAnswers(bin string, env []string, socket string) bool {
	cmd := exec.Command(bin, "--socket", socket, "list")
	cmd.Env = env
	return cmd.Run() == nil
}

// TestDaemonStopHonorsSocketBeforeSubcommand is the core assertion: a stop
// issued with --socket before the subcommand must stop THAT daemon.
func TestDaemonStopHonorsSocketBeforeSubcommand(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}
	bin := buildHarnessBinary(t)
	env, emptyDefault := isolatedEnv(t)
	socket, _ := startDaemonOnSocket(t, bin, env)

	cmd := exec.Command(bin, "daemon", "--socket", socket, "stop")
	cmd.Env = env
	out, err := cmd.CombinedOutput()

	// The default socket must never have been created — proving the command
	// did not wander off to some other daemon.
	if _, statErr := os.Stat(filepath.Join(emptyDefault, "harness.sock")); statErr == nil {
		t.Errorf("stop touched the DEFAULT socket instead of %s — on a real machine this stops an unrelated daemon", socket)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if !daemonAnswers(bin, env, socket) {
			return // stopped the right daemon
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Errorf("daemon on %s still answering after `daemon --socket %s stop` (err=%v)\noutput: %s",
		socket, socket, err, out)
}

// TestDaemonStatusHonorsSocketBeforeSubcommand covers the sibling branch:
// status reads *socket from the same global flag set and has the same defect,
// so it reports on the wrong daemon (or none) rather than the one named.
func TestDaemonStatusHonorsSocketBeforeSubcommand(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)
	socket, _ := startDaemonOnSocket(t, bin, env)

	cmd := exec.Command(bin, "daemon", "--socket", socket, "status")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("`daemon --socket %s status` failed, so it did not reach that daemon: %v\noutput: %s",
			socket, err, out)
	}
	if !containsSocket(string(out), socket) {
		t.Errorf("status did not report the daemon at %s; it read the default socket instead\noutput: %s", socket, out)
	}
}

func containsSocket(out, socket string) bool {
	return len(out) > 0 && (indexOf(out, socket) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

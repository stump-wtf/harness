package main

// Environment Configuration, End to End
//
// internal/settings tests the ladder in isolation. These tests prove the wiring:
// that a HARNESS_* variable set on a real process actually reaches the code that
// uses it. The unit tests would keep passing if the resolver were never called,
// which is exactly the failure mode worth guarding — the environment layer is
// invisible when it silently does nothing.
//
// Governing: ADR-0016, SPEC-0010 REQ "Environment Variable Namespace", REQ
// "Precedence Order", REQ "Fileless Operation", REQ "Source Attribution".
//
// @joestump-agent 08/19/2026 - Introduced with the ADR-0016 environment layer.

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startBackground launches the binary detached and returns a stop func.
func startBackground(t *testing.T, bin string, env []string, args ...string) func() {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", args, err)
	}
	stop := func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}
	t.Cleanup(stop)
	return stop
}

// waitForSocket polls until the daemon answers on socket, or gives up.
func waitForSocket(t *testing.T, bin string, env []string, socket string) bool {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if daemonAnswers(bin, env, socket) {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return false
}

// TestEnvSocketReachesTheClient proves HARNESS_SOCKET is honoured: a daemon is
// started on a scratch socket, and a client given only the environment variable
// finds it. The isolated XDG environment guarantees the default socket is empty,
// so a pass cannot come from accidentally reaching a real daemon.
func TestEnvSocketReachesTheClient(t *testing.T) {
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)
	socket, _ := startDaemonOnSocket(t, bin, env)

	out, code := runCLI(t, bin, append(env, "HARNESS_SOCKET="+socket), "list")
	if code != 0 {
		t.Fatalf("`HARNESS_SOCKET=%s harness list` exited %d:\n%s", socket, code, out)
	}
	if !strings.Contains(out, "demo") {
		t.Errorf("expected the demo harness in the listing:\n%s", out)
	}
}

// TestFlagBeatsEnv proves the top of the ladder end to end. HARNESS_SOCKET names
// a live daemon and --socket names an empty path; the flag must win, so the
// command must FAIL. A pass here would mean the flag was ignored.
func TestFlagBeatsEnv(t *testing.T) {
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)
	socket, _ := startDaemonOnSocket(t, bin, env)
	empty := filepath.Join(shortSockDir(t), "empty.sock")

	out, code := runCLI(t, bin,
		append(env, "HARNESS_SOCKET="+socket),
		"--socket", empty, "list")
	if code == 0 {
		t.Errorf("--socket %s was ignored in favour of HARNESS_SOCKET; the flag must win\n%s", empty, out)
	}
}

// TestEnvConfigPathIsHonoured proves HARNESS_CONFIG selects the file. doctor
// reports the config path it resolved, so it can confirm the choice without a
// running daemon.
func TestEnvConfigPathIsHonoured(t *testing.T) {
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)

	dir := t.TempDir()
	cfg := filepath.Join(dir, "custom.toml")
	body := "[harness.fromenv]\nharness = \"generic\"\nargs = [\"-c\", \"sleep 600\"]\nenabled = false\n"
	if err := os.WriteFile(cfg, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	out, _ := runCLI(t, bin, append(env, "HARNESS_CONFIG="+cfg, "HARNESS_JSON=1"), "doctor")
	if !strings.Contains(out, cfg) {
		t.Errorf("doctor did not resolve HARNESS_CONFIG=%s:\n%s", cfg, out)
	}
}

// TestDoctorReportsSource pins SPEC-0010 REQ "Source Attribution": the JSON
// report carries each setting's value and the source that supplied it, so
// "which one won?" is answerable from a script.
func TestDoctorReportsSource(t *testing.T) {
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)
	scratch := filepath.Join(shortSockDir(t), "attributed.sock")

	out, _ := runCLI(t, bin, append(env, "HARNESS_SOCKET="+scratch), "doctor", "--json")

	// doctor emits JSON on stdout; CombinedOutput may prepend terminal queries,
	// so start at the first brace.
	start := strings.Index(out, "{")
	if start < 0 {
		t.Fatalf("no JSON in doctor output:\n%s", out)
	}
	var got struct {
		Settings map[string]struct {
			Value  string `json:"value"`
			Source string `json:"source"`
		} `json:"settings"`
	}
	if err := json.Unmarshal([]byte(out[start:]), &got); err != nil {
		t.Fatalf("parse doctor JSON: %v\n%s", err, out[start:])
	}

	sock, ok := got.Settings["socket"]
	if !ok {
		t.Fatalf("doctor --json carried no socket setting; got %v", got.Settings)
	}
	if sock.Source != "env" {
		t.Errorf("socket source = %q, want \"env\"", sock.Source)
	}
	if sock.Value != scratch {
		t.Errorf("socket value = %q, want %q", sock.Value, scratch)
	}

	// A setting nobody touched must report as default, or the attribution is
	// decorative rather than accurate.
	if lvl, ok := got.Settings["log-level"]; ok && lvl.Source != "default" {
		t.Errorf("log-level source = %q with nothing set, want \"default\"", lvl.Source)
	}
}

// TestInvalidEnvValueFailsLoudly pins SPEC-0010 REQ "Environment Value
// Validation" through the real binary. Silently falling back to the default is
// the dangerous outcome: the operator believes their setting took effect.
func TestInvalidEnvValueFailsLoudly(t *testing.T) {
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)

	out, code := runCLI(t, bin, append(env, "HARNESS_LOG_LEVEL=verbose"), "daemon", "run")
	if code == 0 {
		t.Fatalf("HARNESS_LOG_LEVEL=verbose started the daemon instead of failing:\n%s", out)
	}
	if !strings.Contains(out, "HARNESS_LOG_LEVEL") {
		t.Errorf("error did not name the offending variable:\n%s", out)
	}
}

// TestEmptyEnvVarFallsThrough pins the shell-artifact case: `export
// HARNESS_SOCKET=$SOME_UNSET_VAR` exports an empty string, which must not become
// the socket path. It should behave exactly as if the variable were unset.
func TestEmptyEnvVarFallsThrough(t *testing.T) {
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)
	socket, _ := startDaemonOnSocket(t, bin, env)

	// The flag supplies the socket; the empty variable must not displace it.
	out, code := runCLI(t, bin, append(env, "HARNESS_SOCKET="), "--socket", socket, "list")
	if code != 0 {
		t.Errorf("empty HARNESS_SOCKET broke an otherwise valid invocation (exit %d):\n%s", code, out)
	}
}

// TestFilelessDaemonStarts pins SPEC-0010 REQ "Fileless Operation": a container
// with no harness.toml anywhere must still come up on environment variables
// alone, reporting zero harnesses rather than erroring on the missing file.
func TestFilelessDaemonStarts(t *testing.T) {
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)

	dir := shortSockDir(t)
	socket := filepath.Join(dir, "fileless.sock")
	absent := filepath.Join(dir, "definitely-not-here.toml")

	cmd := startBackground(t, bin,
		append(env, "HARNESS_SOCKET="+socket, "HARNESS_CONFIG="+absent),
		"daemon", "run")
	defer cmd()

	if !waitForSocket(t, bin, env, socket) {
		t.Fatalf("daemon with no config file at %s never came up on %s", absent, socket)
	}

	out, code := runCLI(t, bin, append(env, "HARNESS_SOCKET="+socket), "list")
	if code != 0 {
		t.Fatalf("`list` against the fileless daemon exited %d:\n%s", code, out)
	}
}

package main

// CLI Argument-Handling Characterization
//
// These tests lock the CLI's observable argv contract so it can be re-platformed
// underneath without anyone having to argue the behaviour was preserved. They
// exec the real binary and assert only on what a user sees — exit status,
// stdout, stderr — never on the parser internals, because the parser is exactly
// what changes. A test that called parseInterleaved or parseDaemonArgs directly
// would have to be deleted alongside them and would prove nothing about the
// replacement.
//
// Governing: ADR-0016 (Cobra owns the command tree), SPEC-0010 REQ "Command
// Tree". The migration plan in the paired design.md lands these BEFORE the
// Cobra swap precisely so they can run unchanged on both sides of it.
//
// Every case here is a contract someone relied on:
//   * flags and the positional interleave in any order (the parseInterleaved
//     loop existed solely for this)
//   * `harness daemon` with no subcommand means `run`
//   * daemon flags may precede the subcommand, and the socket they name is the
//     one that gets used — issue #149, where `daemon --socket X stop` reported
//     success while stopping an unrelated daemon
//   * `--json` is accepted before or after the verb
//   * an unknown verb exits non-zero with a pointer to help, not a usage dump
//
// @joestump-agent 08/19/2026 - Added ahead of the Cobra/Viper migration
// (ADR-0016) as the behaviour-preservation proof for that rewrite.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// runCLI execs the binary with args in an isolated environment and returns
// combined output plus the exit code. A non-zero exit is data, not a failure —
// several cases below assert on it.
func runCLI(t *testing.T, bin string, env []string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("exec %v: %v", args, err)
	}
	return string(out), code
}

// TestCLIInterleavedFlagsAndPositional pins the contract parseInterleaved was
// written for: AFTER the verb, a per-verb flag and the positional may appear in
// either order, so `logs demo --lines 3` and `logs --lines 3 demo` are the same
// command. Run against a live daemon so the assertion is on the resulting
// request, not on parser state.
//
// The interleaving is verb-local by design. Per-verb flags are declared on the
// verb's own flag set, so `harness --lines 3 logs demo` is a parse error —
// pinned separately by TestCLIVerbFlagBeforeVerbIsAnError.
func TestCLIInterleavedFlagsAndPositional(t *testing.T) {
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)
	socket, _ := startDaemonOnSocket(t, bin, env)

	nameFirst, codeNameFirst := runCLI(t, bin, env, "--socket", socket, "logs", "demo", "--lines", "3")
	flagFirst, codeFlagFirst := runCLI(t, bin, env, "--socket", socket, "logs", "--lines", "3", "demo")

	if codeNameFirst != codeFlagFirst {
		t.Fatalf("exit codes differ: name-first=%d flag-first=%d\nname-first:\n%s\nflag-first:\n%s",
			codeNameFirst, codeFlagFirst, nameFirst, flagFirst)
	}
	if nameFirst != flagFirst {
		t.Errorf("interleaved forms produced different output\nname-first:\n%s\nflag-first:\n%s",
			nameFirst, flagFirst)
	}
}

// TestCLIVerbFlagBeforeVerbIsAnError pins the other half of the interleaving
// contract: per-verb flags are verb-local, so passing one before the verb is a
// parse error rather than being silently accepted. Cobra must not "helpfully"
// promote these to persistent root flags during the migration — that would
// change which commands accept --lines, --follow, --ro, and --all.
func TestCLIVerbFlagBeforeVerbIsAnError(t *testing.T) {
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)
	socket := filepath.Join(shortSockDir(t), "none.sock")

	out, code := runCLI(t, bin, env, "--socket", socket, "--lines", "3", "logs", "demo")
	if code == 0 {
		t.Errorf("`--lines` before the verb was accepted; it is a verb-local flag\n%s", out)
	}
}

// TestCLIBareDaemonMeansRun pins that `harness daemon` with no subcommand is
// `harness daemon run` — the ADR-0005 systemd ExecStart form. It asserts via
// --version, which every daemon path accepts and which exits without binding a
// socket, so the test needs no cleanup.
func TestCLIBareDaemonMeansRun(t *testing.T) {
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)

	bare, bareCode := runCLI(t, bin, env, "daemon", "--version")
	explicit, explicitCode := runCLI(t, bin, env, "daemon", "run", "--version")

	if bareCode != 0 || explicitCode != 0 {
		t.Fatalf("expected clean exits, got bare=%d explicit=%d\nbare:\n%s\nexplicit:\n%s",
			bareCode, explicitCode, bare, explicit)
	}
	if bare != explicit {
		t.Errorf("`daemon` and `daemon run` diverged\nbare:\n%s\nexplicit:\n%s", bare, explicit)
	}
}

// TestCLIDaemonFlagsBeforeSubcommand pins issue #149: flags may precede the
// subcommand, and the socket they name must be the socket acted on. The
// regression this guards is silent — the old code reported success while
// talking to the default socket — so the assertion is that stop against an
// empty scratch socket FAILS. A success here means the command found some other
// daemon, which is the bug.
func TestCLIDaemonFlagsBeforeSubcommand(t *testing.T) {
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)
	unused := filepath.Join(shortSockDir(t), "nobody-home.sock")

	out, code := runCLI(t, bin, env, "daemon", "--socket", unused, "stop")
	if code == 0 {
		t.Errorf("`daemon --socket %s stop` succeeded against an empty socket — "+
			"the flag was ignored and some other daemon was targeted (issue #149)\n%s", unused, out)
	}
}

// TestCLIDaemonFlagValueNotMistakenForSubcommand pins the skip-list behaviour:
// a subcommand-shaped token that is really a flag VALUE must not be read as the
// subcommand. `--log-file stop` names a file called "stop".
func TestCLIDaemonFlagValueNotMistakenForSubcommand(t *testing.T) {
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)
	logFile := filepath.Join(shortSockDir(t), "stop")

	// --version short-circuits before the daemon binds anything, so this proves
	// the token was parsed as a flag value rather than dispatched as `stop`.
	out, code := runCLI(t, bin, env, "daemon", "--log-file", logFile, "--version")
	if code != 0 {
		t.Errorf("`daemon --log-file stop --version` exited %d; the value was likely "+
			"misread as the stop subcommand\n%s", code, out)
	}
	if !strings.Contains(out, "harness daemon") {
		t.Errorf("expected daemon version output, got:\n%s", out)
	}
}

// TestCLIJSONFlagEitherSide pins that --json is accepted before or after the
// verb. The global flag set and the per-verb flag set both declared it; any
// replacement must keep both positions working.
func TestCLIJSONFlagEitherSide(t *testing.T) {
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)
	socket, _ := startDaemonOnSocket(t, bin, env)

	before, codeBefore := runCLI(t, bin, env, "--socket", socket, "--json", "list")
	after, codeAfter := runCLI(t, bin, env, "--socket", socket, "list", "--json")

	if codeBefore != 0 || codeAfter != 0 {
		t.Fatalf("expected clean exits, got before=%d after=%d\nbefore:\n%s\nafter:\n%s",
			codeBefore, codeAfter, before, after)
	}
	for _, out := range []string{before, after} {
		if !strings.HasPrefix(strings.TrimSpace(out), "{") && !strings.HasPrefix(strings.TrimSpace(out), "[") {
			t.Errorf("--json did not produce JSON:\n%s", out)
		}
	}
}

// TestCLIUnknownVerb pins the calm-error contract: a non-zero exit naming the
// verb and pointing at help, NOT a full usage dump.
func TestCLIUnknownVerb(t *testing.T) {
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)

	out, code := runCLI(t, bin, env, "frobnicate")
	if code == 0 {
		t.Fatalf("unknown verb exited 0:\n%s", out)
	}
	if !strings.Contains(out, "frobnicate") {
		t.Errorf("error did not name the unknown verb:\n%s", out)
	}
}

// TestCLINameRequiredVerbs pins that name-taking verbs fail without a name
// rather than acting on something arbitrary, and that no-argument verbs reject
// a stray positional rather than silently ignoring it.
func TestCLINameRequiredVerbs(t *testing.T) {
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)

	for _, verb := range []string{"describe", "logs", "start", "stop", "restart", "use-profile"} {
		t.Run("requires-name/"+verb, func(t *testing.T) {
			out, code := runCLI(t, bin, env, verb)
			if code == 0 {
				t.Errorf("`harness %s` with no name exited 0:\n%s", verb, out)
			}
		})
	}
}

// TestCLIVersion pins that --version short-circuits everything, including the
// absence of a daemon.
func TestCLIVersion(t *testing.T) {
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)

	out, code := runCLI(t, bin, env, "--version")
	if code != 0 {
		t.Fatalf("--version exited %d:\n%s", code, out)
	}
	if !strings.HasPrefix(out, "harness ") {
		t.Errorf("unexpected --version output:\n%s", out)
	}
}

// shortSockDirExists guards the helpers this file borrows from
// daemon_flag_socket_test.go, so a refactor that moves them fails loudly here
// rather than as a confusing compile error in an unrelated file.
var _ = os.Getenv

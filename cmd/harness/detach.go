package main

// Governing: ADR-0005 (the daemon is normally supervised by init/systemd;
// `--detach` is a dev convenience that forks into the background with stdio
// redirected to a logfile). Implementation follows the classic Unix daemon
// pattern: fork → setsid → fork, with a readiness pipe so the parent only
// exits once the child has either bound the socket or fatally errored.

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"gitea.stump.rocks/stump.wtf/harness/internal/cliui"
)

// defaultDetachLog is where --detach sends daemon output when the caller did
// not pass --log-file. It lives under $XDG_STATE_HOME/harness/ alongside the
// supervisor's state, matching protocol.DefaultSocketPath's fallback.
const defaultDetachLog = "harness-daemon.log"

// detachDaemon re-execs the daemon without --detach, with stdio redirected
// to a logfile, and waits for the child to signal readiness (it has bound
// the socket) or fatal error before the parent returns. On success the
// parent prints a short "started" line and returns; the child keeps running
// detached in a new session.
func detachDaemon(args []string) error {
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("detach: resolve executable: %w", err)
	}

	// Build the child argv: drop --detach, force --log-file if absent, keep
	// everything else the caller passed. We must re-prepend the `daemon` verb
	// token because runDaemon received args *after* `daemon` was peeled off
	// by main.go's dispatch.
	childArgs := stripDetach(args)
	childArgs = append([]string{"daemon"}, childArgs...)
	logFile := findFlag(args, "--log-file")
	if logFile == "" {
		logFile = defaultLogPath()
	}
	childArgs = ensureFlag(childArgs, "--log-file", logFile)

	// Open the logfile the child will inherit. We pass the *fd* via
	// ExtraFiles so the child's charmbracelet/log can write to it via the
	// --log-file path we already added to argv.
	lf, err := os.OpenFile(logFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("detach: open log file %s: %w", logFile, err)
	}

	// Readiness pipe: child writes one byte ('ok' or 'e') once it knows.
	rdyR, rdyW, err := os.Pipe()
	if err != nil {
		lf.Close()
		return fmt.Errorf("detach: pipe: %w", err)
	}

	cmd := exec.Command(exe, childArgs...)
	cmd.Stdin = nil
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.ExtraFiles = []*os.File{rdyW} // child sees this as fd 3
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		lf.Close()
		rdyR.Close()
		rdyW.Close()
		return fmt.Errorf("detach: start child: %w", err)
	}

	// Close the write end in the parent so a child exit also unblocks Read.
	rdyW.Close()
	defer rdyR.Close()
	defer lf.Close()

	// Wait for readiness signal (one byte) or child exit, whichever first.
	buf := make([]byte, 1)
	n, _ := rdyR.Read(buf)
	if n == 0 {
		// Child exited before signaling. Surface its exit status.
		err := cmd.Wait()
		return fmt.Errorf("detach: child exited before readiness: %w", err)
	}
	if buf[0] == 'e' {
		_ = cmd.Wait()
		return fmt.Errorf("detach: child reported error (see %s)", logFile)
	}

	// 'ok': release the child so it keeps running after we exit.
	_ = cmd.Process.Release()

	cliui.Default.Report(cliui.LevelSuccess, "daemon started",
		fmt.Sprintf("pid %d, logging to %s", cmd.Process.Pid, logFile),
		fmt.Sprintf("connect with: harness --socket %s list", detachSocketPath(childArgs)))
	return nil
}

// stripDetach returns args with any --detach / --detach=true removed.
func stripDetach(args []string) []string {
	out := make([]string, 0, len(args))
	for _, a := range args {
		if a == "--detach" || a == "--detach=true" || a == "--detach=false" {
			continue
		}
		if strings.HasPrefix(a, "--detach=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// findFlag returns the value of --flag in args, or "" if absent.
func findFlag(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
		if strings.HasPrefix(a, flag+"=") {
			return strings.TrimPrefix(a, flag+"=")
		}
	}
	return ""
}

// ensureFlag appends flag+value to args if flag isn't already present (in
// either --flag X or --flag=X form).
func ensureFlag(args []string, flag, value string) []string {
	if findFlag(args, flag) != "" {
		return args
	}
	return append(args, flag, value)
}

// detachSocketPath pulls the --socket value the caller passed (or the default)
// for the "connect with" hint.
func detachSocketPath(args []string) string {
	if s := findFlag(args, "--socket"); s != "" {
		return s
	}
	return "default"
}

// defaultLogPath resolves $XDG_STATE_HOME/harness/harness-daemon.log, falling
// back to ~/.local/state/harness/harness-daemon.log.
func defaultLogPath() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return defaultDetachLog
		}
		base = home + "/.local/state"
	}
	return base + "/harness/" + defaultDetachLog
}

// signalDetached writes one byte to the fd-3 readiness pipe that detachDaemon
// set up, if any. When not running detached, fd 3 is not a pipe and the write
// is silently ignored (best-effort: we don't care about the error). The byte
// is 'o' on readiness, 'e' on fatal error.
func signalDetached(b byte) {
	f := os.NewFile(3, "ready")
	if f != nil {
		_, _ = f.Write([]byte{b})
		_ = f.Close()
	}
}

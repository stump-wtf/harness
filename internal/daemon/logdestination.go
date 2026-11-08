package daemon

import (
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Where a daemon's own log lines land, spelled the way `doctor` and
// `daemon status` print them (stump.wtf/harness#583).
const (
	// LogDestinationJournal is the service manager's log: the daemon wrote to
	// stderr and journald (or an equivalent) collected it. On systemd that is
	// `journalctl --user -u harness`; under launchd, the plist's
	// StandardOutPath file.
	LogDestinationJournal = "journal (stderr)"
	// LogDestinationStderr is a foreground daemon whose stderr the caller
	// still holds — the terminal, or a redirect only the caller can name.
	LogDestinationStderr = "stderr"
)

// LogDestination answers, for the process calling it, which surface its own
// log lines land in. It is the answer `daemon_info` carries, so that
// `harness doctor` and `harness daemon status` report the running daemon's
// log rather than a path someone once pointed `--detach` at
// (stump.wtf/harness#583; ADR-0001 keeps the destination itself as stderr and
// lets the service manager own it — only discoverability changes here).
//
// logFile is the path the logger actually opened, empty when it ended up on
// stderr; a path that failed to open must arrive empty, because the daemon
// really is logging to stderr then. getenv and stderr let a test drive the
// service-manager branch without a service manager: nil means the real
// environment and os.Stderr.
func LogDestination(logFile string, getenv func(string) string, stderr io.Writer) string {
	if abs := absPath(logFile); abs != "" {
		return abs
	}
	if stderrIsJournal(getenv, stderr) {
		return LogDestinationJournal
	}
	return LogDestinationStderr
}

// stderrIsJournal reports whether a service manager owns the daemon's stderr,
// which is the case the stale detach log made confusing: the file named in
// the help text exists, and the live daemon's lines are in the journal
// instead.
//
// systemd sets JOURNAL_STREAM — the device:inode of the journal stream a
// unit's stdout or stderr is wired to — exactly when those streams connect
// to the journal, so it is the one marker that names this case.
// INVOCATION_ID is set for every unit run, including a `StandardOutput=file:`
// unit whose stderr the unit itself redirected into a plain file, so keying
// on it would call those lines "journal" when journald holds none of them.
// launchd marks nothing, and it hands
// the process an already-redirected stderr, which reads as an ordinary file
// and stays spelled `stderr` here — the plist names that file, not us.
//
// A terminal stderr wins either way: an interactive shell inside a systemd
// login session inherits the unit's environment without the journal holding
// this process's lines.
func stderrIsJournal(getenv func(string) string, stderr io.Writer) bool {
	if getenv == nil {
		getenv = os.Getenv
	}
	if getenv("JOURNAL_STREAM") == "" {
		return false
	}
	return !isCharDevice(stderr)
}

// statter is what *os.File satisfies; a test's writer that is not backed by a
// file simply does not, and counts as "not a terminal".
type statter interface {
	Stat() (fs.FileInfo, error)
}

// isCharDevice reports whether w is a terminal or another character device.
// A stat failure is not evidence of a terminal: a daemon that cannot describe
// its own stderr gets the answer the environment gives, not a guess.
func isCharDevice(w io.Writer) bool {
	st, ok := w.(statter)
	if !ok {
		return false
	}
	fi, err := st.Stat()
	if err != nil {
		return false
	}
	return fi.Mode()&fs.ModeCharDevice != 0
}

// absPath names logFile the way a person would follow it: absolute, so
// `doctor` prints something they can hand to tail. An unresolvable path is
// returned unchanged rather than dropped — naming it imperfectly beats
// claiming the daemon logs to stderr.
func absPath(logFile string) string {
	if logFile == "" {
		return ""
	}
	if abs, err := filepath.Abs(logFile); err == nil {
		return abs
	}
	return logFile
}

package daemon

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeStderr stands in for whatever the daemon's stderr really is, so the
// service-manager branch is testable without a journal or a terminal.
type fakeStderr struct {
	mode fs.FileMode
	err  error
}

func (fakeStderr) Write(p []byte) (int, error) { return len(p), nil }

func (f fakeStderr) Stat() (fs.FileInfo, error) {
	if f.err != nil {
		return nil, f.err
	}
	return fakeStderrInfo{mode: f.mode}, nil
}

type fakeStderrInfo struct{ mode fs.FileMode }

func (i fakeStderrInfo) Name() string       { return "stderr" }
func (i fakeStderrInfo) Size() int64        { return 0 }
func (i fakeStderrInfo) Mode() fs.FileMode  { return i.mode }
func (i fakeStderrInfo) ModTime() time.Time { return time.Time{} }
func (i fakeStderrInfo) IsDir() bool        { return false }
func (i fakeStderrInfo) Sys() any           { return nil }

// fixedEnv serves a closed set of variables, so a test says exactly what the
// process's environment looked like instead of inheriting the runner's.
func fixedEnv(kv map[string]string) func(string) string {
	return func(k string) string { return kv[k] }
}

var (
	redirected = fakeStderr{mode: fs.ModeNamedPipe}                  // a redirected stderr
	terminal   = fakeStderr{mode: fs.ModeDevice | fs.ModeCharDevice} // a terminal
	brokenStat = fakeStderr{err: errors.New("fstat: bad fd")}
)

// TestLogDestinationNamesTheRunningSurface is stump.wtf/harness#583: the
// answer must come from how this process was started, not from a path config
// happens to name or a file someone left behind with --detach.
func TestLogDestinationNamesTheRunningSurface(t *testing.T) {
	rel := filepath.Join("state", "daemon.log")
	abs, err := filepath.Abs(rel)
	if err != nil {
		t.Fatal(err)
	}
	systemd := map[string]string{"INVOCATION_ID": "a1b2c3"}
	journalStream := map[string]string{"JOURNAL_STREAM": "8:1963214"}

	cases := []struct {
		name    string
		logFile string
		env     map[string]string
		stderr  fs.FileMode
		want    string
	}{
		{name: "log file", logFile: "/var/log/harness/daemon.log", want: "/var/log/harness/daemon.log"},
		{name: "relative log file is named where it can be followed", logFile: rel, want: abs},
		{name: "service manager owns stderr", env: systemd, want: LogDestinationJournal},
		{name: "journal stream device recorded", env: journalStream, want: LogDestinationJournal},
		{name: "foreground daemon", want: LogDestinationStderr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := LogDestination(tc.logFile, fixedEnv(tc.env), redirected); got != tc.want {
				t.Errorf("LogDestination(%q) = %q, want %q", tc.logFile, got, tc.want)
			}
		})
	}

	// A terminal wins over the environment: a shell inside a systemd login
	// session carries INVOCATION_ID without the journal holding this process's
	// lines, and pointing an operator at journalctl for those lines would be
	// the same wrong answer this issue is about.
	if got := LogDestination("", fixedEnv(systemd), terminal); got != LogDestinationStderr {
		t.Errorf("terminal stderr under systemd = %q, want %q", got, LogDestinationStderr)
	}
	// A stderr that cannot be described is not evidence of a terminal either:
	// the environment still gets the say.
	if got := LogDestination("", fixedEnv(systemd), brokenStat); got != LogDestinationJournal {
		t.Errorf("unstatable stderr under systemd = %q, want %q", got, LogDestinationJournal)
	}
	// os.Stderr is what production passes; it must satisfy the stat seam the
	// detection relies on, or every real daemon silently loses the journal
	// branch.
	if os.Stderr == nil {
		t.Fatal("os.Stderr is nil")
	}
	if _, ok := any(os.Stderr).(interface{ Stat() (fs.FileInfo, error) }); !ok {
		t.Error("os.Stderr does not satisfy the stat seam the journal detection needs")
	}
}

// TestLogDestinationWithoutAServiceManager pins the launchd-shaped case: no
// marker variables at all, so the answer is stderr, which is what the daemon
// truly writes even though the plist sends it to a file.
func TestLogDestinationWithoutAServiceManager(t *testing.T) {
	if got := LogDestination("", fixedEnv(map[string]string{}), redirected); got != LogDestinationStderr {
		t.Errorf("LogDestination with no marker env = %q, want %q", got, LogDestinationStderr)
	}
}

// TestDaemonInfoReportsLogDestination is the half of stump.wtf/harness#583 a
// fixture cannot fake: the answer arrives over the control socket from the
// running server, so a month-old harness-daemon.log on disk cannot outvote
// the process a person is actually asking about.
func TestDaemonInfoReportsLogDestination(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	td := newTestDaemon(t, sleeperTOML, func(o *Options) { o.LogFile = path })
	c := td.dial(t, nil)

	di, err := c.DaemonInfo()
	if err != nil {
		t.Fatalf("DaemonInfo: %v", err)
	}
	if di.LogDestination != path {
		t.Errorf("daemon_info log_destination = %q, want the opened file %q", di.LogDestination, path)
	}
}

// TestDaemonInfoAlwaysNamesASurface: a daemon nobody handed a log file still
// answers, so a client never has to read an empty field as "logs nowhere".
func TestDaemonInfoAlwaysNamesASurface(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML)
	c := td.dial(t, nil)

	di, err := c.DaemonInfo()
	if err != nil {
		t.Fatalf("DaemonInfo: %v", err)
	}
	if di.LogDestination != LogDestinationJournal && di.LogDestination != LogDestinationStderr {
		t.Errorf("daemon_info log_destination = %q, want %q or %q",
			di.LogDestination, LogDestinationJournal, LogDestinationStderr)
	}
}

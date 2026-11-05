package main

// Where a daemon logs has to be answerable from the running process. The file
// the --detach help names is the leftover of some earlier detached run on most
// boxes, so an operator who tails it concludes the wedged daemon said nothing,
// when the live lines are in the journal (stump.wtf/harness#583).
//
// The resolver itself lives in internal/daemon and is tested there. These
// cover the two things only this package does: report what the logger actually
// opened (a --log-file that would not open leaves the lines on stderr, and the
// daemon must not point at a file no line ever reaches), and print the answer
// the daemon gave.
//
// Governing: ADR-0001 (the daemon logs to stderr and lets the service manager
// own the surface); SPEC-0002 REQ "Control Operations" (daemon_info).

import (
	"bytes"
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/log"

	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/daemon"
	"github.com/stump-wtf/harness/internal/protocol"
)

// TestConfigureDaemonLoggerReportsWhatItOpened pins the wiring that only
// exists here: the daemon reports the surface its logger ended up on, not the
// path it was asked for. Every daemon_info test constructs the answer, so
// none of them would notice this return value being dropped on the floor.
func TestConfigureDaemonLoggerReportsWhatItOpened(t *testing.T) {
	prev := log.Default()
	t.Cleanup(func() { log.SetDefault(prev) })

	tmp := t.TempDir()
	path := filepath.Join(tmp, "daemon.log")
	if got := configureDaemonLogger("info", path); got != path {
		t.Errorf("configureDaemonLogger opened %q and reported %q, want the path it opened", path, got)
	}

	blocker := filepath.Join(tmp, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	unopenable := filepath.Join(blocker, "daemon.log")
	if got := configureDaemonLogger("info", unopenable); got != "" {
		t.Errorf("configureDaemonLogger fell back to stderr but reported %q, want \"\" so the CLI names stderr", got)
	}
}

func TestDaemonLogCheckNamesTheSurfaceOrSaysItIsUnknown(t *testing.T) {
	cases := []struct {
		name       string
		di         *protocol.DaemonInfo
		wantLevel  cliui.Level
		wantDetail string
		wantHint   bool
	}{
		{
			name:       "log file",
			di:         &protocol.DaemonInfo{LogDestination: "/var/log/harness/daemon.log"},
			wantLevel:  cliui.LevelSuccess,
			wantDetail: "/var/log/harness/daemon.log",
		},
		{
			name:       "journal",
			di:         &protocol.DaemonInfo{LogDestination: daemon.LogDestinationJournal},
			wantLevel:  cliui.LevelSuccess,
			wantDetail: daemon.LogDestinationJournal,
		},
		{
			// An older daemon answers nothing. Guessing from the filesystem
			// is the failure this issue is about, so the row says it is
			// unknown and how to get an answer.
			name:       "daemon predates the field",
			di:         &protocol.DaemonInfo{},
			wantLevel:  cliui.LevelWarn,
			wantDetail: "the running daemon did not say",
			wantHint:   true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			row := daemonLogCheck(tc.di)
			if row.name != "daemon_log" {
				t.Errorf("row name = %q, want daemon_log", row.name)
			}
			if row.level != tc.wantLevel {
				t.Errorf("level = %v, want %v", row.level, tc.wantLevel)
			}
			if row.detail != tc.wantDetail {
				t.Errorf("detail = %q, want %q", row.detail, tc.wantDetail)
			}
			if got := row.hint != ""; got != tc.wantHint {
				t.Errorf("hint present = %v (%q), want %v", got, row.hint, tc.wantHint)
			}
		})
	}
}

// TestDaemonLogCheckNamesTheSurfaceOrSaysItIsUnknown above drives the row
// builder directly; this one pins the wiring around it (the #315 lesson: a
// constructed argument never exercises a call site). With no daemon to ask,
// doctor --json carries no daemon_log row at all — the version row already
// says why — while a reachable daemon that predates the field still gets one.
func TestDoctorSkipsTheDaemonLogRowWhenTheDaemonIsUnreachable(t *testing.T) {
	cfgPath, _ := laxWebhookConfig(t, t.TempDir(), 0o600, "")
	rows := doctorRows(t, cfgPath)
	if r, ok := rows["daemon_log"]; ok {
		t.Errorf("doctor --json carries a daemon_log row with no daemon to ask: %+v", r)
	}
	r, ok := rows["daemon"]
	if !ok || !strings.Contains(r.Detail, "unreachable") {
		t.Errorf("daemon row = %+v, want the unreachable-daemon complaint", r)
	}
}

// TestDoctorSkipsDaemonLogRowWhenDaemonInfoUnavailable pins the path the
// guard above actually changed: a daemon that answers HELLO but hangs up
// before daemon_info. The unreachable-daemon path never reaches the
// daemon_log row (doctor returns after the daemon row), so only this path
// can tell the skip from the old duplicate-warning behavior — without it,
// removing the diOK guard changes nothing any test notices.
func TestDoctorSkipsDaemonLogRowWhenDaemonInfoUnavailable(t *testing.T) {
	cliui.SetJSON(true)
	t.Cleanup(func() { cliui.SetJSON(false) })

	cfgPath := filepath.Join(t.TempDir(), "harness.toml")
	if err := writeMinimalConfig(cfgPath); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(t.TempDir(), "hello-only.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		pc := protocol.NewConn(conn)
		f, err := pc.ReadFrame()
		if err != nil || f.Type != protocol.TypeHello {
			return
		}
		_ = pc.WriteJSON(protocol.TypeHello, &protocol.Hello{
			ProtoVersion:  protocol.ProtoVersion,
			DaemonVersion: "hello-only",
		})
		// Hang up: daemon_info and everything after it fails, while the
		// handshake succeeded — exactly the state the guard decides on.
	}()

	var code int
	stdout, _ := captureStd(t, func() {
		code = runDoctorWith(verbOpts{configPath: cfgPath, socket: sock, json: true}, false)
	})
	if code != 0 && code != 1 {
		t.Fatalf("runDoctorWith = %d, want 0 or 1", code)
	}

	var res doctorResult
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatalf("invalid doctor --json: %v\nraw: %s", err, stdout)
	}
	if res.DaemonLog != nil {
		t.Errorf("daemon_log row emitted for a daemon that answered no daemon_info: %+v\n%s", res.DaemonLog, stdout)
	}
	if res.Version == nil || res.Version.Status != "warn" || !strings.Contains(res.Version.Detail, "couldn't fetch daemon info") {
		t.Errorf("version row = %+v, want the warn that names the unanswered daemon_info\n%s", res.Version, stdout)
	}
}

// TestDaemonStatusNamesItsOwnLogFile runs the real binary: a daemon started
// with --log-file must answer with that path, through daemon_info and in the
// table a person reads.
func TestDaemonStatusNamesItsOwnLogFile(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}
	bin := buildHarnessBinary(t)
	env, _ := isolatedEnv(t)
	logPath := filepath.Join(t.TempDir(), "daemon.log")

	dir := shortSockDir(t)
	socket := filepath.Join(dir, "h.sock")
	cfg := filepath.Join(dir, "harness.toml")
	if err := os.WriteFile(cfg, []byte("[harness.demo]\nharness = \"generic\"\nargs = [\"-c\", \"sleep 600\"]\nenabled = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := startDaemonProc(t, bin, env, "daemon", "start", "--socket", socket, "--config", cfg, "--log-file", logPath)
	d.waitReady(t, bin, env, socket)

	if got := daemonStatusInfo(t, bin, env, socket).LogDestination; got != logPath {
		t.Errorf("daemon_info log_destination = %q, want the file it was started with (%q)", got, logPath)
	}
	if out := daemonStatus(t, bin, env, socket); !strings.Contains(string(out), logPath) {
		t.Errorf("`daemon status` never named %q, so the path is only in --json:\n%s", logPath, out)
	}
}

// TestDaemonStatusNamesTheJournalIsTheCaseThatCostTheHour: a service-run
// daemon writes to stderr and the journal collects it, while
// $XDG_STATE_HOME/harness/harness-daemon.log sits there from an old detached
// run. The daemon's own answer, not the filesystem, is what settles it.
func TestDaemonStatusNamesTheJournal(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary; skipped under -short")
	}
	bin := buildHarnessBinary(t)
	env, stateHome := isolatedEnv(t)
	// Mark the child as service-started and clear anything the runner itself
	// inherited, so the answer comes from this test's environment.
	env = append(env, "INVOCATION_ID=handoff-m-test", "JOURNAL_STREAM=")

	dir := shortSockDir(t)
	socket := filepath.Join(dir, "h.sock")
	cfg := filepath.Join(dir, "harness.toml")
	if err := os.WriteFile(cfg, []byte("[harness.demo]\nharness = \"generic\"\nargs = [\"-c\", \"sleep 600\"]\nenabled = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	d := startDaemonProc(t, bin, env, "daemon", "start", "--socket", socket, "--config", cfg)
	d.waitReady(t, bin, env, socket)

	// The stale detach log the issue is about: create it, in the state dir the
	// daemon would use, so a guess from the filesystem would be caught out.
	stale := filepath.Join(stateHome, "harness", "harness-daemon.log")
	if err := os.MkdirAll(filepath.Dir(stale), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(stale, []byte("2026-08-18T20:40:21 INFO shutting down\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if got := daemonStatusInfo(t, bin, env, socket).LogDestination; got != daemon.LogDestinationJournal {
		t.Errorf("daemon_info log_destination = %q, want %q while %s exists", got, daemon.LogDestinationJournal, stale)
	}
}

// daemonStatus runs `harness daemon --socket S status` and returns its output.
func daemonStatus(t *testing.T, bin string, env []string, socket string, args ...string) []byte {
	t.Helper()
	cmd := exec.Command(bin, append([]string{"daemon", "--socket", socket, "status"}, args...)...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("`daemon --socket %s status %v`: %v\n%s", socket, args, err, out)
	}
	return out
}

func daemonStatusInfo(t *testing.T, bin string, env []string, socket string) protocol.DaemonInfo {
	t.Helper()
	out := daemonStatus(t, bin, env, socket, "--json")
	var di protocol.DaemonInfo
	if err := json.Unmarshal(out, &di); err != nil {
		t.Fatalf("decode `daemon status --json`: %v\n%s", err, out)
	}
	return di
}

// TestEmitDoctorJSONCarriesTheDaemonLogRow guards the shape bug this repo has
// been bitten by before: a row counted in the summary with no object beside it,
// so `--json` reports a warning a script cannot point at.
func TestEmitDoctorJSONCarriesTheDaemonLogRow(t *testing.T) {
	rows := []check{
		{name: "config", level: cliui.LevelSuccess, detail: "ok"},
		daemonLogCheck(&protocol.DaemonInfo{LogDestination: daemon.LogDestinationJournal}),
	}
	var buf bytes.Buffer
	emitDoctorJSON(&buf, rows, nil)

	var res doctorResult
	if err := json.Unmarshal(buf.Bytes(), &res); err != nil {
		t.Fatalf("invalid JSON: %v\nraw: %s", err, buf.String())
	}
	if res.DaemonLog == nil {
		t.Fatalf("doctor --json has no daemon_log object, so a script cannot read the answer:\n%s", buf.String())
	}
	if res.DaemonLog.Status != "ok" || res.DaemonLog.Detail != daemon.LogDestinationJournal {
		t.Errorf("daemon_log = %+v, want ok with detail %q", res.DaemonLog, daemon.LogDestinationJournal)
	}
}

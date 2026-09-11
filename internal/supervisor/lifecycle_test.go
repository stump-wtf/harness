package supervisor

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLifecycleRoundTrip writes lifecycle events through the logger the
// supervisor really uses, interleaved with program output, and reads them back.
// The format is charmbracelet/log's; pinning it through the writer rather than
// a hand-typed fixture is what catches an upstream format change.
func TestLifecycleRoundTrip(t *testing.T) {
	dir := t.TempDir()
	rl, err := newRotatingLog("sweep", LogConfig{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	ev := newEventLogger(rl)
	ev.Info("state changed", "from", "stopped", "to", "starting")
	ev.Info("state changed", "from", "starting", "to", "running")
	fmt.Fprintln(rl, "Now reading pdx.yaml to get the enabled services for nuc01.")
	// A program can print something shaped like a lifecycle line; with trailing
	// text it is not one.
	fmt.Fprintln(rl, "2026/09/11 07:40:00 INFO exited code=0 — quoted from a runbook")
	ev.Info("exited", "code", 1)
	ev.Info("state changed", "from", "running", "to", "degraded")
	ev.Info("state changed", "from", "degraded", "to", "failed")
	if err := rl.Close(); err != nil {
		t.Fatal(err)
	}

	entries, err := ReadLifecycle(dir, "sweep", time.Time{})
	if err != nil {
		t.Fatalf("ReadLifecycle: %v", err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Msg+" "+e.Fields["to"]+e.Fields["code"])
	}
	want := []string{"state changed starting", "state changed running", "exited 1", "state changed degraded", "state changed failed"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("entries = %q, want %q", got, want)
	}
	if d := time.Since(entries[0].Time); d < 0 || d > time.Minute {
		t.Errorf("first entry time %v is not the local time it was written at", entries[0].Time)
	}

	spans := RunSpans(entries)
	if len(spans) != 1 {
		t.Fatalf("spans = %+v, want one run", spans)
	}
	if spans[0].ExitCode == nil || *spans[0].ExitCode != 1 {
		t.Errorf("exit code = %v, want 1", spans[0].ExitCode)
	}
	if !spans[0].End.After(spans[0].Start) {
		t.Errorf("end %v not after start %v (ends round up a second)", spans[0].End, spans[0].Start)
	}
}

func TestRunSpans(t *testing.T) {
	at := func(sec int) time.Time { return time.Date(2026, 9, 11, 7, 40, sec, 0, time.Local) }
	state := func(sec int, to string) LifecycleEntry {
		return LifecycleEntry{Time: at(sec), Msg: "state changed", Fields: map[string]string{"to": to}}
	}
	exited := func(sec int, code string) LifecycleEntry {
		return LifecycleEntry{Time: at(sec), Msg: "exited", Fields: map[string]string{"code": code}}
	}

	spans := RunSpans([]LifecycleEntry{
		state(0, "starting"), state(0, "running"), state(5, "stopping"), state(6, "stopped"), // graceful stop: no exit line
		state(10, "starting"), state(10, "running"), // the daemon died with this run
		state(20, "starting"), state(20, "running"), exited(30, "0"),
		state(40, "starting"), state(40, "running"), state(45, "stopping"), state(46, "stopped"),
		state(50, "running"), // first start after a daemon boot: the log opened after "starting"
	})
	if len(spans) != 5 {
		t.Fatalf("spans = %+v, want 5", spans)
	}
	if !spans[0].End.Equal(at(7)) || spans[0].ExitCode != nil {
		t.Errorf("graceful stop span = %+v, want end %v and no exit code", spans[0], at(7))
	}
	if !spans[1].End.Equal(at(20)) {
		t.Errorf("unterminated run should close at the next start, got %+v", spans[1])
	}
	if !spans[2].End.Equal(at(31)) || spans[2].ExitCode == nil || *spans[2].ExitCode != 0 {
		t.Errorf("exited span = %+v, want end %v, code 0", spans[2], at(31))
	}
	if !spans[4].Start.Equal(at(50)) || !spans[4].End.IsZero() {
		t.Errorf("a run whose log starts at running should open there and stay open, got %+v", spans[4])
	}
}

// TestReadLifecycleFollowsRotation reads backups oldest first and then the
// active file, and never a sibling harness whose name shares the prefix.
func TestReadLifecycleFollowsRotation(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write("web-20260910T070000.000.log", "2026/09/10 07:00:00 INFO state changed from=stopped to=starting\n")
	write("web.log", "2026/09/10 07:05:00 INFO exited code=2\n")
	write("web-api.log", "2026/09/10 07:01:00 INFO exited code=9\n")
	write("web-api-20260910T070000.000.log", "2026/09/10 07:02:00 INFO exited code=9\n")

	entries, err := ReadLifecycle(dir, "web", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 || entries[0].Fields["to"] != "starting" || entries[1].Fields["code"] != "2" {
		t.Fatalf("entries = %+v, want web's start then web's exit", entries)
	}
}

func TestReadLifecycleSkipsFilesOlderThanSince(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "web-20260101T000000.000.log")
	if err := os.WriteFile(old, []byte("2026/01/01 00:00:00 INFO state changed from=stopped to=starting\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	entries, err := ReadLifecycle(dir, "web", time.Now().Add(-time.Hour))
	if err != nil || len(entries) != 0 {
		t.Fatalf("entries = %+v, err = %v; want the stale backup skipped and no error for a missing active log", entries, err)
	}
}

// TestDiscoveryEnvReturnsOnlyTheKeysAsked is the ADR-0008 boundary: discovery
// needs a couple of path variables from an env_file that also holds secrets.
func TestDiscoveryEnvReturnsOnlyTheKeysAsked(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/daemon/xdg")
	t.Setenv("HOME", "/daemon/home")
	envFile := filepath.Join(t.TempDir(), "agent.env")
	body := "SECRET_TOKEN=hunter2\nexport CRUSH_GLOBAL_DATA=\"/data/crush-signal\"\nXDG_DATA_HOME=/file/xdg\n"
	if err := os.WriteFile(envFile, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	h := shHarness("agent", "true", 0)
	h.EnvFile = envFile

	got, err := DiscoveryEnv(h, []string{"HOME", "XDG_DATA_HOME", "CRUSH_GLOBAL_DATA"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"HOME": "/daemon/home", "XDG_DATA_HOME": "/file/xdg", "CRUSH_GLOBAL_DATA": "/data/crush-signal"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("DiscoveryEnv = %v, want %v (env_file over daemon env, nothing else)", got, want)
	}
	if _, leaked := got["SECRET_TOKEN"]; leaked {
		t.Error("DiscoveryEnv returned a key it was not asked for")
	}
}

// TestRestoreSeedsLastStarted: a post-mortem after a daemon restart needs the
// last run's start, which state.json already persisted.
func TestRestoreSeedsLastStarted(t *testing.T) {
	s := newTestSupervisor(t, shHarness("sweep", "exit 0", 0), fastPolicy())
	started := time.Date(2026, 9, 11, 7, 40, 0, 106_000_000, time.Local)
	exited := started.Add(16 * time.Minute)
	s.Restore(false, 0, 1, exited, started)
	snap := s.Snapshot()
	if !snap.LastStarted.Equal(started) || !snap.LastExitAt.Equal(exited) || snap.LastExitCode != 1 {
		t.Errorf("snapshot = started %v exited %v code %d, want %v %v 1", snap.LastStarted, snap.LastExitAt, snap.LastExitCode, started, exited)
	}
}

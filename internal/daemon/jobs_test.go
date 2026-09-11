package daemon

// Scheduled Runs Over The Protocol — Tests
//
// A real daemon, client, scheduler and PTY-spawned `sh`, dialled over the Unix
// socket. The config is built by hand rather than parsed: the parser requires a
// scheduled harness to be a prompt one-shot, which would spawn an agent CLI a
// test machine does not have, and none of what is tested here depends on what
// the harness runs.
//
// Governing: SPEC-0002 REQ "Control Operations", REQ "Event Subscription";
// SPEC-0008 REQ "Protocol Operations", REQ "Lifecycle Events", REQ "Manual
// Trigger"; issue #120.
//
// @joestump-agent 09/11/2026 - Added for issue #120.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/attach"
	"gitea.stump.rocks/stump.wtf/harness/internal/client"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/scheduler"
	"gitea.stump.rocks/stump.wtf/harness/internal/supervisor"
)

// scheduledSh is a scheduled harness running `sh -c script`.
func scheduledSh(name, script, workdir string) core.Harness {
	return core.Harness{
		Name:      name,
		Adapter:   "generic",
		Args:      []string{"-c", script},
		Backend:   core.BackendNative,
		Workdir:   workdir,
		Restart:   core.RestartNo,
		Schedule:  "0 3 * * *",
		Timeout:   45 * time.Minute,
		OnOverlap: core.OverlapSkip,
		KeepRuns:  core.DefaultKeepRuns,
	}
}

// newJobsDaemon serves a hand-built config with a live scheduler wired the way
// the daemon wires it.
func newJobsDaemon(t *testing.T, hs ...core.Harness) (*testDaemon, *scheduler.Scheduler, *core.Config) {
	t.Helper()
	cfg := &core.Config{Harnesses: map[string]core.Harness{}, Profiles: map[string]core.Profile{}}
	for _, h := range hs {
		cfg.Harnesses[h.Name] = h
		cfg.HarnessOrder = append(cfg.HarnessOrder, h.Name)
	}
	tmp := t.TempDir()
	sockDir, err := os.MkdirTemp("/tmp", "hnj")
	if err != nil {
		t.Fatalf("sock dir: %v", err)
	}
	socket := filepath.Join(sockDir, "d.sock")

	reg := attach.NewRegistry(1000)
	mgr := supervisor.NewManager(cfg, supervisor.ManagerOptions{
		Policy:       supervisor.Policy{StopGrace: 200 * time.Millisecond},
		StatePath:    filepath.Join(tmp, "state.json"),
		LogDir:       filepath.Join(tmp, "logs"),
		ExtraOutFor:  reg.WriterFor,
		DropExtraOut: reg.Remove,
	})
	reg.SetController(mgr)
	sched := scheduler.New(scheduler.Options{NextChanged: mgr.PublishScheduleChanged})
	sched.Apply(cfg)

	srv := NewServer(Options{
		Manager:    mgr,
		Registry:   reg,
		Scheduler:  sched,
		SocketPath: socket,
		ConfigPath: filepath.Join(tmp, "harness.toml"),
		Version:    "test",
	})
	if err := srv.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve()
	td := &testDaemon{socket: socket, configPath: filepath.Join(tmp, "harness.toml"), mgr: mgr, reg: reg, srv: srv}
	t.Cleanup(func() {
		sched.Close()
		srv.Close()
		mgr.Close()
		_ = os.RemoveAll(sockDir)
	})
	return td, sched, cfg
}

// waitRunsOver polls the runs op until pred holds.
func waitRunsOver(t *testing.T, c *client.Client, name string, pred func([]protocol.RunInfo) bool) []protocol.RunInfo {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rd, err := c.Runs(name, 0)
		if err != nil {
			t.Fatalf("runs %s: %v", name, err)
		}
		if pred(rd.Runs) {
			return rd.Runs
		}
		if time.Now().After(deadline) {
			t.Fatalf("runs %s never matched: %+v", name, rd.Runs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func finishedN(n int) func([]protocol.RunInfo) bool {
	return func(rs []protocol.RunInfo) bool {
		if len(rs) != n {
			return false
		}
		for _, r := range rs {
			if r.Outcome == "running" {
				return false
			}
		}
		return true
	}
}

func errCode(t *testing.T, err error) protocol.ErrCode {
	t.Helper()
	var em *protocol.ErrorMsg
	if !errors.As(err, &em) {
		t.Fatalf("error %v is not a structured ERROR", err)
	}
	return em.Code
}

// TestJobsOp lists scheduled harnesses only, with the daemon-computed next
// window, run settings, latest run and failure streak.
func TestJobsOp(t *testing.T) {
	dir := t.TempDir()
	ok := scheduledSh("nightly", "exit 0", dir)
	ok.CatchUp = true
	ok.OnOverlap = core.OverlapQueue
	ok.KeepRuns = 5
	bad := scheduledSh("flaky", "exit 2", dir)
	resident := core.Harness{Name: "resident", Adapter: "generic", Args: []string{"-c", "sleep 60"}, Backend: core.BackendNative}
	td, sched, _ := newJobsDaemon(t, ok, bad, resident)
	c := td.dial(t, nil)

	jobs, err := c.Jobs()
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 2 || jobs[0].Name != "nightly" || jobs[1].Name != "flaky" {
		t.Fatalf("jobs = %+v, want nightly and flaky only", jobs)
	}
	j := jobs[0]
	want, _ := sched.NextFire("nightly")
	if next, err := time.Parse(time.RFC3339, j.NextRun); err != nil || !next.Equal(want.Truncate(time.Second)) {
		t.Errorf("next_run = %q, want the scheduler's %v", j.NextRun, want)
	}
	if !j.CatchUp || j.OnOverlap != "queue" || j.KeepRuns != 5 || j.TimeoutMs != (45*time.Minute).Milliseconds() || j.Schedule != "0 3 * * *" {
		t.Errorf("job settings = %+v", j)
	}
	if j.LastRun != nil || j.Running != nil || j.ConsecutiveFailures != 0 {
		t.Errorf("job before any run = %+v", j)
	}

	for range 2 {
		if _, err := c.Trigger("flaky"); err != nil {
			t.Fatal(err)
		}
		waitRunsOver(t, c, "flaky", func(rs []protocol.RunInfo) bool { return len(rs) > 0 && rs[0].Outcome != "running" })
	}
	if _, err := c.Trigger("nightly"); err != nil {
		t.Fatal(err)
	}
	waitRunsOver(t, c, "nightly", finishedN(1))

	jobs, err = c.Jobs()
	if err != nil {
		t.Fatal(err)
	}
	if last := jobs[0].LastRun; last == nil || last.Outcome != "success" || jobs[0].ConsecutiveFailures != 0 {
		t.Errorf("nightly after a success = %+v", jobs[0])
	}
	if last := jobs[1].LastRun; last == nil || last.Outcome != "failed" || jobs[1].ConsecutiveFailures != 2 {
		t.Errorf("flaky after two failures = %+v", jobs[1])
	}
}

// TestTriggerAndRunsOps: trigger starts a manual run and says so; runs returns
// the history newest first; unknown and unscheduled names are distinct errors.
func TestTriggerAndRunsOps(t *testing.T) {
	dir := t.TempDir()
	resident := core.Harness{Name: "resident", Adapter: "generic", Args: []string{"-c", "sleep 60"}, Backend: core.BackendNative}
	td, _, _ := newJobsDaemon(t, scheduledSh("nightly", "echo from-the-run; exit 0", dir), resident)
	c := td.dial(t, nil)

	for i := 1; i <= 2; i++ {
		tr, err := c.Trigger("nightly")
		if err != nil {
			t.Fatal(err)
		}
		if tr.Decision != protocol.TriggerStarted || tr.Run == nil || tr.Run.RunID != i || tr.Run.Trigger != "manual" {
			t.Errorf("trigger %d = %+v", i, tr)
		}
		waitRunsOver(t, c, "nightly", finishedN(i))
	}

	rd, err := c.Runs("nightly", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rd.Runs) != 2 || rd.Runs[0].RunID != 2 || rd.Runs[1].RunID != 1 {
		t.Fatalf("runs = %+v, want newest first", rd.Runs)
	}
	r := rd.Runs[0]
	if r.Outcome != "success" || r.ExitCode == nil || *r.ExitCode != 0 || !r.HasLog || r.EndedAt == "" {
		t.Errorf("run record on the wire = %+v", r)
	}
	if limited, err := c.Runs("nightly", 1); err != nil || len(limited.Runs) != 1 || limited.Runs[0].RunID != 2 {
		t.Errorf("runs limit 1 = %+v, %v", limited, err)
	}

	_, err = c.Trigger("resident")
	if got := errCode(t, err); got != protocol.ErrNotScheduled {
		t.Errorf("trigger of an unscheduled harness = %s, want %s", got, protocol.ErrNotScheduled)
	}
	_, err = c.Trigger("ghost")
	if got := errCode(t, err); got != protocol.ErrUnknownHarness {
		t.Errorf("trigger of an unknown harness = %s, want %s", got, protocol.ErrUnknownHarness)
	}
	_, err = c.Runs("ghost", 0)
	if got := errCode(t, err); got != protocol.ErrUnknownHarness {
		t.Errorf("runs of an unknown harness = %s, want %s", got, protocol.ErrUnknownHarness)
	}
}

// TestTriggerHonorsOnOverlap: a manual trigger during a run goes through the
// same overlap policy a schedule firing does.
func TestTriggerHonorsOnOverlap(t *testing.T) {
	td, _, _ := newJobsDaemon(t, scheduledSh("busy", "sleep 30", t.TempDir()))
	c := td.dial(t, nil)

	if tr, err := c.Trigger("busy"); err != nil || tr.Decision != protocol.TriggerStarted {
		t.Fatalf("first trigger = %+v, %v", tr, err)
	}
	tr, err := c.Trigger("busy")
	if err != nil {
		t.Fatal(err)
	}
	if tr.Decision != protocol.TriggerSkipped || tr.Run == nil || tr.Run.Outcome != "skipped" || tr.Run.RunID != 2 {
		t.Errorf("trigger during a run = %+v, want skipped record 2", tr)
	}
	if _, err := c.Stop("busy"); err != nil {
		t.Fatal(err)
	}
}

// TestLogsRunSelector: logs --run N reads that run and no other, in the raw
// and the events view alike; an unknown run is its own error.
func TestLogsRunSelector(t *testing.T) {
	dir := t.TempDir()
	counter := `n=$(cat count 2>/dev/null || echo 0); n=$((n+1)); echo "$n" > count; echo "output-of-run-$n"`
	td, _, _ := newJobsDaemon(t, scheduledSh("counter", counter, dir))
	c := td.dial(t, nil)

	for i := 1; i <= 2; i++ {
		if _, err := c.Trigger("counter"); err != nil {
			t.Fatal(err)
		}
		waitRunsOver(t, c, "counter", finishedN(i))
	}

	raw, err := c.RunLogs("counter", 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw.Text, "output-of-run-1") || strings.Contains(raw.Text, "output-of-run-2") {
		t.Errorf("raw --run 1 text:\n%s", raw.Text)
	}

	events, err := c.LogEvents("counter", client.LogOptions{Run: 2, Lines: 100})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(events.Text, "output-of-run-2") || strings.Contains(events.Text, "output-of-run-1") {
		t.Errorf("events --run 2 text:\n%s", events.Text)
	}

	_, err = c.RunLogs("counter", 99, 100)
	if got := errCode(t, err); got != protocol.ErrUnknownRun {
		t.Errorf("logs --run 99 = %s, want %s", got, protocol.ErrUnknownRun)
	}
}

// TestJobEventsOverTheWire: a subscriber sees job_run_started and
// job_run_finished for a triggered run, and job_schedule_changed when a reload
// moves the next window.
func TestJobEventsOverTheWire(t *testing.T) {
	h := scheduledSh("nightly", "exit 0", t.TempDir())
	td, sched, cfg := newJobsDaemon(t, h)
	sub := td.dial(t, []string{"events"})
	ctl := td.dial(t, nil)

	if _, err := ctl.Trigger("nightly"); err != nil {
		t.Fatal(err)
	}
	pc := sub.Conn()
	_ = sub.SetReadDeadline(time.Now().Add(5 * time.Second))
	read := func(kind protocol.EventKind) protocol.EventMsg {
		t.Helper()
		for {
			f, err := pc.ReadFrame()
			if err != nil {
				t.Fatalf("waiting for %s: %v", kind, err)
			}
			switch f.Type {
			case protocol.TypeEvent:
				if ev := decodeEvent(t, f.Payload); ev.Kind == kind && ev.Name == "nightly" {
					return ev
				}
			case protocol.TypePing:
				_ = pc.WriteFrame(protocol.TypePong, nil)
			}
		}
	}

	started := read(protocol.EvJobRunStarted)
	if started.RunID != 1 || started.Trigger != "manual" || started.Outcome != "running" {
		t.Errorf("job_run_started = %+v", started)
	}
	finished := read(protocol.EvJobRunFinished)
	if finished.RunID != 1 || finished.Outcome != "success" || finished.ExitCode == nil || *finished.ExitCode != 0 {
		t.Errorf("job_run_finished = %+v", finished)
	}

	h.Schedule = "30 4 * * *"
	cfg.Harnesses["nightly"] = h
	sched.Apply(cfg)
	changed := read(protocol.EvJobScheduleChanged)
	next, err := time.Parse(time.RFC3339, changed.NextRunAt)
	if err != nil || next.Minute() != 30 {
		t.Errorf("job_schedule_changed = %+v, want the new 04:30 window", changed)
	}
}

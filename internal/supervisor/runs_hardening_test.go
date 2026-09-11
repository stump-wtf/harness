package supervisor

// Run History Hardening Tests
//
// Review coverage for issue #119: a timeout ends the run's whole process group,
// not only its leader; a reload that drops a schedule mid-run still closes the
// run; a timed-out run never respawns; run ids survive a restart even when the
// newest records have no log to floor them; pruning holds under concurrent
// appends; and a run log path cannot leave the jobs root.
//
// Governing: ADR-0013; SPEC-0008 REQ "Run History", REQ "Per-Run Logs", REQ
// "Run Timeout"; issue #119.
//
// @joestump-agent 09/11/2026 - Added in review of PR #310.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// readPidFile waits for a child to write its pid.
func readPidFile(t *testing.T, path string) int {
	t.Helper()
	var pid int
	waitFor(t, 5*time.Second, "pid file "+filepath.Base(path), func() bool {
		b, err := os.ReadFile(path)
		if err != nil {
			return false
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(b)))
		pid = n
		return err == nil
	})
	return pid
}

// processLive reports whether pid is a running process. A zombie is not: an
// orphan the kernel has already killed can sit unreaped under a container's
// PID 1, and kill(pid, 0) would still call it alive.
func processLive(pid int) bool {
	if _, err := os.Stat("/proc/self/stat"); err == nil {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return false
		}
		s := string(b) // "pid (comm) S ..." — the state follows the last ')'
		i := strings.LastIndexByte(s, ')')
		return i < 0 || i+2 >= len(s) || (s[i+2] != 'Z' && s[i+2] != 'X')
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	st := strings.TrimSpace(string(out))
	return err == nil && st != "" && !strings.HasPrefix(st, "Z")
}

// TestRunTimeoutKillsWholeProcessGroup: a timed-out run leaves no child alive.
// Child A ignores SIGHUP, so only a signal to the whole group ends it — a
// SIGTERM sent to the leader alone would orphan it. Child B ignores SIGHUP and
// SIGTERM, so it survives the leader's exit and only a SIGKILL to what remains
// of the group ends it.
func TestRunTimeoutKillsWholeProcessGroup(t *testing.T) {
	e := newRunsEnv(t)
	a := filepath.Join(e.dir, "a.pid")
	b := filepath.Join(e.dir, "b.pid")
	script := fmt.Sprintf(`sh -c 'trap "" HUP; echo $$ > %s; while :; do sleep 0.05; done' &
sh -c 'trap "" HUP TERM; echo $$ > %s; while :; do sleep 0.05; done' &
wait`, a, b)
	h := sweep("tree", script)
	h.Timeout = time.Second
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	m.StartRun("tree", RunRequest{Trigger: TriggerSchedule})
	pa, pb := readPidFile(t, a), readPidFile(t, b)
	t.Cleanup(func() {
		for _, pid := range []int{pa, pb} {
			if p, err := os.FindProcess(pid); err == nil && processLive(pid) {
				_ = p.Kill()
			}
		}
	})
	waitRuns(t, m, "tree", "run times out", outcomesAre(OutcomeTimedOut))

	waitFor(t, 3*time.Second, "every child of the timed-out run is gone", func() bool {
		return !processLive(pa) && !processLive(pb)
	})
}

// TestScheduleRemovedMidRunClosesRun: a reload that drops `schedule` applies at
// once, so the run already in flight ends on the unscheduled exit path — and
// must still be closed there, log and record both.
func TestScheduleRemovedMidRunClosesRun(t *testing.T) {
	tally := &closeTally{}
	orig := openRunLog
	t.Cleanup(func() { openRunLog = orig })
	openRunLog = func(path string) (io.WriteCloser, error) {
		f, err := orig(path)
		if err != nil {
			return nil, err
		}
		tally.mu.Lock()
		tally.opened++
		tally.mu.Unlock()
		return tallyCloser{WriteCloser: f, tally: tally}, nil
	}

	e := newRunsEnv(t)
	h := sweep("sweep", "sleep 0.5; exit 0")
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())
	m.StartRun("sweep", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "sweep", "run in flight", outcomesAre(OutcomeRunning))

	unscheduled := h
	unscheduled.Schedule = ""
	m.Reload(sweepCfg(unscheduled))

	waitRuns(t, m, "sweep", "run closed on the unscheduled exit path", outcomesAre(OutcomeSuccess))
	tally.mu.Lock()
	defer tally.mu.Unlock()
	if tally.opened != 1 || tally.closed != 1 {
		t.Errorf("run logs opened %d, closed %d; want 1 of each", tally.opened, tally.closed)
	}
}

// TestRunTimeoutDoesNotRespawn: `restart = "on-failure"` on a scheduled harness
// does not turn a timed-out run into a respawn loop; the schedule is the retry.
func TestRunTimeoutDoesNotRespawn(t *testing.T) {
	e := newRunsEnv(t)
	h := sweep("hung", "sleep 30")
	h.Restart = core.RestartOnFailure
	h.Timeout = 200 * time.Millisecond
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	m.StartRun("hung", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "hung", "run times out", outcomesAre(OutcomeTimedOut))
	time.Sleep(500 * time.Millisecond)
	if rs := m.Runs("hung"); len(rs) != 1 {
		t.Errorf("records after a timeout = %v, want only the timed-out run", outcomesOf(rs))
	}
	if snap, _ := m.Snapshot("hung"); snap.State != core.StateFailed || snap.PID != 0 {
		t.Errorf("after a timeout: state %s, pid %d; want failed with no process", snap.State, snap.PID)
	}
}

// TestRunIDsSurviveRestartWithoutLogs: when the newest ids belong to records
// with no log (a missed window) and older logs have been pruned, only the
// persisted last id stands between a restart and a reissued id.
func TestRunIDsSurviveRestartWithoutLogs(t *testing.T) {
	e := newRunsEnv(t)
	h := sweep("sweep", "exit 0")
	h.KeepRuns = 1
	cfg := sweepCfg(h)
	m, closeM := e.manager(t, cfg, fastPolicy())

	m.StartRun("sweep", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "sweep", "run 1", outcomesAre(OutcomeSuccess))
	now := time.Now()
	if err := m.RecordMissed("sweep", now, now, 1, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(m.RunLogPath("sweep", 1)); !os.IsNotExist(err) {
		t.Fatalf("run 1's log should have been pruned with its record: %v", err)
	}
	closeM()

	m2, _ := e.manager(t, cfg, fastPolicy())
	m2.StartRun("sweep", RunRequest{Trigger: TriggerSchedule})
	rs := waitRuns(t, m2, "sweep", "run after restart finishes", func(rs []RunRecord) bool {
		return len(rs) == 1 && rs[0].Outcome == OutcomeSuccess
	})
	if rs[0].RunID != 3 {
		t.Errorf("run id after restart = %d, want 3 (after missed record 2)", rs[0].RunID)
	}
}

// TestRunHistoryPruningUnderConcurrency: appends from the actor loop (firings
// the skip policy drops) and from outside it (missed windows) race with each
// other and with readers. The bound holds, the run in flight survives with its
// log, ids are unique and in order, and no other log is left behind.
func TestRunHistoryPruningUnderConcurrency(t *testing.T) {
	e := newRunsEnv(t)
	h := sweep("busy", "sleep 30")
	h.KeepRuns = 3
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())
	m.StartRun("busy", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "busy", "run in flight", outcomesAre(OutcomeRunning))

	const workers, each = 8, 20
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range each {
				if w%2 == 0 {
					m.StartRun("busy", RunRequest{Trigger: TriggerSchedule})
				} else {
					now := time.Now()
					if err := m.RecordMissed("busy", now, now, 1, now); err != nil {
						t.Error(err)
					}
				}
				_ = m.Runs("busy")
			}
		}()
	}
	wg.Wait()

	rs := m.Runs("busy")
	if len(rs) != 3 || rs[0].RunID != 1 || rs[0].Outcome != OutcomeRunning {
		t.Fatalf("history = %+v, want 3 records led by run 1 in flight", rs)
	}
	for i := 1; i < len(rs); i++ {
		if rs[i].RunID <= rs[i-1].RunID {
			t.Errorf("ids out of order: %d then %d", rs[i-1].RunID, rs[i].RunID)
		}
	}
	if last := rs[len(rs)-1].RunID; last != 1+workers*each {
		t.Errorf("last id = %d, want %d: an id was lost or reissued", last, 1+workers*each)
	}
	entries, err := os.ReadDir(filepath.Join(e.jobs, "busy"))
	if err != nil {
		t.Fatal(err)
	}
	var files []string
	for _, entry := range entries {
		files = append(files, entry.Name())
	}
	if !slices.Equal(files, []string{"1.log"}) {
		t.Errorf("run logs on disk = %v, want only the in-flight [1.log]", files)
	}
	m.Stop("busy")
}

// TestRunLogPathStaysUnderJobsRoot: a name that is not a single path element
// gets no run log path at all, rather than one outside its own directory.
func TestRunLogPathStaysUnderJobsRoot(t *testing.T) {
	e := newRunsEnv(t)
	m, _ := e.manager(t, sweepCfg(sweep("sweep", "exit 0")), fastPolicy())
	for _, name := range []string{"", ".", "..", "../escape", "proj/sweep"} {
		if p := m.RunLogPath(name, 1); p != "" {
			t.Errorf("RunLogPath(%q) = %q, want none", name, p)
		}
	}
	if p, want := m.RunLogPath("sweep", 1), filepath.Join(e.jobs, "sweep", "1.log"); p != want {
		t.Errorf("RunLogPath(sweep) = %q, want %q", p, want)
	}
}

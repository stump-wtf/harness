package supervisor

// SPEC-0022 REQ-3 and REQ-5 for resident harnesses (#446): every process
// lifetime is a run, opened at spawn and closed however it ends. The records
// are read from the ledger's day files, not from the Manager's memory.

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/ledger"
)

// closedRecords folds the day files into one record per run of name, in run
// id order, keeping only runs with a closed line.
func closedRecords(t *testing.T, e runsEnv, name string) []map[string]any {
	t.Helper()
	byID := map[float64]map[string]any{}
	var order []float64
	closed := map[float64]bool{}
	for _, ln := range ledgerLines(t, e) {
		if ln["harness"] != name {
			continue
		}
		id := ln["run_id"].(float64)
		rec, ok := byID[id]
		if !ok {
			rec = map[string]any{}
			byID[id] = rec
			order = append(order, id)
		}
		for k, v := range ln {
			rec[k] = v
		}
		if ln["type"] == "closed" {
			closed[id] = true
		}
	}
	var out []map[string]any
	for _, id := range order {
		if closed[id] {
			out = append(out, byID[id])
		}
	}
	return out
}

func waitClosed(t *testing.T, e runsEnv, name string, n int, within time.Duration) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		recs := closedRecords(t, e, name)
		if len(recs) >= n {
			return recs
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: %d closed records on disk after %s, want %d: %v", name, len(recs), within, n, recs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// REQ-3 "A resident crash loop", through the real Manager with the daemon's
// own restart policy (DefaultPolicy, as cmd/harness wires it; see #315 for
// why not a hand-built one): four failed records with exit code 1, triggers
// autostart then restart, and consecutive run ids.
func TestResidentCrashLoopRecordsEveryLifetime(t *testing.T) {
	e := newRunsEnv(t)
	h := shHarnessWithRestart("loop", "exit 1", 0, core.RestartAlways)
	m, _ := e.manager(t, managerCfg(h), DefaultPolicy())
	m.Autostart()

	recs := waitClosed(t, e, "loop", 4, 20*time.Second)[:4]
	wantTriggers := []string{"autostart", "restart", "restart", "restart"}
	for i, r := range recs {
		if r["run_id"].(float64) != float64(i+1) {
			t.Errorf("record %d has run id %v, want %d", i, r["run_id"], i+1)
		}
		if r["trigger"] != wantTriggers[i] || r["outcome"] != "failed" || r["exit_code"] != float64(1) || r["kind"] != "resident" {
			t.Errorf("record %d = %v, want trigger %s, failed, exit 1, resident", i, r, wantTriggers[i])
		}
		if r["log"] != filepath.Join(e.logs, "loop.log") {
			t.Errorf("record %d log = %v, want the durable log", i, r["log"])
		}
	}
	m.Stop("loop")
}

// REQ-3 "A spawn failure": failed, no exit code, reason spawn.
func TestResidentSpawnFailureIsRecorded(t *testing.T) {
	e := newRunsEnv(t)
	h := shHarnessWithRestart("nowhere", "true", 0, core.RestartNo)
	h.Workdir = filepath.Join(e.dir, "does-not-exist")
	m, _ := e.manager(t, managerCfg(h), fastPolicy())
	m.Start("nowhere")

	r := waitClosed(t, e, "nowhere", 1, 5*time.Second)[0]
	if r["outcome"] != "failed" || r["reason"] != "spawn" || r["trigger"] != "manual" {
		t.Errorf("record = %v, want failed, reason spawn, trigger manual", r)
	}
	if _, has := r["exit_code"]; has {
		t.Errorf("a process that never started has an exit code: %v", r["exit_code"])
	}
}

// REQ-5: an operator restart replaces the run in flight and opens the next,
// and an operator stop cancels it; both say so.
func TestResidentOperatorStopAndRestart(t *testing.T) {
	e := newRunsEnv(t)
	h := shHarness("svc", "sleep 30", 0)
	m, _ := e.manager(t, managerCfg(h), fastPolicy())
	m.Start("svc")
	waitFor(t, 5*time.Second, "running", func() bool { s, _ := m.Snapshot("svc"); return s.State == core.StateRunning })
	m.Restart("svc")
	waitFor(t, 5*time.Second, "running again", func() bool { s, _ := m.Snapshot("svc"); return s.State == core.StateRunning })
	m.Stop("svc")

	recs := waitClosed(t, e, "svc", 2, 5*time.Second)
	if r := recs[0]; r["outcome"] != "replaced" || r["reason"] != "operator" || r["trigger"] != "manual" {
		t.Errorf("run 1 = %v, want replaced by the operator", r)
	}
	if r := recs[1]; r["outcome"] != "cancelled" || r["reason"] != "operator" || r["trigger"] != "manual" || r["run_id"] != float64(2) {
		t.Errorf("run 2 = %v, want run 2 cancelled by the operator", r)
	}
}

// REQ-5: a harness a reload removes has its run cancelled, reason reload.
func TestReloadRemovalCancelsTheRun(t *testing.T) {
	e := newRunsEnv(t)
	gone := shHarness("gone", "sleep 30", 0)
	keep := shHarness("keep", "sleep 30", 0)
	m, _ := e.manager(t, managerCfg(gone, keep), fastPolicy())
	m.Autostart()
	waitFor(t, 5*time.Second, "running", func() bool { s, _ := m.Snapshot("gone"); return s.State == core.StateRunning })

	m.Reload(managerCfg(keep))
	r := waitClosed(t, e, "gone", 1, 5*time.Second)[0]
	if r["outcome"] != "cancelled" || r["reason"] != "reload" || r["trigger"] != "autostart" {
		t.Errorf("removed harness's run = %v, want cancelled, reason reload", r)
	}
	if recs := closedRecords(t, e, "keep"); len(recs) != 0 {
		t.Errorf("the kept harness's run was closed by the reload: %v", recs)
	}
}

// Run ids for a resident continue across a daemon restart, and the restart
// itself closes the run interrupted, reason shutdown.
func TestResidentRunIDsContinueAcrossRestart(t *testing.T) {
	e := newRunsEnv(t)
	h := shHarness("svc", "sleep 30", 0)
	cfg := managerCfg(h)
	m, closeM := e.manager(t, cfg, fastPolicy())
	m.Autostart()
	waitFor(t, 5*time.Second, "running", func() bool { s, _ := m.Snapshot("svc"); return s.State == core.StateRunning })
	closeM()

	if r := waitClosed(t, e, "svc", 1, time.Second)[0]; r["outcome"] != "interrupted" || r["reason"] != "shutdown" || r["ended_at"] == nil {
		t.Errorf("run 1 after a clean shutdown = %v", r)
	}
	m2, _ := e.manager(t, cfg, fastPolicy())
	m2.Autostart()
	waitFor(t, 5*time.Second, "running", func() bool { s, _ := m2.Snapshot("svc"); return s.State == core.StateRunning })
	rs := m2.Runs("svc")
	if len(rs) != 2 || rs[1].RunID != 2 || rs[1].Trigger != TriggerAutostart || rs[1].Kind != KindResident {
		t.Errorf("after the restart: %+v, want run 2, autostart, resident", rs)
	}
	m2.Stop("svc")
}

// REQ-5's mapping, for the consecutive-failure count: which outcomes count,
// which reset, and which do neither.
func TestConsecutiveFailuresFollowsTheVerdict(t *testing.T) {
	for _, tc := range []struct {
		outcome RunOutcome
		verdict int
	}{
		{OutcomeSuccess, 1},
		{OutcomeFailed, -1},
		{OutcomeTimedOut, -1},
		{OutcomeBudgetExceeded, -1},
		{OutcomeModelMismatch, -1},
		{OutcomeModelUnattested, -1},
		{OutcomeQuotaParked, 0},
		{OutcomeSkipped, 0},
		{OutcomeMissed, 0},
		{OutcomeReplaced, 0},
		{OutcomeCancelled, 0},
		{OutcomeInterrupted, 0},
		{OutcomeRunning, 0},
	} {
		t.Run(string(tc.outcome), func(t *testing.T) {
			if got := tc.outcome.Verdict(); got != tc.verdict {
				t.Fatalf("Verdict = %d, want %d", got, tc.verdict)
			}
			// After one failure: a failure makes it 2, a success resets it
			// to 0, and anything else leaves it at 1.
			runs := []RunRecord{{Outcome: OutcomeSuccess}, {Outcome: OutcomeFailed}, {Outcome: tc.outcome}}
			want := map[int]int{1: 0, -1: 2, 0: 1}[tc.verdict]
			if got := ConsecutiveFailures(runs); got != want {
				t.Errorf("ConsecutiveFailures(success, failed, %s) = %d, want %d", tc.outcome, got, want)
			}
		})
	}
}

// REQ-4: a model_mismatch record carries its mismatch through the ledger.
func TestMismatchFieldRoundTrips(t *testing.T) {
	at := time.Date(2026, 9, 24, 3, 0, 0, 0, time.UTC)
	in := RunRecord{RunID: 3, Outcome: OutcomeModelMismatch, StartedAt: at,
		Mismatch: &RunMismatch{Kind: "provider", ServedModel: "m", ServedProvider: "p", At: at}}
	out := fromLedger(ledger.Folded{RunID: 3, Record: toLedger(in)})
	if out.Mismatch == nil || *out.Mismatch != *in.Mismatch {
		t.Errorf("mismatch = %+v, want %+v", out.Mismatch, in.Mismatch)
	}
	if got := fmt.Sprint(out.Outcome); got != "model_mismatch" {
		t.Errorf("outcome = %s", got)
	}
}

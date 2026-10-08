package supervisor

// A Slow Disk Does Not Stall Supervision
//
// SPEC-0021 REQ-21: admission, counters, the park detector and the gate never
// block the supervisor's loop on a ledger write beyond the one synchronous
// append admission requires, and a harness subject to no budget needs none
// (SPEC-0022 REQ-6, which names the exception). The park consult on
// the exit path must not add one either.
//
// Governing: SPEC-0021 REQ-21, REQ-4; SPEC-0022 REQ-6.
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#477.
// @joestump 10/08/2026 - A stop still waits (SPEC-0022 REQ-6, as amended).

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// REQ-21 Scenario "A slow disk does not stall supervision": with the
// ledger's writer held, as behind a disk that has fallen behind, a resident
// with no budget that exits restarts without waiting on its pending appends.
// Waiting would cost the ledger's SyncTimeout (2s) for the exit's `closed`
// line and again for the restart's `opened` line; before this change two
// restarts took 8.1s here.
func TestASlowDiskDoesNotStallSupervision(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "spawns")
	h := shHarness("plain", "echo ran >> '"+marker+"'; sleep 0.05; exit 1", 0)
	h.Restart = core.RestartAlways
	p := fastPolicy()
	p.MaxRestarts = 0 // never give up: the restarts are the point
	e := newRunsEnv(t)
	m, _ := e.manager(t, managerCfg(h), p)
	m.Start(h.Name)
	waitFor(t, 5*time.Second, "first spawn", func() bool { return spawns(t, marker) >= 1 })

	release := m.Ledger().HoldWritesForTesting()
	defer release()
	begin := time.Now()
	n0 := spawns(t, marker)
	waitFor(t, 20*time.Second, "two restarts", func() bool { return spawns(t, marker) >= n0+2 })
	if took := time.Since(begin); took >= 1500*time.Millisecond {
		t.Fatalf("two restarts took %v with the ledger held; a restart waited on a pending append", took)
	}

	// The lines were queued, not lost: once the disk catches up, every
	// record is there, in order.
	release()
	waitRuns(t, m, h.Name, "the held records landed", func(rs []RunRecord) bool { return len(rs) >= n0+2 })
}

// SPEC-0022 REQ-6 Scenario "A stop still waits for its record": the
// exception for an unbudgeted resident's own exits does not reach an operator
// stop. With the ledger's writer held, `harness stop` does not return until
// the run's `closed` line is written, so it never answers ahead of the record.
func TestAStopStillWaitsForItsRecord(t *testing.T) {
	h := shHarness("plain", "while true; do sleep 0.02; done", 0)
	e := newRunsEnv(t)
	m, _ := e.manager(t, managerCfg(h), fastPolicy())
	m.Start(h.Name)
	waitSnapshot(t, m, h.Name, "running", func(s Snapshot) bool { return s.State == core.StateRunning })

	release := m.Ledger().HoldWritesForTesting()
	defer release()
	done := make(chan struct{})
	go func() { m.Stop(h.Name); close(done) }()
	select {
	case <-done:
		t.Fatal("harness stop returned with its run's closed line still held")
	case <-time.After(500 * time.Millisecond):
	}
	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("harness stop never returned after the ledger caught up")
	}
	rs := m.Runs(h.Name)
	if len(rs) == 0 || rs[len(rs)-1].Outcome == OutcomeRunning {
		t.Fatalf("after the stop: records = %+v, want the run closed", rs)
	}
}

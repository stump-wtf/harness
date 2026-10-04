package supervisor

// A Slow Disk Does Not Stall Supervision
//
// SPEC-0021 REQ-21: admission, counters, the park detector and the gate never
// block the supervisor's loop on a ledger write beyond the one synchronous
// append admission requires, and a harness subject to no budget needs none
// (SPEC-0022 REQ-6: its supervision "SHALL NOT wait"). The park consult on
// the exit path must not add one either.
//
// Governing: SPEC-0021 REQ-21, REQ-4; SPEC-0022 REQ-6.
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#477.

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

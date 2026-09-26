package scheduler

// SPEC-0022 REQ-3 "An operating-hours close", driven by the scheduler's own
// gate pass on its fake clock in front of a real Manager (#446).

import (
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// The gate's close ends the run cancelled, reason hours; the next window's
// open starts a new run with trigger release.
func TestHoursCloseAndOpenAreRecordedAsRuns(t *testing.T) {
	r, m, _ := realGate(t, "TZ=UTC 09:00-13:00", core.HoursShutdownImmediate, time.Time{}, nil)

	// mon30 00:00 is out of hours: the first pass holds the running harness.
	r.tick()
	waitRunsFor(t, m, "the close ends run 1", func(rs []supervisor.RunRecord) bool {
		return len(rs) == 1 && rs[0].Outcome == supervisor.OutcomeCancelled
	})
	// Into the window: the pass releases it.
	r.runUntil(mon30(9, 1), time.Minute)
	rs := waitRunsFor(t, m, "the open starts run 2", func(rs []supervisor.RunRecord) bool {
		return len(rs) == 2 && rs[1].Outcome == supervisor.OutcomeRunning
	})

	if rs[0].Reason != supervisor.ReasonHours || rs[0].Trigger != supervisor.TriggerManual || rs[0].Kind != supervisor.KindResident {
		t.Errorf("run 1 = %+v, want a manual resident run cancelled by its hours", rs[0])
	}
	if rs[1].RunID != 2 || rs[1].Trigger != supervisor.TriggerRelease {
		t.Errorf("run 2 = %+v, want trigger release", rs[1])
	}
}

func waitRunsFor(t *testing.T, m *supervisor.Manager, desc string, pred func([]supervisor.RunRecord) bool) []supervisor.RunRecord {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rs := m.Runs("w")
		if pred(rs) {
			return rs
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: runs = %+v", desc, rs)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

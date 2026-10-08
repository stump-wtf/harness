package daemon

// Admission Refusals On The Wire
//
// SPEC-0021 REQ-21: ErrOverBudget, ErrParked and ErrLedgerUnavailable are
// sentinels the control op distinguishes. Over the real socket, a start and a
// trigger that admission refuses come back as structured errors whose code
// names the refusal and whose message names the spent counter; a ledger that
// cannot be written comes back as its own code (REQ-4 Scenario "The ledger
// cannot be written"), and doctor's daemon_info carries the failure.
//
// Governing: ADR-0027; SPEC-0021 REQ-4, REQ-5, REQ-21; SPEC-0002 REQ
// "Control Operations" (structured failure).
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#470.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/budget"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/protocol"
)

// admissionTOML is a resident and a scheduled one-shot with one run a day
// each, on a budget day whose start is twelve hours away, so the test cannot
// straddle a rollover.
func admissionTOML() string {
	at := time.Now().UTC().Add(12 * time.Hour)
	return fmt.Sprintf(`
[budget]
day_starts = "TZ=UTC %02d:%02d"

[harness.resident]
harness = "generic"
args = ["-c", "exit 0"]
restart = "no"
max_runs_per_day = 1

[harness.sweep]
harness = "command"
argv = ["/bin/sh", "-c", "exit 0"]
schedule = "0 7 * * *"
max_runs_per_day = 1
`, at.Hour(), at.Minute())
}

func wantRefusal(t *testing.T, what string, err error, code protocol.ErrCode, detail string) {
	t.Helper()
	var em *protocol.ErrorMsg
	if !errors.As(err, &em) || em.Code != code || !strings.Contains(em.Message, detail) {
		t.Fatalf("%s: err = %v, want a %s error naming %q", what, err, code, detail)
	}
}

func TestControlOpsReportOverBudget(t *testing.T) {
	td := newTestDaemon(t, admissionTOML())
	c := td.dial(t, nil)

	if _, err := c.Start("resident"); err != nil {
		t.Fatalf("first start: %v", err)
	}
	waitFor(t, "the resident's run to end", func() bool {
		s, _ := td.mgr.Snapshot("resident")
		return s.State == core.StateStopped && s.PID == 0
	})
	_, err := c.Start("resident")
	wantRefusal(t, "second start", err, protocol.ErrOverBudget, "resident: over budget: 1/1 runs today")
	if s, _ := td.mgr.Snapshot("resident"); !s.Holds.Has(core.HoldBudget) || !s.Enabled {
		t.Errorf("refused resident: holds=%s enabled=%v, want held for budget with its intent recorded", s.Holds, s.Enabled)
	}

	if _, err := c.Trigger("sweep"); err != nil {
		t.Fatalf("first trigger: %v", err)
	}
	// Ended, so the next trigger reaches admission rather than the overlap
	// policy.
	waitFor(t, "the sweep's run to end", func() bool {
		rs := td.mgr.Runs("sweep")
		return len(rs) == 1 && rs[0].Outcome != "running"
	})
	_, err = c.Trigger("sweep")
	wantRefusal(t, "second trigger", err, protocol.ErrOverBudget, "sweep: over budget: 1/1 runs today")
	rs := td.mgr.Runs("sweep")
	if n := len(rs); n != 2 || rs[1].Outcome != "skipped" || rs[1].Reason != "budget" {
		t.Errorf("sweep's history: %+v, want a run and a skipped budget record", rs)
	}
}

func TestControlOpsReportLedgerUnavailable(t *testing.T) {
	td := newTestDaemon(t, admissionTOML())
	c := td.dial(t, nil)
	dir := filepath.Join(filepath.Dir(td.mgr.LogDir()), "ledger")
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dir, []byte("not a directory\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Remove(dir) }) // so the ledger drains at close

	_, err := c.Trigger("sweep")
	wantRefusal(t, "trigger on a broken ledger", err, protocol.ErrLedgerUnavailable, "sweep: ledger unavailable")
	di, err := c.DaemonInfo()
	if err != nil {
		t.Fatal(err)
	}
	if di.RunLedger == nil || di.RunLedger.Refused < 1 || di.RunLedger.LastError == "" {
		t.Errorf("daemon_info run_ledger = %+v, want the refusal and its error", di.RunLedger)
	}
}

// TestAdmissionErrCodes pins the mapping for every sentinel, parked included:
// nothing parks a harness until the park story (#477), so no socket test can
// produce it yet.
func TestAdmissionErrCodes(t *testing.T) {
	for _, c := range []struct {
		err  error
		code protocol.ErrCode
		ok   bool
	}{
		{nil, "", false},
		{budget.Decision{Verdict: budget.Skip, Reason: budget.ReasonBudget, Detail: "40/40 runs today"}.Err(), protocol.ErrOverBudget, true},
		{budget.Decision{Verdict: budget.Hold, Reason: budget.ReasonQuotaParked, Detail: "parked"}.Err(), protocol.ErrParked, true},
		{budget.Decision{Verdict: budget.Skip, Reason: budget.ReasonLedgerUnavailable, Detail: "disk"}.Err(), protocol.ErrLedgerUnavailable, true},
		{budget.Decision{Verdict: budget.Hold, Reason: budget.ReasonHours}.Err(), "", false},
		{errors.New("something else"), "", false},
	} {
		if code, ok := admissionErrCode(c.err); code != c.code || ok != c.ok {
			t.Errorf("admissionErrCode(%v) = %q, %v; want %q, %v", c.err, code, ok, c.code, c.ok)
		}
	}
}

package supervisor

// Operating Hours Durable-Log Tests
//
// SPEC-0012 REQ "Operating Hours Visibility": a lifecycle line for each of
// close start, hold, open, lease start and lease end, stating the reason and
// the next transition. hours_close_test.go already covers "close ended" (a
// #384 requirement, the close's OUTCOME); these are the five distinct lines
// this story adds or extends.
//
// Governing: ADR-0019, SPEC-0012 REQ "Operating Hours Visibility".
//
// @joestump-agent 09/22/2026 - Added for stump.wtf/harness#385.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/hours"
)

// gatedExpr builds a gated harness with a REAL parsed HoursExpr (unlike
// hours_test.go's gated(), which leaves HoursExpr zero — fine for tests that
// drive Hold/Release directly, but nextHoursTransition needs a real
// expression to report anything but "unknown").
func gatedExpr(t *testing.T, h core.Harness, expr string, mode core.HoursShutdownMode) core.Harness {
	t.Helper()
	e, err := hours.Parse(expr)
	if err != nil {
		t.Fatalf("hours.Parse(%q): %v", expr, err)
	}
	h.OperatingHours = expr
	h.HoursExpr = e
	h.HoursShutdown = mode
	return h
}

func readLog(t *testing.T, logDir, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(logDir, name+".log"))
	if err != nil {
		t.Fatalf("read durable log: %v", err)
	}
	return string(data)
}

func waitLogContains(t *testing.T, logDir, name, substr string) string {
	t.Helper()
	var log string
	waitFor(t, 3*time.Second, "durable log contains \""+substr+"\"", func() bool {
		log = readLog(t, logDir, name)
		return strings.Contains(log, substr)
	})
	return log
}

// TestHoldLogsNextOpen: an immediate hold on a running, gated harness writes
// "held" with the reason and the RFC 3339 instant it next opens — the exact
// scenario SPEC-0012 names: "a harness is held at 13:00 ... its durable log
// gains a line saying it stopped for operating hours and when it next opens".
func TestHoldLogsNextOpen(t *testing.T) {
	h := gatedExpr(t, shHarness("gated", "while true; do sleep 0.02; done", 0), "Mon 09:00-13:00", core.HoursShutdownImmediate)
	m, _ := newStateManager(t, managerCfg(h), fastPolicy())
	logDir := m.LogDir()

	m.Start("gated")
	waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("gated"); return s.State == core.StateRunning })
	m.Hold("gated", core.HoursShutdownImmediate, time.Time{})

	log := waitLogContains(t, logDir, "gated", "held")
	if !strings.Contains(log, "reason=operating_hours") {
		t.Fatalf("held line missing the reason: tail:\n%s", tailOf(log, 400))
	}
	if !strings.Contains(log, "next=") {
		t.Errorf("held line missing the next transition: tail:\n%s", tailOf(log, 400))
	}
	if strings.Contains(log, "next=unknown") {
		t.Errorf("held line's next transition is \"unknown\" — the expression should have resolved one: tail:\n%s", tailOf(log, 400))
	}
}

// TestGracefulHoldLogsCloseStartNotHeld: a graceful hold on a RUNNING
// harness logs "close start", not "held" — the harness is still up, so a
// line claiming it stopped would be wrong. The eventual stop is "close
// ended" (hours_close_test.go, #384).
func TestGracefulHoldLogsCloseStartNotHeld(t *testing.T) {
	h := gatedExpr(t, shHarness("gated", "while true; do sleep 0.02; done", 0), "Mon 09:00-13:00", core.HoursShutdownGraceful)
	m, _ := newStateManager(t, managerCfg(h), fastPolicy())
	m.watch = newStubWatch() // no real turn-state I/O needed for this assertion
	logDir := m.LogDir()

	m.Start("gated")
	waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("gated"); return s.State == core.StateRunning })
	m.Hold("gated", core.HoursShutdownGraceful, time.Now())

	log := waitLogContains(t, logDir, "gated", "close start")
	if !strings.Contains(log, "reason=operating_hours") {
		t.Errorf("close start line missing the reason: tail:\n%s", tailOf(log, 400))
	}
	if !strings.Contains(log, "next=") {
		t.Errorf("close start line missing the next transition: tail:\n%s", tailOf(log, 400))
	}
	// The process is still running: "held" — the actual stop — must not
	// appear on its own line (only as the "reason" of a DIFFERENT event, but
	// this expression has none set up, so any bare occurrence is wrong).
	if strings.Contains(log, "msg=held") {
		t.Errorf("close start log wrongly also claims held while the process is still up: tail:\n%s", tailOf(log, 400))
	}
}

// TestReleaseLogsOpenWithNextClose: releasing a held harness logs "open"
// with the next close time.
func TestReleaseLogsOpenWithNextClose(t *testing.T) {
	h := gatedExpr(t, shHarness("gated", "while true; do sleep 0.02; done", 0), "Mon-Sun 09:00-13:00", core.HoursShutdownImmediate)
	m, _ := newStateManager(t, managerCfg(h), fastPolicy())
	logDir := m.LogDir()

	m.Start("gated")
	waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("gated"); return s.State == core.StateRunning })
	m.Hold("gated", core.HoursShutdownImmediate, time.Time{})
	waitFor(t, 3*time.Second, "stopped", func() bool { s, _ := m.Snapshot("gated"); return s.State == core.StateStopped })
	m.Release("gated")

	log := waitLogContains(t, logDir, "gated", "open")
	if !strings.Contains(log, "reason=operating_hours") {
		t.Errorf("open line missing the reason: tail:\n%s", tailOf(log, 400))
	}
	if !strings.Contains(log, "next=") {
		t.Errorf("open line missing the next transition: tail:\n%s", tailOf(log, 400))
	}
}

// TestLogLifecycleWritesDurableLine covers Manager.LogLifecycle, the seam
// the daemon uses for "lease start" and "lease end" (decided outside the
// actor loop, in internal/daemon/control.go and the scheduler's gate pass —
// this is the write path both go through). ok=false for an unknown harness.
func TestLogLifecycleWritesDurableLine(t *testing.T) {
	h := gatedExpr(t, shHarness("gated", "while true; do sleep 0.02; done", 0), "Mon 09:00-13:00", core.HoursShutdownImmediate)
	m, _ := newStateManager(t, managerCfg(h), fastPolicy())
	logDir := m.LogDir()
	m.Start("gated")
	waitFor(t, 3*time.Second, "running", func() bool { s, _ := m.Snapshot("gated"); return s.State == core.StateRunning })

	if ok := m.LogLifecycle("gated", "lease start", "reason", "operating_hours", "until", "2026-09-22T21:00:00Z"); !ok {
		t.Fatal("LogLifecycle returned ok=false for a known harness")
	}
	if ok := m.LogLifecycle("does-not-exist", "lease start"); ok {
		t.Error("LogLifecycle returned ok=true for an unknown harness")
	}

	log := waitLogContains(t, logDir, "gated", "lease start")
	if !strings.Contains(log, "until=2026-09-22T21:00:00Z") {
		t.Errorf("lease start line missing until: tail:\n%s", tailOf(log, 400))
	}

	if ok := m.LogLifecycle("gated", "lease end", "reason", "operating_hours"); !ok {
		t.Fatal("LogLifecycle(lease end) returned ok=false")
	}
	waitLogContains(t, logDir, "gated", "lease end")
}

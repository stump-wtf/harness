package daemon

// After-Hours Lease Control-Op Tests
//
// SPEC-0012 REQ "After-Hours Lease" over the real control socket: the `for`
// argument on start, its validation, its rejection where no lease applies,
// the default lease a plain start picks up out of hours, and the old-client
// path (start with no `for` on an ungated harness) being byte-for-byte the
// old behaviour. The in/out-of-hours windows are computed around the current
// wall clock in the harness TOML itself, so every scenario is deterministic
// whatever time the suite runs at.
//
// Governing: ADR-0019, SPEC-0012 REQ "After-Hours Lease"; issue #383.
//
// @joestump-agent 09/21/2026 - Added for stump.wtf/harness#383.

import (
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// leaseTOML builds a daemon config with two harnesses: a gated "late" one
// whose window is closed right now (ended a minute ago) and a plain one. The
// gated harness's TOML carries the operating_hours string the daemon parses.
func leaseTOML() string {
	closed := leaseWindowFor(-3*time.Minute, -1*time.Minute)
	return `
[harness.late]
harness = "generic"
args = ["-c", "while true; do sleep 0.2; done"]
operating_hours = "` + closed + `"
hours_shutdown = "immediate"

[harness.plain]
harness = "generic"
args = ["-c", "while true; do sleep 0.2; done"]
`
}

// leaseWindowFor is leaseWindow's daemon-side twin: a Mon-Sun window from two
// wall-clock offsets around now, minute-precision.
func leaseWindowFor(from, to time.Duration) string {
	now := time.Now()
	a, b := now.Add(from), now.Add(to)
	start, end := a.Format("15:04"), b.Format("15:04")
	if b.Before(a) {
		start, end = end, start
	}
	return "Mon-Sun " + start + "-" + end
}

// SPEC-0012 Scenario "Late-night start" (over the socket): a PLAIN start —
// the old-client shape, no `for` — of a gated, out-of-hours harness starts
// it, records intent, and creates the default one-hour lease. This is the
// scenario the 20:00-start-is-held-at-21:00 requirement lives or dies on.
func TestOpStartCreatesDefaultLeaseOutOfHours(t *testing.T) {
	td := newTestDaemon(t, leaseTOML())
	defer td.srv.Close()
	c := td.dial(t, nil)

	if _, err := c.Start("late"); err != nil {
		t.Fatalf("plain start out of hours: %v", err)
	}
	waitForState(t, c, "late", "running")
	snap, _ := td.mgr.Snapshot("late")
	if !snap.Enabled {
		t.Fatal("leased start must record enabled intent")
	}
	until, ok := td.mgr.Lease("late")
	if !ok {
		t.Fatal("no default lease after a plain start out of hours")
	}
	if until.Before(time.Now().Add(50*time.Minute)) || until.After(time.Now().Add(70*time.Minute)) {
		t.Fatalf("lease end %v, want the default ~now+1h", until)
	}
}

// An explicit for on a harness to which no lease applies is rejected with a
// "no lease applies" error, and the harness is not started.
func TestOpStartForRejectedWhereNoLeaseApplies(t *testing.T) {
	td := newTestDaemon(t, leaseTOML())
	defer td.srv.Close()
	c := td.dial(t, nil)

	if _, err := c.StartFor("plain", "2h"); err == nil {
		t.Fatal("start --for on an ungated harness succeeded; want no lease applies")
	} else if !strings.Contains(err.Error(), "no lease applies") {
		t.Fatalf("error = %v, want it to name the missing lease", err)
	}
	snap, ok := td.mgr.Snapshot("plain")
	if !ok || snap.State == core.StateRunning || snap.State == core.StateStarting {
		t.Fatalf("plain started through a rejected lease: state=%v", snap.State)
	}
}

// A non-positive or unparseable for is a bad request.
func TestOpStartForValidation(t *testing.T) {
	td := newTestDaemon(t, leaseTOML())
	defer td.srv.Close()
	c := td.dial(t, nil)

	for _, bad := range []string{"bogus", "0s", "-5m", ""} {
		if bad == "" {
			continue // the old-client path, covered separately
		}
		if _, err := c.StartFor("late", bad); err == nil {
			t.Fatalf("start --for %q succeeded; want a validation error", bad)
		} else if !strings.Contains(err.Error(), "for must be a positive duration") {
			t.Fatalf("--for %q error = %v, want the duration message", bad, err)
		}
	}
}

// Protocol compatibility: an old client's start with no for behaves exactly
// as before on an ungated harness — starts, and creates no lease.
func TestOpStartPlainStaysPlain(t *testing.T) {
	td := newTestDaemon(t, leaseTOML())
	defer td.srv.Close()
	c := td.dial(t, nil)

	if _, err := c.Start("plain"); err != nil {
		t.Fatalf("plain start: %v", err)
	}
	waitForState(t, c, "plain", "running")
	if _, ok := td.mgr.Lease("plain"); ok {
		t.Fatal("a plain start created a lease")
	}
}

// The leased start leaves a live lease the gate pass consults. (File-level
// durability is asserted at the supervisor level, where the state path is
// owned by the test: TestStartForPersistsLeaseAndStarts,
// TestRestartMidLeaseRoundTrip.)
func TestLeasedStartLeavesLiveLease(t *testing.T) {
	td := newTestDaemon(t, leaseTOML())
	defer td.srv.Close()

	if err := td.mgr.StartFor("late", time.Hour); err != nil {
		t.Fatalf("StartFor: %v", err)
	}
	if until, ok := td.mgr.Lease("late"); !ok {
		t.Fatal("no live lease after a leased start")
	} else if until.Before(time.Now().Add(55 * time.Minute)) {
		t.Fatalf("lease end %v, want ~now+1h", until)
	}
}

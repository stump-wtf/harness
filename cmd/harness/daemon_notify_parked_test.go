package main

// Parked Notification Tests
//
// Governing tests: SPEC-0003 REQ "Operator Notification" (parked);
// SPEC-0021 REQ-13, REQ-19.
//
// @joestump 10/04/2026 - Added with the parked notify event.

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/notify"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// The tars review lane (2026-10-04) through the daemon's own construction,
// with a notify hook: the first run's 402 parks the harness, and the hook
// hears it once — when it is released and what refused it — and never a
// give-up or a failed run, because the run ended quota_parked. The next
// firing, skipped by the park, says nothing more.
func TestDaemonNotifyParkedTheTarsShape(t *testing.T) {
	tmp := t.TempDir()
	hermeticHome(t, tmp)
	_, envFile := standIn(t, tmp, "crush", "crush-402")
	cfg := tarsConfig(t, tmp, envFile)
	nc, out := newNotifyHook(t)
	nc.Events = slices.Clone(core.NotifyEvents) // run_failed is opt-in
	nc.Cooldown = time.Millisecond              // a second park would not be suppressed
	cfg.Notify = nc
	d := newQuotaDaemon(t, cfg, time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC), true)
	n := startTestNotify(t, d.mgr)
	const name = "review"

	d.tick(time.Date(2026, 10, 4, 0, 5, 0, 0, time.UTC))
	got := waitHookEvent(t, n, out, core.NotifyParked)
	until := time.Date(2026, 10, 4, 0, 20, 0, 0, time.UTC)
	p := got.payload
	if p.Harness != name || p.Until != "2026-10-04T00:20:00Z" || p.Cause != "common/payment required" ||
		!strings.HasPrefix(p.Message, "review parked until "+until.Local().Format("Jan 2 15:04 MST")+": its provider refused it for quota (common/payment required).") {
		t.Fatalf("parked payload = %+v", p)
	}
	if !strings.Contains(got.env, "HARNESS_NOTIFY_EVENT=parked") {
		t.Fatalf("hook environment lacks the event:\n%s", got.env)
	}

	// 00:15: the firing is skipped by the park; nothing new to say.
	d.tick(time.Date(2026, 10, 4, 0, 15, 0, 0, time.UTC))
	waitRunsWhere(t, d.mgr, name, "the 00:15 firing skipped", func(rs []supervisor.RunRecord) bool {
		return len(rs) >= 2
	})
	time.Sleep(200 * time.Millisecond)
	finished := n.finished(t)
	n.requireHookSucceeded(t, finished)
	if finished[core.NotifyParked][notify.ResultOK] != 1 {
		t.Fatalf("parked deliveries = %v, want exactly one", finished[core.NotifyParked])
	}
	if len(finished[core.NotifyFailed]) > 0 || len(finished[core.NotifyRunFailed]) > 0 {
		t.Fatalf("a parked run was reported as a failure: %v", finished)
	}
}

package main

// Daemon Reload Reactions
//
// The Manager holds exactly one reload hook, and the daemon needs two
// reactions from it: re-apply schedules (ADR-0013) and warn that a changed
// [telemetry] table waits for a restart (SPEC-0015 REQ-2). startDaemonScheduler
// owns the hook and runs the others after its own; a second SetReloadHook
// anywhere would silently replace it. This drives the pairing runDaemon
// registers — startDaemonScheduler with telemetryReloadWarning — through a
// real Manager's Reload and asserts both reactions happen on the same reload,
// and that the warning stays quiet when [telemetry] did not change.
//
// Governing tests: ADR-0013, SPEC-0008 (schedules re-apply on reload);
// ADR-0022, SPEC-0015 REQ-2 (the restart warning).
//
// @joestump-agent 09/21/2026 - Added for harness#391, after the rebase onto
// the one-shots work made the scheduler's reload hook variadic.

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/attach"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/supervisor"
)

func TestDaemonReloadReappliesSchedulesAndWarnsOnTelemetry(t *testing.T) {
	tmp := t.TempDir()
	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = filepath.Join(tmp, "state.json")
	opts.LogDir = filepath.Join(tmp, "logs")

	withSchedule := func(expr string, tc core.TelemetryConfig) *core.Config {
		h := core.Harness{Name: "job", Adapter: "generic", Args: []string{"-c", "true"}, Backend: core.BackendNative, Schedule: expr}
		return &core.Config{
			Harnesses: map[string]core.Harness{h.Name: h}, HarnessOrder: []string{h.Name},
			Profiles: map[string]core.Profile{}, Telemetry: tc,
		}
	}
	running := core.DefaultTelemetryConfig()
	changed := running
	changed.Logs = true

	cfg := withSchedule("0 9 * * *", running)
	mgr := supervisor.NewManager(cfg, opts)
	reg.SetController(mgr)
	t.Cleanup(mgr.Close)

	now := time.Date(2026, 9, 21, 1, 0, 0, 0, time.Local) // cron runs in local time
	clock := &stubClock{now: now, ticks: make(chan time.Time)}
	sched := startDaemonScheduler(mgr, cfg, clock, telemetryReloadWarning(mgr, cfg.Telemetry))
	t.Cleanup(sched.Close)
	nineAM := time.Date(2026, 9, 21, 9, 0, 0, 0, time.Local)
	fivePM := time.Date(2026, 9, 21, 17, 0, 0, 0, time.Local)
	if next, ok := sched.NextFire("job"); !ok || !next.Equal(nineAM) {
		t.Fatalf("initial next fire = %v, %v; want %v", next, ok, nineAM)
	}

	var buf syncBuffer
	prev := log.Default()
	log.SetDefault(log.New(&buf))
	t.Cleanup(func() { log.SetDefault(prev) })
	const warning = "[telemetry] changed in harness.toml"

	// One reload that moves the schedule AND changes [telemetry]: both
	// reactions must fire.
	mgr.Reload(withSchedule("0 17 * * *", changed))
	if next, ok := sched.NextFire("job"); !ok || !next.Equal(fivePM) {
		t.Fatalf("after reload next fire = %v, %v; want %v (schedules not re-applied)", next, ok, fivePM)
	}
	if n := strings.Count(buf.String(), warning); n != 1 {
		t.Fatalf("restart warnings after a [telemetry] change = %d, want 1; log:\n%s", n, buf.String())
	}

	// A reload that moves the schedule but leaves [telemetry] as the daemon
	// is running it: schedules re-apply, no new warning.
	mgr.Reload(withSchedule("0 9 * * *", running))
	if next, ok := sched.NextFire("job"); !ok || !next.Equal(nineAM) {
		t.Fatalf("after second reload next fire = %v, %v; want %v", next, ok, nineAM)
	}
	if n := strings.Count(buf.String(), warning); n != 1 {
		t.Fatalf("restart warnings = %d after a reload that left [telemetry] alone, want still 1; log:\n%s", n, buf.String())
	}
}

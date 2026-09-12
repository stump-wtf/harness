package main

// Daemon Lifecycle Policy
//
// Governing tests: SPEC-0003 REQ "Backoff Give-Up"; ADR-0005 (capped-
// exponential backoff); issue #315.
//
// Give-up was specified, implemented, and promised by harness.toml's own
// comments — and switched off in every production daemon, because the Manager
// was constructed without a Policy and Policy.normalize backfills every field
// except MaxRestarts. Nothing caught it: the give-up tests in
// internal/supervisor each hand NewManager a policy of their own, and
// TestPolicyNormalizeFillsDefaults asserts the five fields that DO default
// while omitting MaxRestarts. No test ever looked at the policy the DAEMON
// builds, so that is what this file tests.
//
// @joestump-agent 09/12/2026 - Added with the #315 fix.

import (
	"path/filepath"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/attach"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/supervisor"
)

// The wiring: the options the daemon actually runs with must enable give-up.
// Asserting on DefaultPolicy directly would prove nothing — it always had
// MaxRestarts 5. The bug was that nothing used it.
func TestDaemonManagerOptionsEnableGiveUp(t *testing.T) {
	opts := daemonManagerOptions(attach.NewRegistry(100))
	if opts.Policy.MaxRestarts <= 0 {
		t.Fatalf("daemon Policy.MaxRestarts = %d, want > 0: zero means \"never give up\", "+
			"so a harness that fails on every run retries forever and a metered agent "+
			"burns quota around the clock (#315)", opts.Policy.MaxRestarts)
	}
	if want := supervisor.DefaultPolicy(); opts.Policy != want {
		t.Errorf("daemon Policy = %+v, want DefaultPolicy() %+v", opts.Policy, want)
	}
}

// The behavior, through the daemon's own wiring: a harness that fails every
// run must park in `failed` rather than respawn forever.
//
// Only the durations are shrunk, so the machine runs in milliseconds.
// MaxRestarts — the field under test — is whatever the daemon configures, so
// this fails if the daemon ever stops supplying a policy again. A test that
// built its own policy would pass either way, which is exactly how the
// original bug survived.
func TestDaemonPolicyParksAReliablyFailingHarness(t *testing.T) {
	tmp := t.TempDir()
	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = filepath.Join(tmp, "state.json")
	opts.LogDir = filepath.Join(tmp, "logs")
	opts.Policy.CrashWindow = 20 * time.Millisecond
	opts.Policy.CrashThreshold = 3
	opts.Policy.BackoffBase = 2 * time.Millisecond
	opts.Policy.BackoffCap = 10 * time.Millisecond
	// Zero re-derives from CrashWindow in normalize (600ms here), so an
	// instantly-exiting run never counts as "came up" and clears the budget.
	opts.Policy.HealthyRun = 0
	opts.Policy.StopGrace = 80 * time.Millisecond

	h := core.Harness{
		Name:         "alwaysfails",
		Adapter:      "generic",
		Args:         []string{"-c", "exit 1"},
		Backend:      core.BackendNative,
		Restart:      core.RestartOnFailure,
		RestartDelay: time.Millisecond,
		Enabled:      true,
	}
	cfg := &core.Config{
		Harnesses:    map[string]core.Harness{h.Name: h},
		HarnessOrder: []string{h.Name},
		Profiles:     map[string]core.Profile{},
	}

	mgr := supervisor.NewManager(cfg, opts)
	reg.SetController(mgr)
	t.Cleanup(mgr.Close)

	if !mgr.Start(h.Name) {
		t.Fatal("Start returned false for a configured harness")
	}

	deadline := time.Now().Add(5 * time.Second)
	var last core.State
	for time.Now().Before(deadline) {
		snap, ok := mgr.Snapshot(h.Name)
		if ok {
			last = snap.State
			if last == core.StateFailed {
				return
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
	snap, _ := mgr.Snapshot(h.Name)
	t.Fatalf("harness never gave up: state=%s restarts=%d flapping=%v (MaxRestarts=%d) — "+
		"it would retry forever, which is #315",
		last, snap.RestartCount, snap.Flapping, opts.Policy.MaxRestarts)
}

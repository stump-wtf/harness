package main

// Daemon Runaway Tool-Loop Guard
//
// Governing tests: stumpcloud/stumpcloud#469.
//
// The guard's own tests stop harnesses through a fake. This drives what the
// daemon builds — startDaemonObserver with daemonObserverOptions and
// startDaemonLoopGuard with the production threshold — over a real Manager
// running a (stand-in) crush process, plants the incident's transcript in its
// store, and checks the real Manager stopped the harness, cleared its intent
// so nothing restarts it into the same loop, and said so in its own log.
// Only the poll interval is shrunk.
//
// @joestump-agent 09/23/2026 - Added with the guard.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/attach"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/loopguard"
	rt "github.com/stump-wtf/harness/internal/runtrace/runtracetest"
	"github.com/stump-wtf/harness/internal/supervisor"
)

func TestDaemonLoopGuardStopsTheRealHarness(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("CRUSH_GLOBAL_DATA", "")
	t.Setenv("XDG_DATA_HOME", "")
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "crush"), []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	work := filepath.Join(tmp, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}

	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = filepath.Join(tmp, "state.json")
	opts.LogDir = filepath.Join(tmp, "logs")
	opts.Policy.StopGrace = 100 * time.Millisecond
	h := core.Harness{Name: "pool-worker", Adapter: "crush", Workdir: work, Backend: core.BackendNative, Restart: core.RestartAlways, Enabled: true}
	cfg := &core.Config{Harnesses: map[string]core.Harness{h.Name: h}, HarnessOrder: []string{h.Name}, Profiles: map[string]core.Profile{}}
	mgr := supervisor.NewManager(cfg, opts)
	reg.SetController(mgr)
	t.Cleanup(mgr.Close)
	if !mgr.Start(h.Name) {
		t.Fatal("Start returned false for a configured harness")
	}
	waitFor(t, "the harness to come up", func() bool {
		snap, _ := mgr.Snapshot(h.Name)
		return snap.State == core.StateRunning && snap.PID != 0
	})

	db := filepath.Join(work, ".crush", "crush.db")
	now := time.Now()
	rt.WriteCrushDB(t, db, rt.CrushSession{
		ID: "live", Created: now, Updated: now,
		Messages: []rt.CrushMessage{{Role: "user", At: now, Parts: `[{"type":"text","data":{"text":"triage harness#383"}}]`}},
	})
	obsOpts := daemonObserverOptions()
	obsOpts.PollInterval = 10 * time.Millisecond
	obs := startDaemonObserver(mgr, obsOpts)
	t.Cleanup(obs.Stop)
	guard := startDaemonLoopGuard(mgr, obs, loopguard.Options{})
	t.Cleanup(guard.Close)

	incident := map[string]any{"method": "add_comment", "owner": "stump.wtf", "repo": "harness", "index": 383, "body": "."}
	comment := func(i int) {
		id := fmt.Sprintf("c%d", i)
		at := time.Now()
		rt.AppendCrushMessages(t, db, "live",
			rt.CrushMessage{Role: "assistant", At: at, Parts: rt.ToolCall(id, "mcp_gitea_issue_write", incident)},
			rt.CrushMessage{Role: "tool", At: at, Parts: rt.ToolResult(id, fmt.Sprintf(`{"id":%d}`, i))},
		)
	}
	// One short: once the guard has read every call the observer delivered,
	// the harness must still be running.
	for i := 1; i < loopguard.DefaultThreshold; i++ {
		comment(i)
	}
	waitFor(t, "the guard to read the first calls", func() bool {
		d := obs.Stats().Delivered
		return d >= loopguard.DefaultThreshold-1 && guard.Seen() == d
	})
	if snap, _ := mgr.Snapshot(h.Name); snap.State != core.StateRunning || len(guard.Trips()) != 0 {
		t.Fatalf("harness %s (trips %+v) after %d identical calls, want running until %d", snap.State, guard.Trips(), loopguard.DefaultThreshold-1, loopguard.DefaultThreshold)
	}
	comment(loopguard.DefaultThreshold)

	waitFor(t, "the guard to stop the harness", func() bool {
		snap, _ := mgr.Snapshot(h.Name)
		return snap.State == core.StateStopped && !snap.Enabled
	})
	if trips := guard.Trips(); len(trips) != 1 || trips[0].Count != loopguard.DefaultThreshold {
		t.Fatalf("trips = %+v, want one at the default threshold %d", trips, loopguard.DefaultThreshold)
	}
	var logText string
	waitFor(t, "the harness log to record the stop", func() bool {
		b, _ := os.ReadFile(filepath.Join(opts.LogDir, h.Name+".log"))
		logText = string(b)
		return strings.Contains(logText, "runaway tool loop")
	})
	for _, want := range []string{"mcp_gitea_issue_write", fmt.Sprint(loopguard.DefaultThreshold)} {
		if !strings.Contains(logText, want) {
			t.Errorf("harness log lacks %q:\n%s", want, logText)
		}
	}
	// restart = "always" would bring a crashed harness back; a guard stop is
	// the operator's kind of stop, so it stays down.
	time.Sleep(300 * time.Millisecond)
	if snap, _ := mgr.Snapshot(h.Name); snap.State != core.StateStopped {
		t.Fatalf("harness came back as %s after the guard stopped it", snap.State)
	}
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

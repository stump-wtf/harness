package main

// Daemon Agent Event Observer
//
// Governing tests: issue #390.
//
// The observer's own tests drive it through a fake Source. What they cannot
// show is that the daemon builds one over its real Manager, that the real
// Snapshot/HarnessRecord answers attribute a session to the harness the
// Manager is running, and that the loop the daemon starts actually delivers —
// the same gap #315 fell through with a Policy nobody wired. So this drives
// startDaemonObserver with daemonObserverOptions against a real Manager and a
// real (stand-in) crush process, and shrinks only the poll interval.
//
// @joestump-agent 09/21/2026 - Added with the observer (harness#390).

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/attach"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/observe"
	rt "github.com/stump-wtf/harness/internal/runtrace/runtracetest"
	"github.com/stump-wtf/harness/internal/supervisor"
	"github.com/stump-wtf/harness/internal/testwait"
)

// fakeCrushOnPath puts a `crush` stand-in first on PATH: the crush adapter
// spawns `crush`, and this one stays up, without the real tool, until the
// supervisor stops it.
//
// Until it is stopped, not for a while. It used to exit after 30 seconds,
// and a loaded runner can take longer than that to get from the spawn to the
// observer's first scan. The harness has exited by then, its run window closes
// before the session is written, and the observer rightly attributes the
// session to no one. CI run 13777 ran these tests for 60-141s and reported
// exactly that: Sessions:0, Delivered:0.
func fakeCrushOnPath(t *testing.T, tmp string) {
	t.Helper()
	bin := filepath.Join(tmp, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "crush"), []byte("#!/bin/sh\nwhile :; do sleep 1; done\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestDaemonObserverDeliversForTheRealManager(t *testing.T) {
	tmp := t.TempDir()
	// Discovery reads the daemon's own environment; keep crush's registry
	// source off the real home directory.
	t.Setenv("HOME", tmp)
	t.Setenv("CRUSH_GLOBAL_DATA", "")
	t.Setenv("XDG_DATA_HOME", "")
	fakeCrushOnPath(t, tmp)
	work := filepath.Join(tmp, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}

	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = filepath.Join(tmp, "state.json")
	opts.LogDir = filepath.Join(tmp, "logs")
	opts.Policy.StopGrace = 100 * time.Millisecond
	h := core.Harness{
		Name:    "worker",
		Adapter: "crush",
		Workdir: work,
		Backend: core.BackendNative,
		Restart: core.RestartNo,
		Enabled: true,
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
	deadline := time.Now().Add(testwait.Budget(t, 5*time.Second))
	for {
		if snap, _ := mgr.Snapshot(h.Name); snap.State == core.StateRunning && snap.PID != 0 {
			break
		}
		if time.Now().After(deadline) {
			snap, _ := mgr.Snapshot(h.Name)
			t.Fatalf("harness never came up: %+v", snap)
		}
		time.Sleep(5 * time.Millisecond)
	}

	db := filepath.Join(work, ".crush", "crush.db")
	now := time.Now()
	rt.WriteCrushDB(t, db, rt.CrushSession{
		ID: "live", Created: now, Updated: now,
		Messages: []rt.CrushMessage{{Role: "user", At: now, Parts: `[{"type":"text","data":{"text":"go"}}]`}},
	})

	obsOpts := daemonObserverOptions()
	obsOpts.PollInterval = 10 * time.Millisecond
	obs := startDaemonObserver(mgr, obsOpts)
	t.Cleanup(obs.Stop)
	ch, cancel := obs.Subscribe("test", 16)
	defer cancel()

	rt.AppendCrushMessages(t, db, "live", rt.CrushMessage{Role: "assistant", At: time.Now(), Parts: rt.FinishError("quota exhausted", "429")})

	timeout := time.After(testwait.Budget(t, 10*time.Second))
	for {
		select {
		case ev := <-ch:
			if ev.Kind != observe.KindMark || ev.Mark.Type != "error" {
				continue // the user message, delivered first
			}
			if ev.Harness != h.Name || ev.Adapter != "crush" {
				t.Fatalf("error attributed to %q (%q), want %q (crush)", ev.Harness, ev.Adapter, h.Name)
			}
			return
		case <-timeout:
			t.Fatalf("no error mark from the daemon's observer; stats %+v", obs.Stats())
		}
	}
}

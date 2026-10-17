package supervisor

// Governing tests: ADR-0045 (proposed); stump.wtf/harness#960.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

func writeProbe(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "probe.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHealthProberRestartsAfterStreak(t *testing.T) {
	probe := writeProbe(t, "echo queue depth 9\ndate\nexit 1")
	ups := filepath.Join(t.TempDir(), "ups.txt")
	h := shHarness("worker", "echo up >> "+ups+"\nwhile true; do sleep 0.02; done", 0)
	h.HealthCmd = probe
	h.HealthInterval = 60 * time.Millisecond
	h.HealthFailures = 2
	h.HealthAction = core.HealthRestart
	m := newTestManager(t, managerCfg(h))
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	m.Autostart()
	waitFor(t, 3*time.Second, "worker running", func() bool {
		snap, _ := m.Snapshot("worker")
		return snap.State == core.StateRunning
	})
	p := NewHealthProber(m, 10*time.Millisecond)
	m.SetHealthProber(p)
	p.Start()
	defer p.Close()

	// A verb-style restart does not move restartCount (that counts crash
	// respawns), so the test counts process starts instead.
	waitFor(t, 5*time.Second, "health restart after two failures", func() bool {
		data, err := os.ReadFile(ups)
		if err != nil {
			return false
		}
		return strings.Count(string(data), "up") >= 2
	})
	if got := p.Flag("worker"); got != "failing" {
		t.Fatalf("Flag = %q, want failing", got)
	}
	probes := m.HealthProbes("worker")
	if len(probes) == 0 || probes[len(probes)-1].OK {
		t.Fatalf("HealthProbes = %+v, want last probe failed", probes)
	}
	if !strings.Contains(probes[len(probes)-1].Output, "queue depth 9") {
		t.Fatalf("probe output = %q, want the failure tail", probes[len(probes)-1].Output)
	}
}

func TestHealthProberNotifyActionDoesNotRestart(t *testing.T) {
	probe := writeProbe(t, "exit 7")
	h := shHarness("watched", "while true; do sleep 0.02; done", 0)
	h.HealthCmd = probe
	h.HealthInterval = 60 * time.Millisecond
	h.HealthFailures = 2
	h.HealthAction = core.HealthNotify
	m := newTestManager(t, managerCfg(h))
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	m.Autostart()
	waitFor(t, 3*time.Second, "watched running", func() bool {
		snap, _ := m.Snapshot("watched")
		return snap.State == core.StateRunning
	})
	events, cancel := m.Events()
	defer cancel()
	p := NewHealthProber(m, 10*time.Millisecond)
	m.SetHealthProber(p)
	p.Start()
	defer p.Close()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Kind == EventHealth && ev.Name == "watched" {
				if ev.Action != string(core.HealthNotify) {
					t.Fatalf("event action = %q, want notify", ev.Action)
				}
				if ev.Streak != 2 || ev.Code != 7 {
					t.Fatalf("event streak/code = %d/%d, want 2/7", ev.Streak, ev.Code)
				}
				snap, _ := m.Snapshot("watched")
				if snap.RestartCount != 0 {
					t.Fatalf("notify action restarted the harness %d times", snap.RestartCount)
				}
				return
			}
		case <-deadline:
			t.Fatal("no EventHealth within the deadline")
		}
	}
}

func TestHealthProberHealthyProbePassesEnv(t *testing.T) {
	calls := filepath.Join(t.TempDir(), "calls.txt")
	probe := writeProbe(t, `echo "${HARNESS_LAST_SUCCESSFUL_CALL}" >> '`+calls+`'
[ "$HARNESS_NAME" = worker ] && [ -n "$HARNESS_PID" ] && [ "$HARNESS_STATE" = running ]`)
	h := shHarness("worker", "while true; do sleep 0.02; done", 0)
	h.HealthCmd = probe
	h.HealthInterval = 40 * time.Millisecond
	h.HealthFailures = 2
	m := newTestManager(t, managerCfg(h))
	if err := m.Restore(); err != nil {
		t.Fatal(err)
	}
	m.Autostart()
	waitFor(t, 3*time.Second, "worker running", func() bool {
		snap, _ := m.Snapshot("worker")
		return snap.State == core.StateRunning
	})
	p := NewHealthProber(m, 10*time.Millisecond)
	m.SetHealthProber(p)
	p.Start()
	defer p.Close()

	waitFor(t, 5*time.Second, "two successful probes", func() bool {
		data, err := os.ReadFile(calls)
		if err != nil {
			return false
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		return len(lines) >= 2 && strings.TrimSpace(lines[1]) != ""
	})
	if got := p.Flag("worker"); got != "ok" {
		t.Fatalf("Flag = %q, want ok", got)
	}
	snap, _ := m.Snapshot("worker")
	if snap.Health != "ok" {
		t.Fatalf("snapshot Health = %q, want ok", snap.Health)
	}
	if snap.RestartCount != 0 {
		t.Fatalf("healthy probe restarted %d times", snap.RestartCount)
	}
}

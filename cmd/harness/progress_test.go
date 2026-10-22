package main

// Governing: ADR-0048 (proposed), ADR-0020; issue #958 — `harness describe`
// shows when the agent last made a successful model call and what the children
// of its process have been doing, so a worker blocked for hours inside one tool
// call stops reading as healthy. The daemon sources the timestamp from the same
// progress clock the stall watchdog and /metrics use, so this test wires a
// stand-in for it and checks the rows the operator reads.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/attach"
	"github.com/stump-wtf/harness/internal/buildinfo"
	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/daemon"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/supervisor"
	"github.com/stump-wtf/harness/internal/testwait"
)

// stubProgress stands in for the stall watchdog's progress clock.
type stubProgress struct {
	at time.Time
}

func (p stubProgress) LastCall(string) (time.Time, bool) { return p.at, !p.at.IsZero() }

// bootProgressDaemon is a daemon with one resident harness that keeps a
// long-lived child of its own, and a process-table sample taken often enough
// that a test can wait for it.
func bootProgressDaemon(t *testing.T, at time.Time) string {
	t.Helper()
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "harness.toml")
	if err := os.WriteFile(configPath, []byte(
		"[harness.demo]\nharness = \"generic\"\nargs = [\"-c\", \"sleep 300 & wait\"]\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(shortSockDir(t), "d.sock")

	reg := attach.NewRegistry(1000)
	mgr := supervisor.NewManager(cfg, supervisor.ManagerOptions{
		StatePath:   tmp + "/state.json",
		LogDir:      filepath.Join(tmp, "logs"),
		ExtraOutFor: reg.WriterFor,
	})
	reg.SetController(mgr)

	srv := daemon.NewServer(daemon.Options{
		Manager:          mgr,
		Registry:         reg,
		SocketPath:       socket,
		ConfigPath:       configPath,
		Version:          buildinfo.Version,
		ChildrenInterval: 200 * time.Millisecond,
	})
	srv.SetProgress(stubProgress{at: at})
	if err := srv.Listen(); err != nil {
		t.Fatal(err)
	}
	go srv.Serve()
	t.Cleanup(func() {
		srv.Close()
		mgr.Close()
	})
	return socket
}

func waitForRunningChild(t *testing.T, c *client.Client, name string) protocol.HarnessInfo {
	t.Helper()
	deadline := testwait.Budget(t, 30*time.Second)
	start := time.Now()
	for time.Since(start) < deadline {
		h, err := c.Describe(name)
		if err != nil {
			t.Fatalf("describe: %v", err)
		}
		if h.State == "running" {
			for _, kid := range h.Children {
				if kid.PID > 0 && kid.PID != h.PID {
					return h
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the harness's own child never appeared in its snapshot")
	return protocol.HarnessInfo{}
}

func TestDescribeShowsLastCallAndChildren(t *testing.T) {
	stalled := time.Now().Add(-23 * time.Hour).Truncate(time.Second)
	socket := bootProgressDaemon(t, stalled)
	c := dialTest(t, socket)

	if _, err := callLifecycle(c, "start", "demo", ""); err != nil {
		t.Fatal(err)
	}
	h := waitForRunningChild(t, c, "demo")

	out, err := captureStdout(t, func() error {
		return withClient(verbOpts{socket: socket, name: "demo"}, nil, cmdDescribe)
	})
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if !strings.Contains(out, "last call") {
		t.Errorf("describe shows no last call row:\n%s", out)
	}
	if !strings.Contains(out, "23h") {
		t.Errorf("describe does not phrase the 23 hours without a call:\n%s", out)
	}
	if !strings.Contains(out, "child") || !strings.Contains(out, "cpu") {
		t.Errorf("describe shows no child row with its cpu:\n%s", out)
	}
	if !strings.Contains(out, "sleep") {
		t.Errorf("describe's child row does not name the child (%+v):\n%s", h.Children, out)
	}
}

// A harness the daemon has never seen make a call says so rather than printing
// a timestamp from 1970.
func TestDescribeSaysNoCallWasSeen(t *testing.T) {
	socket := bootProgressDaemon(t, time.Time{})
	c := dialTest(t, socket)
	if _, err := callLifecycle(c, "start", "demo", ""); err != nil {
		t.Fatal(err)
	}
	waitForRunningChild(t, c, "demo")

	out, err := captureStdout(t, func() error {
		return withClient(verbOpts{socket: socket, name: "demo"}, nil, cmdDescribe)
	})
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if !strings.Contains(out, "none since the daemon started") {
		t.Errorf("describe invented a call for a harness the observer never reported on:\n%s", out)
	}
}

func TestChildCellNamesAgeAndCPU(t *testing.T) {
	got := childCell(protocol.ChildProcess{
		PID:       4242,
		Command:   "grep",
		StartedAt: time.Now().Add(-3 * time.Hour).Format(time.RFC3339Nano),
		CPUMs:     0,
	})
	for _, want := range []string{"pid 4242", "grep", "up 3h", "cpu 0s"} {
		if !strings.Contains(got, want) {
			t.Errorf("childCell = %q, missing %q", got, want)
		}
	}
}

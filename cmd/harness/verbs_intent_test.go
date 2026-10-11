package main

// Governing: issue #835, ask 2 — `harness describe` shows the last
// enabled-intent change and its source (and the peer, when the platform
// gave one), so "who turned this off, and when" no longer lives only in the
// daemon journal.

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

// bootIntentDaemon is bootTestDaemon with a real resident command, so the
// intent verbs have something to flip on.
func bootIntentDaemon(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "harness.toml")
	// generic execs sh with args appended, so args start at -c: a leading
	// "sh" made Linux's dash try to open a script named sh and exit 2 in
	// about a millisecond, and the test passed only when a poll landed in
	// that window.
	if err := os.WriteFile(configPath, []byte(
		"[harness.demo]\nharness = \"generic\"\nargs = [\"-c\", \"while true; do sleep 0.05; done\"]\nenabled = false\n",
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
		StatePath:   filepath.Join(tmp, "state.json"),
		LogDir:      filepath.Join(tmp, "logs"),
		ExtraOutFor: reg.WriterFor,
	})
	reg.SetController(mgr)

	srv := daemon.NewServer(daemon.Options{
		Manager:    mgr,
		Registry:   reg,
		SocketPath: socket,
		ConfigPath: configPath,
		Version:    buildinfo.Version,
	})
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

func TestDescribeShowsLastIntentChange(t *testing.T) {
	socket := bootIntentDaemon(t)
	c := dialTest(t, socket)

	// First boot, no verb has run: there is no change to show.
	h, err := c.Describe("demo")
	if err != nil {
		t.Fatal(err)
	}
	if h.LastIntentAt != "" || h.LastIntentSource != "" {
		t.Fatalf("fresh describe shows a last intent change: %+v", h)
	}

	if _, err := callLifecycle(c, "start", "demo", ""); err != nil {
		t.Fatal(err)
	}
	// Scaled by testwait.Budget, not the shared 5s: this test boots a whole
	// daemon and races the whole package, and a loaded CI runner starved both
	// the 5s wait (#835 review, CI run 15822) and a fixed 30s (CI run on
	// 10/01). The budget is a hang guard, not a correctness window.
	waitIntentRunning(t, c, "demo", testwait.Budget(t, 30*time.Second))
	h, err = c.Describe("demo")
	if err != nil {
		t.Fatal(err)
	}
	if h.RestartCount != 0 {
		t.Fatalf("demo restarted %d time(s) after start; it must stay running (last exit %d)", h.RestartCount, h.LastExitCode)
	}
	if h.LastIntentSource != "verb:start" || h.LastIntentAt == "" {
		t.Fatalf("after start, last intent = %q @ %q, want verb:start with a time", h.LastIntentSource, h.LastIntentAt)
	}
	// The verb came over the unix socket, so where the platform reports the
	// peer the daemon must carry it — and where it does not, there is none to
	// carry. Keyed on the daemon's own flag, not GOOS, so this tracks
	// whichever platforms have an implementation.
	if (h.LastIntentPeer != "") != daemon.ReportsPeerCredentials {
		t.Fatalf("socket verb peer = %q, want one iff the platform reports peers (%v)", h.LastIntentPeer, daemon.ReportsPeerCredentials)
	}

	if _, err := callLifecycle(c, "stop", "demo", ""); err != nil {
		t.Fatal(err)
	}
	waitIntentState(t, c, "demo", "stopped", testwait.Budget(t, 30*time.Second))
	h, err = c.Describe("demo")
	if err != nil {
		t.Fatal(err)
	}
	if h.LastIntentSource != "verb:stop" || (h.LastIntentPeer != "") != daemon.ReportsPeerCredentials {
		t.Fatalf("after stop, last intent = %q @ %q peer %q, want verb:stop with a peer iff the platform reports peers (%v)", h.LastIntentSource, h.LastIntentAt, h.LastIntentPeer, daemon.ReportsPeerCredentials)
	}

	// The rendered describe shows the row, not just the wire fields.
	out, err := captureStdout(t, func() error {
		return withClient(verbOpts{socket: socket, name: "demo"}, nil, cmdDescribe)
	})
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	// The label column truncates long labels ("last intent…"); the value
	// cell is what must stay readable.
	if !strings.Contains(out, "last intent") || !strings.Contains(out, "verb:stop") {
		t.Fatalf("describe does not show the last intent change and its source:\n%s", out)
	}
}

// waitIntentRunning polls until the harness reports running, with the given
// deadline (the shared helper's 5s is too tight on a loaded runner).
func waitIntentRunning(t *testing.T, c *client.Client, name string, within time.Duration) {
	waitIntentState(t, c, name, "running", within)
}

func waitIntentState(t *testing.T, c *client.Client, name, state string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	var last protocol.HarnessInfo
	var lastErr error
	for time.Now().Before(deadline) {
		h, err := c.Describe(name)
		if err == nil && h.State == state {
			return
		}
		last, lastErr = h, err
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("%s never reached %s within %v (last: %+v, err %v)", name, state, within, last, lastErr)
}

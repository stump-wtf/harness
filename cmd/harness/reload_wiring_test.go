package main

// The reload wiring test: a REAL config file, parsed by the real parser, and
// reloaded through the real Manager.ReloadFromFile.
//
// It exists because the property under test is about a signal that arrives
// periodically and means nothing — chezmoi (and czu on its timer) rewrite
// harness.toml byte-identically — and the failure mode is a listener that
// reconnects every time. A test that called Reconcile directly would prove
// the reconciler works; only this one proves the daemon's reload path reaches
// it at all, and that the scheduler's hook was not clobbered on the way.
//
// Governing: ADR-0021; SPEC-0014 REQ "Source Reconciliation On Reload".
//
// @joestump 09/23/2026 - Introduced with SPEC-0014 channel reconnection (#474).

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/attach"
	"github.com/stump-wtf/harness/internal/config"
	"github.com/stump-wtf/harness/internal/supervisor"
	"github.com/stump-wtf/harness/internal/trigger"
	"github.com/stump-wtf/harness/internal/trigger/channel/testserver"
)

// reloadFixture writes a harness.toml naming srv as a channel source, with
// token as the resolved Authorization header.
func reloadFixture(t *testing.T, dir, url, token string) string {
	t.Helper()
	envPath := filepath.Join(dir, "sb.env")
	if err := os.WriteFile(envPath, []byte("SB_TOKEN="+token+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	promptPath := filepath.Join(dir, "review.md")
	if err := os.WriteFile(promptPath, []byte("review the pull request\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`
[channel.sb]
url = %q
env_file = "sb.env"
headers = { Authorization = "Bearer ${SB_TOKEN}" }

[harness.pr-review]
harness = "claude-code"
prompt_file = %q
triggers = ["channel.sb"]
`, url, promptPath)
	path := filepath.Join(dir, "harness.toml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDaemonWiringReloadHoldsAndRotatesAChannelSession(t *testing.T) {
	srv := testserver.New(testserver.Options{})
	t.Cleanup(srv.Close)

	dir := t.TempDir()
	cfgPath := reloadFixture(t, dir, srv.URL, "old-token")
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatalf("the fixture does not parse: %v", err)
	}

	reg := attach.NewRegistry(100)
	opts := daemonManagerOptions(reg)
	opts.StatePath = filepath.Join(dir, "state.json")
	opts.LogDir = filepath.Join(dir, "logs")
	opts.JobsDir = filepath.Join(dir, "jobs")
	opts.Policy.StopGrace = 200 * time.Millisecond

	mgr := supervisor.NewManager(cfg, opts)
	t.Cleanup(mgr.Close)
	reg.SetController(mgr)
	if err := mgr.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	sources := startDaemonSources(mgr)
	t.Cleanup(sources.Close)

	// A stand-in for the scheduler's hook, so the test also proves
	// wireSourceReload COMPOSES rather than replaces. Without this the
	// daemon's schedules would silently stop being re-applied on reload.
	schedulerRan := 0
	wireSourceReload(mgr, sources, func() { schedulerRan++ })

	waitConnected := func(what string) {
		t.Helper()
		deadline := time.Now().Add(10 * time.Second)
		for {
			st, ok := sources.StatusOf("channel.sb")
			if ok && st.State == trigger.StateConnected {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s: the source never connected: %+v", what, sources.Status())
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitConnected("initial connect")
	before := initializeCount(srv)

	// Five byte-identical rewrites, the chezmoi shape. Rewriting the file
	// rather than re-using the parsed config matters: it is the real path,
	// and config.Load builds a fresh *core.Config every time — so a
	// reconciler comparing pointers rather than identity would fail here.
	for i := 0; i < 5; i++ {
		reloadFixture(t, dir, srv.URL, "old-token")
		if err := mgr.ReloadFromFile(cfgPath); err != nil {
			t.Fatalf("reload %d: %v", i+1, err)
		}
	}
	if schedulerRan != 5 {
		t.Errorf("the scheduler's reload hook ran %d times, want 5: wireSourceReload must compose, not replace", schedulerRan)
	}
	if got := initializeCount(srv); got != before {
		t.Errorf("no-change reloads re-initialized the session %d times", got-before)
	}
	if st, _ := sources.StatusOf("channel.sb"); st.State != trigger.StateConnected {
		t.Errorf("state after the rewrites = %q, want connected throughout", st.State)
	}

	// Now rotate the credential the source resolves. The identity changed,
	// so this one MUST reconnect — and with the new header on the wire.
	reloadFixture(t, dir, srv.URL, "new-token")
	if err := mgr.ReloadFromFile(cfgPath); err != nil {
		t.Fatalf("reload after rotation: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for initializeCount(srv) == before {
		if time.Now().After(deadline) {
			t.Fatal("a rotated token did not rebuild the session")
		}
		time.Sleep(10 * time.Millisecond)
	}
	waitConnected("after rotation")

	sawNew := false
	for _, r := range srv.Requests() {
		if r.Header.Get("Authorization") == "Bearer new-token" {
			sawNew = true
		}
	}
	if !sawNew {
		t.Error("the rotated token never reached the server")
	}
	// And the old one is not still in use: the point of rotating is that the
	// daemon stops presenting a credential the operator revoked.
	var lastAuth string
	for _, r := range srv.Requests() {
		if a := r.Header.Get("Authorization"); a != "" {
			lastAuth = a
		}
	}
	if !strings.Contains(lastAuth, "new-token") {
		t.Errorf("the most recent request carried %q, want the rotated token", lastAuth)
	}
}

// initializeCount is how many sessions the server has been asked to open.
func initializeCount(srv *testserver.Server) int {
	n := 0
	for _, m := range srv.RPCMethods() {
		if m == "initialize" {
			n++
		}
	}
	return n
}

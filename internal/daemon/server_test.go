package daemon

// Governing: SPEC-0002 — the scenarios for REQ "Handshake And Versioning",
// "Control Operations", "Message Framing", "Event Subscription", and the
// last-good reload behaviour (ADR-0006), all exercised over a real Unix socket
// with a real supervised process. Runs under -race (goroutines + sockets +
// channels).

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/attach"
	"gitea.stump.rocks/stump.wtf/harness/internal/client"
	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/supervisor"
)

// testDaemon is a running daemon on a private socket for one test.
type testDaemon struct {
	socket     string
	configPath string
	mgr        *supervisor.Manager
	srv        *Server
}

// newTestDaemon writes tomlBody to a config file, boots a Manager (temp state/
// log dirs) + attach Registry + Server, and serves on a short-path socket. It
// registers cleanup. It does NOT autostart (tests drive lifecycle explicitly).
func newTestDaemon(t *testing.T, tomlBody string) *testDaemon {
	t.Helper()
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "harnessd.toml")
	if err := os.WriteFile(configPath, []byte(tomlBody), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	// Unix socket paths are length-limited (~108 bytes); the test tempdir can be
	// long, so put the socket under a short /tmp dir.
	sockDir, err := os.MkdirTemp("/tmp", "hnd")
	if err != nil {
		t.Fatalf("sock dir: %v", err)
	}
	socket := filepath.Join(sockDir, "d.sock")

	reg := attach.NewRegistry(1000)
	mgr := supervisor.NewManager(cfg, supervisor.ManagerOptions{
		StatePath:   filepath.Join(tmp, "state.json"),
		LogDir:      filepath.Join(tmp, "logs"),
		ExtraOutFor: reg.WriterFor,
	})
	reg.SetController(mgr)

	srv := NewServer(Options{
		Manager:    mgr,
		Registry:   reg,
		SocketPath: socket,
		ConfigPath: configPath,
		Version:    "test",
	})
	if err := srv.Listen(); err != nil {
		t.Fatalf("listen: %v", err)
	}
	go srv.Serve()

	td := &testDaemon{socket: socket, configPath: configPath, mgr: mgr, srv: srv}
	t.Cleanup(func() {
		srv.Close()
		mgr.Close()
		_ = os.RemoveAll(sockDir)
	})
	return td
}

func (td *testDaemon) dial(t *testing.T, wants []string) *client.Client {
	t.Helper()
	c, err := client.Dial(td.socket, "test-client", wants)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

const sleeperTOML = `
[harness.sleeper]
cmd = "sleep"
args = ["60"]
description = "long runner"
`

// TestHandshakeVersionMismatch is SPEC-0002 REQ "Handshake And Versioning"
// scenario "Old client, upgraded daemon": a mismatched proto major gets a
// structured version-mismatch ERROR and a clean close.
func TestHandshakeVersionMismatch(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML)

	raw, err := net.Dial("unix", td.socket)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer raw.Close()
	pc := protocol.NewConn(raw)
	// A client two majors ahead.
	if err := pc.WriteJSON(protocol.TypeHello, &protocol.Hello{ProtoVersion: "99.0", ClientVersion: "x"}); err != nil {
		t.Fatalf("write hello: %v", err)
	}
	f, err := pc.ReadFrame()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if f.Type != protocol.TypeError {
		t.Fatalf("frame type = %s, want ERROR", f.Type)
	}
	em := errorFrom(t, f.Payload)
	if em.Code != protocol.ErrVersionMismatch {
		t.Errorf("error code = %s, want %s", em.Code, protocol.ErrVersionMismatch)
	}
	// The daemon closes cleanly: the next read is EOF.
	_ = raw.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := pc.ReadFrame(); err == nil {
		t.Error("expected connection close after version mismatch")
	}
}

// TestListGlyphs is SPEC-0002 REQ "Control Operations" (list) + the acceptance
// criterion that states map to SPEC-0003 glyphs.
func TestListGlyphs(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML)
	c := td.dial(t, nil)
	hs, err := c.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(hs) != 1 || hs[0].Name != "sleeper" {
		t.Fatalf("List = %+v, want one 'sleeper'", hs)
	}
	if hs[0].State != string(core.StateStopped) {
		t.Errorf("state = %s, want stopped", hs[0].State)
	}
	// Every reported state must render a SPEC-0003 glyph.
	if !core.State(hs[0].State).Valid() || core.State(hs[0].State).Glyph() == "" {
		t.Errorf("state %q has no SPEC-0003 glyph", hs[0].State)
	}
	if hs[0].Cmd != "sleep" {
		t.Errorf("cmd = %q, want sleep", hs[0].Cmd)
	}
}

// TestIdempotentStart is SPEC-0002 REQ "Control Operations" scenario
// "Idempotent start": starting an already-running harness succeeds without
// disturbing the process.
func TestIdempotentStart(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML)
	c := td.dial(t, nil)

	first, err := c.Start("sleeper")
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if first.State != string(core.StateRunning) {
		t.Fatalf("after start state = %s, want running", first.State)
	}
	pid := first.PID

	second, err := c.Start("sleeper")
	if err != nil {
		t.Fatalf("second Start (should be a no-op success): %v", err)
	}
	if second.State != string(core.StateRunning) {
		t.Errorf("after double start state = %s, want running", second.State)
	}
	if pid != 0 && second.PID != pid {
		t.Errorf("double start disturbed the process: pid %d → %d", pid, second.PID)
	}
}

// TestUnknownHarnessStructuredError is SPEC-0002 REQ "Control Operations"
// scenario "Structured failure".
func TestUnknownHarnessStructuredError(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML)
	c := td.dial(t, nil)
	_, err := c.Describe("does-not-exist")
	if err == nil {
		t.Fatal("want error for unknown harness")
	}
	em, ok := err.(*protocol.ErrorMsg)
	if !ok {
		t.Fatalf("error type = %T, want *protocol.ErrorMsg", err)
	}
	if em.Code != protocol.ErrUnknownHarness {
		t.Errorf("code = %s, want %s", em.Code, protocol.ErrUnknownHarness)
	}
	if em.Message == "" {
		t.Error("want a human-readable message")
	}
}

// TestMixedTrafficOneConnection is SPEC-0002 REQ "Message Framing" scenario
// "Mixed traffic on one connection": a control request issued while an attach
// session is open flows without corrupting either stream.
func TestMixedTrafficOneConnection(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML)
	c := td.dial(t, nil)
	if _, err := c.Start("sleeper"); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Open an attach session; the daemon will stream snapshot/live ATTACH_DATA.
	if err := c.AttachOpen(1, "sleeper", 80, 24, protocol.AttachRW); err != nil {
		t.Fatalf("attach open: %v", err)
	}
	// While the attach stream is live, a control request still round-trips
	// (client.call transparently steps over interleaved attach frames).
	info, err := c.Describe("sleeper")
	if err != nil {
		t.Fatalf("control request during attach: %v", err)
	}
	if info.Name != "sleeper" {
		t.Errorf("describe during attach = %+v", info)
	}
}

// TestEventSubscription is SPEC-0002 REQ "Event Subscription" scenario "Reactive
// dashboard": a subscribed client receives harness_state_changed and
// harness_exited when a harness exits, without issuing any request itself.
func TestEventSubscription(t *testing.T) {
	const blipTOML = `
[harness.blip]
cmd = "sh"
args = ["-c", "exit 0"]
restart_delay = 60
`
	td := newTestDaemon(t, blipTOML)

	// The subscriber only listens — it never issues a control request.
	sub := td.dial(t, []string{"events"})

	// A separate control client triggers the lifecycle.
	ctl := td.dial(t, nil)
	if _, err := ctl.Start("blip"); err != nil {
		t.Fatalf("start blip: %v", err)
	}

	pc := sub.Conn()
	_ = sub.SetReadDeadline(time.Now().Add(5 * time.Second))
	sawExited, sawStateChange := false, false
	for !sawExited || !sawStateChange {
		f, err := pc.ReadFrame()
		if err != nil {
			t.Fatalf("subscriber read (saw exited=%v stateChange=%v): %v", sawExited, sawStateChange, err)
		}
		switch f.Type {
		case protocol.TypeEvent:
			ev := decodeEvent(t, f.Payload)
			if ev.Name != "blip" {
				continue
			}
			switch ev.Kind {
			case protocol.EvExited:
				sawExited = true
			case protocol.EvStateChanged:
				sawStateChange = true
			}
		case protocol.TypePing:
			_ = pc.WriteFrame(protocol.TypePong, nil)
		}
	}
}

// --- test helpers ---

func errorFrom(t *testing.T, payload []byte) *protocol.ErrorMsg {
	t.Helper()
	em := &protocol.ErrorMsg{}
	if err := json.Unmarshal(payload, em); err != nil {
		t.Fatalf("decode error frame: %v", err)
	}
	return em
}

func decodeEvent(t *testing.T, payload []byte) protocol.EventMsg {
	t.Helper()
	var ev protocol.EventMsg
	if err := json.Unmarshal(payload, &ev); err != nil {
		t.Fatalf("decode event frame: %v", err)
	}
	return ev
}

// TestReloadKeepsLastGood is ADR-0006 / SPEC-0002 "reload": a parse error comes
// back as a structured reload_failed ERROR and the daemon keeps serving its
// last-good config.
func TestReloadKeepsLastGood(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML)
	c := td.dial(t, nil)

	// Corrupt the config file, then reload.
	if err := os.WriteFile(td.configPath, []byte("this is not = valid = toml ["), 0o600); err != nil {
		t.Fatalf("corrupt config: %v", err)
	}
	_, err := c.Reload()
	if err == nil {
		t.Fatal("want reload error on invalid config")
	}
	em, ok := err.(*protocol.ErrorMsg)
	if !ok || em.Code != protocol.ErrReload {
		t.Fatalf("error = %v, want reload_failed", err)
	}
	// Last-good config is still served.
	hs, err := c.List()
	if err != nil {
		t.Fatalf("List after failed reload: %v", err)
	}
	if len(hs) != 1 || hs[0].Name != "sleeper" {
		t.Errorf("last-good config lost: %+v", hs)
	}
}

// TestDaemonInfo is SPEC-0002 REQ "Control Operations" (daemon_info).
func TestDaemonInfo(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML)
	c := td.dial(t, nil)
	di, err := c.DaemonInfo()
	if err != nil {
		t.Fatalf("DaemonInfo: %v", err)
	}
	if di.ProtoVersion != protocol.ProtoVersion {
		t.Errorf("proto = %s, want %s", di.ProtoVersion, protocol.ProtoVersion)
	}
	if di.Version != "test" {
		t.Errorf("version = %s, want test", di.Version)
	}
	if di.Harnesses != 1 {
		t.Errorf("harnesses = %d, want 1", di.Harnesses)
	}
}

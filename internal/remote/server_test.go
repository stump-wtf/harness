package remote

// Governing: SPEC-0002 REQ "Transport Bindings" (scenario "Remote parity"),
// ADR-0004 (Wish data plane hosts the same TUI), ADR-0008 (SSH pubkey auth,
// persistent host key 0600 under a 0700 dir). These run under -race: the SSH
// server, the daemon, and the client each own goroutines + sockets.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	gossh "golang.org/x/crypto/ssh"

	"gitea.stump.rocks/stump.wtf/harness/internal/attach"
	"gitea.stump.rocks/stump.wtf/harness/internal/config"
	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/daemon"
	"gitea.stump.rocks/stump.wtf/harness/internal/supervisor"
)

// clientKey mints an ed25519 signer plus its authorized_keys line for a test.
func clientKey(t *testing.T) (gossh.Signer, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("gen: %v", err)
	}
	signer, err := gossh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	sshPub, err := gossh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("pub: %v", err)
	}
	return signer, string(gossh.MarshalAuthorizedKey(sshPub))
}

// bootDaemon starts a real daemon on a private Unix socket (no harnesses) so a
// remote session has a live protocol endpoint to be a local client of. It
// returns the socket path and the live *daemon.Server so tests can observe its
// connection count.
func bootDaemon(t *testing.T) (string, *daemon.Server) {
	t.Helper()
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, "harnessd.toml")
	if err := os.WriteFile(configPath, []byte("# empty\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := config.Load(configPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	sockDir, err := os.MkdirTemp("/tmp", "hnd-remote")
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
	srv := daemon.NewServer(daemon.Options{
		Manager:    mgr,
		Registry:   reg,
		SocketPath: socket,
		ConfigPath: configPath,
		Version:    "test",
	})
	if err := srv.Listen(); err != nil {
		t.Fatalf("daemon listen: %v", err)
	}
	go srv.Serve()
	t.Cleanup(func() {
		srv.Close()
		mgr.Close()
		_ = os.RemoveAll(sockDir)
	})
	return socket, srv
}

// startServer builds a remote.Server bound to an ephemeral loopback port and
// serving in the background. Returns the address.
func startServer(t *testing.T, opts Options) *Server {
	t.Helper()
	s, err := New(opts)
	if err != nil {
		t.Fatalf("remote.New: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s.addr = ln.Addr().String()
	go func() { _ = s.ServeListener(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = s.Shutdown(ctx)
		cancel()
	})
	return s
}

// TestNewRefusesEmptyAllowlist: an enabled server with no keys is never allowed
// (ADR-0008 — no unauthenticated remote access).
func TestNewRefusesEmptyAllowlist(t *testing.T) {
	_, err := New(Options{
		Socket:      "/tmp/nope.sock",
		HostKeyPath: filepath.Join(t.TempDir(), "hostkey"),
	})
	if err == nil {
		t.Fatal("expected New to refuse an empty allowlist")
	}
}

// TestHostKeyPersistedPerms: the host key is generated on first run at 0600
// under a 0700 dir (ADR-0008), and reused (stable identity) on the next New.
func TestHostKeyPersistedPerms(t *testing.T) {
	_, authLine := clientKey(t)
	dir := filepath.Join(t.TempDir(), "state")
	hostKey := filepath.Join(dir, "ssh_host_ed25519_key")

	_, err := New(Options{
		Socket:      "/tmp/nope.sock",
		HostKeyPath: hostKey,
		Keys:        []core.AuthorizedKey{{Line: authLine}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	di, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := di.Mode().Perm(); perm != 0o700 {
		t.Errorf("host key dir perm = %o, want 0700", perm)
	}
	ki, err := os.Stat(hostKey)
	if err != nil {
		t.Fatalf("stat host key: %v", err)
	}
	if perm := ki.Mode().Perm(); perm != 0o600 {
		t.Errorf("host key perm = %o, want 0600", perm)
	}

	before, _ := os.ReadFile(hostKey)
	if _, err := New(Options{
		Socket:      "/tmp/nope.sock",
		HostKeyPath: hostKey,
		Keys:        []core.AuthorizedKey{{Line: authLine}},
	}); err != nil {
		t.Fatalf("second New: %v", err)
	}
	after, _ := os.ReadFile(hostKey)
	if string(before) != string(after) {
		t.Error("host key should be stable across restarts, not regenerated")
	}
}

// TestSSHAuthorizedLandsInTUI is SPEC-0002 scenario "Remote parity": an
// authorized key gets an SSH+PTY session that hosts the TUI (bytes flow), while
// an unlisted key is refused at auth.
func TestSSHAuthorizedLandsInTUI(t *testing.T) {
	socket, _ := bootDaemon(t)
	signer, authLine := clientKey(t)
	strangerSigner, _ := clientKey(t)

	s := startServer(t, Options{
		Socket:      socket,
		ConfigPath:  "",
		Version:     "test",
		HostKeyPath: filepath.Join(t.TempDir(), "hostkey"),
		Keys:        []core.AuthorizedKey{{Line: authLine}},
	})

	// Unauthorized key: SSH auth must fail.
	if _, err := gossh.Dial("tcp", s.Addr(), &gossh.ClientConfig{
		User:            "joe",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(strangerSigner)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	}); err == nil {
		t.Fatal("unauthorized key should be refused")
	}

	// Authorized key: connect, request a PTY + shell, expect TUI output.
	conn, err := gossh.Dial("tcp", s.Addr(), &gossh.ClientConfig{
		User:            "joe",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	})
	if err != nil {
		t.Fatalf("authorized dial: %v", err)
	}
	defer conn.Close()

	sess, err := conn.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()

	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := sess.RequestPty("xterm-256color", 40, 120, gossh.TerminalModes{}); err != nil {
		t.Fatalf("request pty: %v", err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}

	// Read some bytes: the TUI enters the alt-screen and paints, so any output
	// proves the session landed in the in-process Bubble Tea program.
	got := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 512)
		n, _ := stdout.Read(buf)
		got <- buf[:n]
	}()
	select {
	case b := <-got:
		if len(b) == 0 {
			t.Fatal("remote session produced no TUI output")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for TUI output over SSH")
	}
}

// TestRemoteSessionReleasesDaemonConns guards the lifecycle: an SSH session that
// lands in the TUI opens two daemon connections (control + events/attach); when
// the session closes, both must be reaped. Bubble Tea returns on QuitMsg without
// running Update, so the hosting middleware must call Model.Close() after
// Program.Run — otherwise every remote attach/detach leaks two socket
// connections and the read-loop goroutine for the life of the daemon.
func TestRemoteSessionReleasesDaemonConns(t *testing.T) {
	socket, daemonSrv := bootDaemon(t)
	signer, authLine := clientKey(t)

	s := startServer(t, Options{
		Socket:      socket,
		Version:     "test",
		HostKeyPath: filepath.Join(t.TempDir(), "hostkey"),
		Keys:        []core.AuthorizedKey{{Line: authLine}},
	})

	base := daemonSrv.ConnCount()

	conn, err := gossh.Dial("tcp", s.Addr(), &gossh.ClientConfig{
		User:            "joe",
		Auth:            []gossh.AuthMethod{gossh.PublicKeys(signer)},
		HostKeyCallback: gossh.InsecureIgnoreHostKey(),
		Timeout:         3 * time.Second,
	})
	if err != nil {
		t.Fatalf("authorized dial: %v", err)
	}
	sess, err := conn.NewSession()
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout pipe: %v", err)
	}
	if err := sess.RequestPty("xterm-256color", 40, 120, gossh.TerminalModes{}); err != nil {
		t.Fatalf("request pty: %v", err)
	}
	if err := sess.Shell(); err != nil {
		t.Fatalf("shell: %v", err)
	}

	// Wait until the in-process TUI has dialed the daemon (two connections above
	// baseline: control + events/attach) so we know the session truly landed.
	if !waitForConns(daemonSrv, base+2, 5*time.Second) {
		// Read some output to surface why it never connected, then fail.
		go func() { io.Copy(io.Discard, stdout) }()
		t.Fatalf("TUI never opened its daemon connections: ConnCount=%d want %d", daemonSrv.ConnCount(), base+2)
	}

	// Close the SSH connection: the hosting program must quit AND release the
	// TUI's daemon connections.
	_ = sess.Close()
	_ = conn.Close()

	if !waitForConns(daemonSrv, base, 5*time.Second) {
		t.Fatalf("daemon connections leaked after SSH session closed: ConnCount=%d want %d", daemonSrv.ConnCount(), base)
	}
}

// waitForConns polls the daemon's live connection count until it equals want or
// the timeout elapses.
func waitForConns(s *daemon.Server, want int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s.ConnCount() == want {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return s.ConnCount() == want
}

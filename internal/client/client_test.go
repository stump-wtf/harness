package client

// Dial Failure Shapes
//
// A client that cannot reach the daemon has to say WHICH failure it hit: no
// socket, a socket nobody is listening on, or a daemon that took the
// connection and never answered. The CLI's error text branches on these, and
// issue #578 is what the single "daemon not running" message cost when the
// third case was live and supervising the whole time.
//
// @joestump 09/22/2026 - Added with the fix for #578.

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// shortSocket returns a socket path short enough for sun_path.
func shortSocket(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hcd")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

// TestDialSilentDaemonIsNotAMissingDaemon: something accepted the connection,
// so a daemon is bound to that socket. Dial must report that distinctly from
// "nothing is there", and carry the socket path so the CLI can name it.
func TestDialSilentDaemonIsNotAMissingDaemon(t *testing.T) {
	socket := shortSocket(t, "silent.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	// Accept and hang up without a HELLO — a daemon that is bound but not
	// answering the protocol.
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	_, err = Dial(socket, "test-client", nil)
	if err == nil {
		t.Fatal("Dial succeeded against a daemon that never sent HELLO")
	}
	var silent *NoHandshakeError
	if !errors.As(err, &silent) {
		t.Fatalf("Dial error = %v (%T), want a *NoHandshakeError", err, err)
	}
	if silent.Socket != socket {
		t.Errorf("NoHandshakeError.Socket = %q, want %q", silent.Socket, socket)
	}
	if !strings.Contains(err.Error(), "did not answer the handshake") {
		t.Errorf("Dial error = %q, want it to say the daemon did not answer", err)
	}
}

// TestDialMissingSocketStaysAPathError: the missing-socket case must keep
// wrapping fs.ErrNotExist, which is what the CLI branches on to say "the
// socket is gone" instead of "the daemon is down".
func TestDialMissingSocketStaysAPathError(t *testing.T) {
	socket := shortSocket(t, "nope.sock")
	_, err := Dial(socket, "test-client", nil)
	if err == nil {
		t.Fatal("Dial succeeded against a socket that does not exist")
	}
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("Dial error = %v, want it to wrap fs.ErrNotExist", err)
	}
	var silent *NoHandshakeError
	if errors.As(err, &silent) {
		t.Error("a missing socket must not be reported as a silent daemon")
	}
}

// TestDialTimesOutOnADaemonThatNeverAnswers is the wedged case the typed error
// exists for: the kernel accepts the connection into the backlog and nothing
// ever reads it. Without a handshake deadline Dial blocks forever, so every
// CLI call hangs and the "not answering" message is unreachable for exactly
// the daemon it describes.
func TestDialTimesOutOnADaemonThatNeverAnswers(t *testing.T) {
	socket := shortSocket(t, "wedged.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Accept and hold every connection open without a word. The accept loop
	// owns the held connections and closes them once the listener closes.
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		var held []net.Conn
		defer func() {
			for _, c := range held {
				_ = c.Close()
			}
		}()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			held = append(held, conn)
		}
	}()
	defer func() {
		_ = ln.Close()
		<-acceptDone
	}()

	prev := HandshakeTimeout
	HandshakeTimeout = 200 * time.Millisecond
	defer func() { HandshakeTimeout = prev }()

	done := make(chan error, 1)
	go func() {
		_, err := Dial(socket, "test-client", nil)
		done <- err
	}()
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Dial is still blocked on a daemon that never answers; the handshake has no deadline")
	}
	var silent *NoHandshakeError
	if !errors.As(err, &silent) {
		t.Fatalf("Dial error = %v (%T), want a *NoHandshakeError", err, err)
	}
	if !silent.TimedOut() {
		t.Errorf("NoHandshakeError.Err = %v, want a timeout", silent.Err)
	}
	if silent.Socket != socket {
		t.Errorf("NoHandshakeError.Socket = %q, want %q", silent.Socket, socket)
	}
}

// TestDialHangUpIsNotATimeout keeps the two shapes apart: a peer that hangs up
// (what a daemon does while shutting down) must not read as one holding the
// connection in silence.
func TestDialHangUpIsNotATimeout(t *testing.T) {
	socket := shortSocket(t, "hangup.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			_ = conn.Close()
		}
	}()
	_, err = Dial(socket, "test-client", nil)
	var silent *NoHandshakeError
	if !errors.As(err, &silent) {
		t.Fatalf("Dial error = %v (%T), want a *NoHandshakeError", err, err)
	}
	if silent.TimedOut() {
		t.Errorf("a hang-up was reported as a timeout: %v", silent.Err)
	}
}

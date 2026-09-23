package daemon

// Socket Lifecycle Tests
//
// Issue #578: on tars the control socket vanished while the daemon was alive
// and supervising, and every client reported "daemon not running" for twenty
// minutes. These tests pin the two halves of the invariant that failure broke
// — nobody unlinks a socket a live daemon answers on, and a daemon whose
// socket disappears gets it back — and they assert the property (a client can
// reach the daemon) rather than a proxy for it (a file exists).
//
// @joestump 09/22/2026 - Added with the fix.

import (
	"bytes"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/protocol"
)

// sleepingWatchdog keeps the watchdog from firing during tests that drive
// reassertSocket by hand.
func sleepingWatchdog(o *Options) { o.SocketWatchInterval = time.Hour }

// mustReach dials the daemon and makes one real control call, which is the
// only thing that proves the socket on disk belongs to a serving daemon.
func mustReach(t *testing.T, socket, what string) {
	t.Helper()
	c, err := client.Dial(socket, "test-client", nil)
	if err != nil {
		t.Fatalf("%s: dial %s: %v", what, socket, err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.List(); err != nil {
		t.Fatalf("%s: list over %s: %v", what, socket, err)
	}
}

// shortSocketDir returns a directory short enough for a Unix socket path.
func shortSocketDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hsk")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// TestSecondDaemonRefusesToUnlinkALiveSocket is the incident's prime suspect:
// a second `harness daemon` that clears a "stale" socket before binding takes
// the live daemon off the air without touching the live daemon at all.
func TestSecondDaemonRefusesToUnlinkALiveSocket(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML, sleepingWatchdog)
	mustReach(t, td.socket, "before the second daemon")

	before, err := os.Stat(td.socket)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}

	second := NewServer(Options{
		Manager:    td.mgr,
		Registry:   td.reg,
		SocketPath: td.socket,
		Version:    "second",
	})
	err = second.Listen()
	if err == nil {
		second.Close()
		t.Fatal("a second daemon bound the live daemon's socket; the first is now unreachable")
	}
	if !errors.Is(err, ErrSocketInUse) {
		t.Fatalf("Listen error = %v, want it to wrap ErrSocketInUse", err)
	}
	// The refusal must name who is holding it: on tars the operator had no way
	// to tell which process to look at.
	if want := fmt.Sprintf("pid %d", os.Getpid()); !strings.Contains(err.Error(), want) {
		t.Errorf("Listen error = %q, want it to name the live daemon (%s)", err, want)
	}

	after, err := os.Stat(td.socket)
	if err != nil {
		t.Fatalf("the live daemon's socket was removed: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Error("the socket file was replaced; the live daemon is listening on an unlinked inode")
	}
	mustReach(t, td.socket, "after the second daemon was refused")
}

// TestListenClearsAStaleSocket is the other side of the same coin: a socket
// left behind by a crashed daemon must not block the next one. Without this
// the refusal above would turn every hard kill into a daemon that cannot
// restart.
func TestListenClearsAStaleSocket(t *testing.T) {
	socket := filepath.Join(shortSocketDir(t), "stale.sock")
	ln, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Leave the file behind the way a SIGKILLed daemon does.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(socket); err != nil {
		t.Fatalf("stale socket file should still exist: %v", err)
	}

	srv := NewServer(Options{SocketPath: socket, Version: "test"})
	if err := srv.Listen(); err != nil {
		t.Fatalf("Listen over a stale socket: %v", err)
	}
	srv.Close()
}

// TestListenRefusesANonSocketPath: a misconfigured --socket used to delete
// whatever was at that path before binding over it.
func TestListenRefusesANonSocketPath(t *testing.T) {
	path := filepath.Join(shortSocketDir(t), "not-a-socket")
	if err := os.WriteFile(path, []byte("important"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	srv := NewServer(Options{SocketPath: path, Version: "test"})
	err := srv.Listen()
	if err == nil {
		srv.Close()
		t.Fatal("Listen bound over a regular file")
	}
	if !strings.Contains(err.Error(), "not a socket") {
		t.Errorf("Listen error = %v, want it to say the path is not a socket", err)
	}
	body, err := os.ReadFile(path)
	if err != nil || string(body) != "important" {
		t.Errorf("the file at the socket path was destroyed (body=%q err=%v)", body, err)
	}
}

// TestDaemonRebindsASocketRemovedUnderIt drives the watchdog the way
// production does — through Serve's own ticker, so the wiring is under test
// and not just the check (the #315 lesson). The property is that a client
// works again, not that a file reappeared.
func TestDaemonRebindsASocketRemovedUnderIt(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML, func(o *Options) {
		o.SocketWatchInterval = 10 * time.Millisecond
	})
	mustReach(t, td.socket, "before the socket was removed")

	before, err := os.Stat(td.socket)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if err := os.Remove(td.socket); err != nil {
		t.Fatalf("remove socket: %v", err)
	}

	deadline := time.Now().Add(10 * time.Second)
	for {
		c, err := client.Dial(td.socket, "test-client", nil)
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("daemon never re-bound its socket: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}

	after, err := os.Stat(td.socket)
	if err != nil {
		t.Fatalf("stat re-bound socket: %v", err)
	}
	if os.SameFile(before, after) {
		t.Error("the socket file is the same inode as before; nothing was re-bound")
	}
	if got := after.Mode().Perm(); got != protocol.SocketMode {
		t.Errorf("re-bound socket mode = %o, want %o (ADR-0008)", got, protocol.SocketMode)
	}
	mustReach(t, td.socket, "after the daemon re-bound")
}

// TestReassertSocketIsLevelTriggered checks the watchdog pass itself, without
// a tick: it re-binds when the path is gone and does nothing at all when the
// path is healthy — a check that re-bound every pass would churn the socket
// once a second forever.
func TestReassertSocketIsLevelTriggered(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML, sleepingWatchdog)

	if rebound, err := td.srv.reassertSocket(); rebound || err != nil {
		t.Fatalf("healthy pass: rebound=%v err=%v, want false/nil", rebound, err)
	}
	if err := os.Remove(td.socket); err != nil {
		t.Fatalf("remove socket: %v", err)
	}
	rebound, err := td.srv.reassertSocket()
	if err != nil {
		t.Fatalf("reassertSocket after removal: %v", err)
	}
	if !rebound {
		t.Fatal("reassertSocket did not re-bind a socket that was gone")
	}
	mustReach(t, td.socket, "after reassertSocket re-bound")

	if rebound, err := td.srv.reassertSocket(); rebound || err != nil {
		t.Fatalf("second pass: rebound=%v err=%v, want false/nil", rebound, err)
	}
}

// TestWatchdogLeavesAnotherLiveListenerAlone: if something else has taken the
// path and answers there, re-binding would hand the socket back and forth once
// a second between two live daemons. Reporting it and standing down is the
// less-bad state.
func TestWatchdogLeavesAnotherLiveListenerAlone(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML, sleepingWatchdog)
	if err := os.Remove(td.socket); err != nil {
		t.Fatalf("remove socket: %v", err)
	}
	foreign, err := net.Listen("unix", td.socket)
	if err != nil {
		t.Fatalf("foreign listen: %v", err)
	}
	defer func() { _ = foreign.Close() }()
	foreignInfo, err := os.Stat(td.socket)
	if err != nil {
		t.Fatalf("stat foreign socket: %v", err)
	}

	rebound, err := td.srv.reassertSocket()
	if rebound {
		t.Error("stole the path back from a live listener")
	}
	if err == nil || !strings.Contains(err.Error(), "another live listener") {
		t.Errorf("reassertSocket err = %v, want it to report the foreign listener", err)
	}
	after, err := os.Stat(td.socket)
	if err != nil {
		t.Fatalf("stat after: %v", err)
	}
	if !os.SameFile(foreignInfo, after) {
		t.Error("the foreign listener's socket was replaced")
	}
}

// TestCloseDoesNotUnlinkAnotherListenersSocket is the startup foot-gun
// pointed the other way: daemon A shutting down used to unlink whatever was at
// the path, including daemon B's live socket.
func TestCloseDoesNotUnlinkAnotherListenersSocket(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML, sleepingWatchdog)
	if err := os.Remove(td.socket); err != nil {
		t.Fatalf("remove socket: %v", err)
	}
	foreign, err := net.Listen("unix", td.socket)
	if err != nil {
		t.Fatalf("foreign listen: %v", err)
	}
	defer func() { _ = foreign.Close() }()
	before, err := os.Stat(td.socket)
	if err != nil {
		t.Fatalf("stat foreign socket: %v", err)
	}

	td.srv.Close()

	after, err := os.Stat(td.socket)
	if err != nil {
		t.Fatalf("Close unlinked the other listener's socket: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Error("the other listener's socket was replaced during Close")
	}
}

// TestCloseUnlinksItsOwnSocket keeps the above from being satisfied by a Close
// that never cleans up at all.
func TestCloseUnlinksItsOwnSocket(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML, sleepingWatchdog)
	td.srv.Close()
	if _, err := os.Stat(td.socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat after Close = %v, want the socket to be gone", err)
	}
}

// TestListenRefusesWhenTheProbeIsInconclusive: only a refused dial proves a
// socket is stale. A live listener whose socket this process cannot connect
// to (EACCES here; EAGAIN from a full backlog on Linux is the same shape) used
// to read as "nobody is listening", and Listen deleted the live socket and
// bound over it — the #578 incident through a side door. Running as root the
// dial succeeds and the refusal comes from the probe answering instead; either
// way the live socket must survive.
func TestListenRefusesWhenTheProbeIsInconclusive(t *testing.T) {
	socket := filepath.Join(shortSocketDir(t), "locked.sock")
	live, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = live.Close() }()
	go func() {
		for {
			c, err := live.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	before, err := os.Stat(socket)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if err := os.Chmod(socket, 0); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	srv := NewServer(Options{SocketPath: socket, Version: "test"})
	if err := srv.Listen(); err == nil {
		srv.Close()
		t.Fatal("Listen deleted a live listener's socket on an inconclusive probe and bound over it")
	}
	after, err := os.Stat(socket)
	if err != nil {
		t.Fatalf("the live listener's socket is gone: %v", err)
	}
	if !os.SameFile(before, after) {
		t.Error("the live listener's socket was replaced")
	}
}

// TestReassertSocketAfterCloseDoesNothing: a watchdog tick that loses the race
// with Close finds the path gone — Close removed it — and must not read that
// as a vanished socket and bind a fresh listener nobody will ever accept on.
func TestReassertSocketAfterCloseDoesNothing(t *testing.T) {
	td := newTestDaemon(t, sleeperTOML, sleepingWatchdog)
	td.srv.Close()

	rebound, err := td.srv.reassertSocket()
	if rebound || err != nil {
		t.Errorf("reassertSocket after Close: rebound=%v err=%v, want false/nil", rebound, err)
	}
	if _, err := os.Lstat(td.socket); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat after the post-Close pass = %v, want no socket", err)
	}
}

// syncBuffer is a goroutine-safe log sink: other tests in this package log
// concurrently while one of these is installed.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestWatchdogWarnsOnceARebindWorks: a re-bind that keeps failing fails on
// every tick, and warning before the attempt put a WARN line in the log once a
// second, forever, next to watchSocket's single deduplicated error. The
// failure's reason travels in the returned error instead, and the warning is
// logged when the socket is actually back.
//
// A dangling symlink at the path makes the failure permanent for any user,
// root included: Stat follows it (ENOENT, so "vanished") while the bind
// refuses to remove something that is not a socket.
func TestWatchdogWarnsOnceARebindWorks(t *testing.T) {
	sink := &syncBuffer{}
	log.SetOutput(sink)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	td := newTestDaemon(t, sleeperTOML, sleepingWatchdog)
	if err := os.Remove(td.socket); err != nil {
		t.Fatalf("remove socket: %v", err)
	}
	if err := os.Symlink(filepath.Join(filepath.Dir(td.socket), "nowhere"), td.socket); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	warnings := func() int {
		n := 0
		for _, line := range strings.Split(sink.String(), "\n") {
			if strings.Contains(line, "WARN") && strings.Contains(line, td.socket) {
				n++
			}
		}
		return n
	}

	for i := 0; i < 3; i++ {
		rebound, err := td.srv.reassertSocket()
		if rebound || err == nil {
			t.Fatalf("pass %d over a path it cannot bind: rebound=%v err=%v, want an error", i, rebound, err)
		}
		if !strings.Contains(err.Error(), "vanished") {
			t.Errorf("pass %d error = %v, want it to carry why it tried to re-bind", i, err)
		}
	}
	if n := warnings(); n != 0 {
		t.Errorf("%d WARN lines from passes that re-bound nothing; want 0\n%s", n, sink.String())
	}

	if err := os.Remove(td.socket); err != nil {
		t.Fatalf("remove symlink: %v", err)
	}
	if rebound, err := td.srv.reassertSocket(); !rebound || err != nil {
		t.Fatalf("pass after the path cleared: rebound=%v err=%v, want true/nil", rebound, err)
	}
	if n := warnings(); n != 1 {
		t.Errorf("%d WARN lines after one successful re-bind; want 1\n%s", n, sink.String())
	}
	mustReach(t, td.socket, "after the re-bind")
}

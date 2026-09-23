package daemon

// Socket Lifecycle
//
// The control socket is the daemon's only interface, so the file at the socket
// path must keep naming THIS daemon's listener for as long as it serves. Two
// rules hold that invariant. On startup the daemon unlinks a socket only after
// proving nothing answers on it, so a second `harness daemon` cannot delete a
// live one's socket and take the box off the air. While serving, a watchdog
// re-asserts the path on a ticker: when the socket is gone — or has been
// replaced by one nobody answers on — it re-binds and says so in the log,
// because a live-but-unreachable daemon is indistinguishable from a dead one
// to every client.
//
// Governing: ADR-0004 (the Unix socket is the local transport), ADR-0008
// (socket mode 0600), SPEC-0002 REQ "Socket Lifecycle".
//
// @joestump 09/22/2026 - Added for issue #578: tars ran ~20 minutes with a
// live daemon and no socket. The supervisor kept restarting harnesses the
// whole time while every client reported "daemon not running".

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"strings"
	"syscall"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/protocol"
)

// ErrSocketInUse is what Listen returns when another daemon already answers on
// the socket path. It is a refusal rather than a bind failure: removing that
// socket to make room would take the running daemon off the air, which is
// exactly the incident this guards (#578).
var ErrSocketInUse = errors.New("another harness daemon is already listening")

// probeTimeout bounds both the dial and the two-frame exchange of a probe. It
// is short on purpose: a probe runs on the startup path and on the watchdog
// tick, and "did not answer in half a second" is not a reason to unlink
// anything — only a refused dial is.
const probeTimeout = 500 * time.Millisecond

// defaultSocketWatchInterval is how often a serving daemon re-asserts that the
// socket path still names its own listener. One stat per second: cheap enough
// to run forever, quick enough that an unreachable daemon is measured in
// seconds instead of however long it takes a human to notice.
const defaultSocketWatchInterval = time.Second

// socketProbe is what dialing an existing socket path told us. Answered is the
// load-bearing field: anything that accepts a connection is a live listener
// whose socket must not be unlinked, whether or not it goes on to speak the
// protocol. PID and Version are a courtesy for the error message.
type socketProbe struct {
	Answered bool
	PID      int
	Version  string
}

// describe renders the probe's identity for an operator-facing message.
func (p socketProbe) describe() string {
	switch {
	case p.PID > 0 && p.Version != "":
		return fmt.Sprintf("pid %d, version %s", p.PID, p.Version)
	case p.PID > 0:
		return fmt.Sprintf("pid %d", p.PID)
	default:
		return "pid unknown — it accepted the connection but did not answer daemon_info"
	}
}

// probeSocket dials path and asks whoever is there to identify itself.
//
// Only a refused dial (a socket file nobody has open) or a missing path proves
// nobody is listening. Any other dial failure — EACCES on a socket owned by
// another user, EAGAIN from a full listen backlog on Linux, a timeout — says
// nothing about whether a daemon is behind the path, so it comes back as an
// error and the caller must not unlink anything on the strength of it.
func probeSocket(path string) (socketProbe, error) {
	raw, err := net.DialTimeout("unix", path, probeTimeout)
	if err != nil {
		if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, fs.ErrNotExist) {
			return socketProbe{}, nil
		}
		return socketProbe{}, fmt.Errorf("cannot tell whether a daemon is listening at %s: %w", path, err)
	}
	defer func() { _ = raw.Close() }()
	p := socketProbe{Answered: true}
	_ = raw.SetDeadline(time.Now().Add(probeTimeout))
	info, err := probeDaemonInfo(raw)
	if err != nil {
		return p, nil
	}
	p.PID, p.Version = info.PID, info.Version
	return p, nil
}

// probeDaemonInfo runs the client half of the handshake plus one daemon_info
// call over raw. It speaks the protocol directly instead of importing
// internal/client: the daemon needs two frames, not the whole CLI client, and
// the dependency would only run the other way for this one call.
func probeDaemonInfo(raw net.Conn) (protocol.DaemonInfo, error) {
	var zero protocol.DaemonInfo
	pc := protocol.NewConn(raw)
	hello := protocol.Hello{
		ProtoVersion:  protocol.ProtoVersion,
		ClientVersion: "harness-daemon-probe",
	}
	if err := pc.WriteJSON(protocol.TypeHello, &hello); err != nil {
		return zero, err
	}
	f, err := pc.ReadFrame()
	if err != nil {
		return zero, err
	}
	if f.Type != protocol.TypeHello {
		return zero, fmt.Errorf("expected HELLO, got %s", f.Type)
	}
	const probeID = 1
	req := protocol.ControlReq{ID: probeID, Op: protocol.OpDaemonInfo}
	if err := pc.WriteJSON(protocol.TypeControlReq, &req); err != nil {
		return zero, err
	}
	for {
		f, err := pc.ReadFrame()
		if err != nil {
			return zero, err
		}
		switch f.Type {
		case protocol.TypeControlResp:
			var resp protocol.ControlResp
			if err := json.Unmarshal(f.Payload, &resp); err != nil {
				return zero, err
			}
			if resp.ID != probeID {
				continue
			}
			var info protocol.DaemonInfo
			if err := json.Unmarshal(resp.Data, &info); err != nil {
				return zero, err
			}
			return info, nil
		case protocol.TypeError:
			return zero, errors.New("daemon answered the probe with an ERROR frame")
		default:
			// PING, EVENT, anything else: not ours, keep reading.
		}
	}
}

// clearStaleSocket makes path bindable without ever unlinking a socket a live
// daemon answers on. A path that holds something other than a socket is left
// alone too — a regular file there is a misconfigured --socket, and deleting
// an operator's file to bind over it is never the right move.
func clearStaleSocket(path string) error {
	if err := CheckSocketFree(path); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// CheckSocketFree reports whether a daemon could bind path without displacing
// a live one, and changes nothing on disk. It returns an error wrapping
// ErrSocketInUse when something answers there.
//
// runDaemon calls it before restoring state and autostarting harnesses: a
// daemon that is going to refuse the socket must refuse BEFORE it has spawned
// duplicates of the live daemon's harnesses and flushed its own state.json
// over the live one's on the way out. Listen still runs the same probe, since
// a daemon can appear between the two.
func CheckSocketFree(path string) error {
	fi, err := os.Lstat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil
	case err != nil:
		return err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s exists and is not a socket (mode %s); refusing to remove it", path, fi.Mode())
	}
	p, err := probeSocket(path)
	if err != nil {
		return fmt.Errorf("%w; refusing to remove it", err)
	}
	if p.Answered {
		return fmt.Errorf("%w at %s (%s)", ErrSocketInUse, path, p.describe())
	}
	return nil
}

// bind creates a listener at the socket path and publishes it, replacing any
// listener this server already holds.
//
// The new listener is published BEFORE the old one is closed, and both happen
// under lnMu. Publishing after the close would leave a window where Serve's
// blocked Accept returns net.ErrClosed while the listener it is holding is
// still the current one — which reads as "the server is shutting down" and
// stops the accept loop for good.
func (s *Server) bind() error {
	// A watchdog tick that raced Close must not create a socket at all: the
	// done check further down drops it again, but in between a starting
	// daemon would probe a listener that never accepts and refuse to start.
	select {
	case <-s.done:
		return net.ErrClosed
	default:
	}
	if err := protocol.EnsureSocketDir(s.socketPath); err != nil {
		return err
	}
	if err := clearStaleSocket(s.socketPath); err != nil {
		return err
	}
	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return err
	}
	// Go unlinks a Unix listener's path when the listener closes, and it does
	// that by path, not by inode. On a re-bind the path already holds the
	// REPLACEMENT socket by the time the old listener closes, so the default
	// would delete the socket we just created — the same defect this file
	// exists to fix, arriving through the standard library. Every unlink is
	// ours to make, and removeOwnSocket makes it only when the path still
	// names our own listener.
	if ul, ok := ln.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	drop := func() {
		_ = ln.Close()
		_ = os.Remove(s.socketPath)
	}
	if err := os.Chmod(s.socketPath, protocol.SocketMode); err != nil {
		drop()
		return err
	}
	// The identity of the socket we just created: every later check compares
	// the path against this, so "the path exists" is never mistaken for "the
	// path is still mine".
	fi, err := os.Stat(s.socketPath)
	if err != nil {
		drop()
		return err
	}

	s.lnMu.Lock()
	select {
	case <-s.done:
		// Close ran while we were binding; do not resurrect the socket.
		s.lnMu.Unlock()
		drop()
		return net.ErrClosed
	default:
	}
	old := s.ln
	s.ln, s.bound = ln, fi
	s.lnMu.Unlock()

	if old != nil {
		_ = old.Close()
	}
	return nil
}

// listener returns the current listener, or nil before the first bind.
func (s *Server) listener() net.Listener {
	s.lnMu.Lock()
	defer s.lnMu.Unlock()
	return s.ln
}

// boundInfo returns the identity of the socket file this server bound, or nil
// if it never bound one.
func (s *Server) boundInfo() os.FileInfo {
	s.lnMu.Lock()
	defer s.lnMu.Unlock()
	return s.bound
}

// removeOwnSocket unlinks the socket path only when it still names this
// server's listener. An unconditional remove at shutdown is the same foot-gun
// as an unconditional remove at startup, pointed the other way: daemon A
// exiting would delete daemon B's socket (#578).
func (s *Server) removeOwnSocket() {
	bound := s.boundInfo()
	if bound == nil {
		return
	}
	cur, err := os.Stat(s.socketPath)
	if err != nil || !os.SameFile(cur, bound) {
		return
	}
	_ = os.Remove(s.socketPath)
}

// reassertSocket is one watchdog pass. It reports whether it re-bound, and an
// error for a state it deliberately will not act on — a foreign live listener,
// or a stat it could not make sense of.
//
// A path that has been taken over by another daemon that ANSWERS is left
// alone: re-binding it would hand the socket back and forth once a second
// between two live daemons, which is worse than one of them being unreachable.
func (s *Server) reassertSocket() (bool, error) {
	bound := s.boundInfo()
	if bound == nil {
		return false, nil // never listened; nothing to assert
	}
	select {
	case <-s.done:
		return false, nil // Close removed the socket on purpose
	default:
	}
	var why string
	cur, err := os.Stat(s.socketPath)
	switch {
	case err == nil:
		if os.SameFile(cur, bound) {
			return false, nil // the healthy case: one stat, nothing else
		}
		p, err := probeSocket(s.socketPath)
		if err != nil {
			return false, fmt.Errorf("%s no longer names this daemon's listener, and %w; leaving it alone", s.socketPath, err)
		}
		if p.Answered {
			return false, fmt.Errorf("%s now belongs to another live listener (%s); leaving it alone", s.socketPath, p.describe())
		}
		why = "daemon socket was replaced by one nothing answers on; re-binding"
	case errors.Is(err, fs.ErrNotExist):
		why = "daemon socket vanished while the daemon was still serving; re-binding"
	default:
		return false, err
	}
	// The warning is logged only once the bind has worked. A bind that keeps
	// failing (the runtime dir removed at logout, a regular file parked at
	// the path) fails on every tick, and its error — which carries why — goes
	// through watchSocket's once-per-change dedupe instead of a warning a
	// second forever.
	if err := s.bind(); err != nil {
		return false, fmt.Errorf("%s: re-binding %s: %w", strings.TrimSuffix(why, "; re-binding"), s.socketPath, err)
	}
	log.Warn(why, "socket", s.socketPath)
	log.Info("daemon socket re-bound; clients can reach the daemon again",
		"socket", s.socketPath)
	return true, nil
}

// watchSocket runs reassertSocket on the watch interval until the server
// closes. Errors repeat every tick by their nature (a foreign listener does
// not go away because we looked at it), so it logs a given complaint once and
// stays quiet until the state changes.
func (s *Server) watchSocket() {
	ticks := time.NewTicker(s.watchInterval)
	defer ticks.Stop()
	var lastComplaint string
	for {
		select {
		case <-s.done:
			return
		case <-ticks.C:
			_, err := s.reassertSocket()
			switch {
			case err == nil:
				lastComplaint = ""
			case err.Error() != lastComplaint:
				lastComplaint = err.Error()
				log.Error("daemon socket watchdog", "socket", s.socketPath, "err", err)
			}
		}
	}
}

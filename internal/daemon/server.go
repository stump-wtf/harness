// Package daemon serves the framed client↔daemon protocol (SPEC-0002) over the
// local Unix socket, bridging clients to the supervisor Manager (control plane
// + event subscription) and the attach Registry (data plane).
//
// Governing: SPEC-0002 (all requirements); ADR-0002 (the daemon owns state,
// clients are thin); ADR-0004 (Unix-socket control+data plane); ADR-0008
// (socket 0600). The server accepts connections, runs the HELLO handshake and
// proto-major check, dispatches control requests idempotently with structured
// ERROR frames, pushes EVENT frames from Manager.Events() to subscribers, and
// multiplexes attach sessions over each connection.
package daemon

import (
	"errors"
	"net"
	"os"
	"sync"
	"time"

	"github.com/stump-wtf/harness/internal/attach"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/scheduler"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// defaultPingInterval is how often the daemon PINGs each connection so a dead
// client's write fails and its sessions get reaped (SPEC-0002 REQ "Backpressure
// Isolation": "PING/PONG heartbeats SHALL detect dead clients").
const defaultPingInterval = 15 * time.Second

// defaultLivenessTimeout is how long a connection may go without a single
// inbound frame before the daemon reaps it (#183). A client that is alive but
// no longer reading its socket never fails the daemon's writes — the kernel
// receive buffer swallows the PINGs — so it holds its attach sessions forever
// and, because the resize policy takes the minimum viewport across sessions
// (ADR-0003), clamps the guest PTY for every other client until the daemon
// restarts. Silence is the only signal that distinguishes it from a live one.
//
// Four ping intervals is three consecutive unanswered PINGs plus a full
// interval of slack. A healthy client answers within one round trip on a Unix
// socket, so the margin covers a GC pause, a long repaint, or a stalled SSH
// hop without ever reaping a working session, while still freeing a clamped
// PTY inside a minute rather than never.
const defaultLivenessTimeout = 4 * defaultPingInterval

// Server serves the protocol on a Unix socket.
type Server struct {
	mgr        *supervisor.Manager
	reg        *attach.Registry
	sched      *scheduler.Scheduler
	socketPath string
	configPath string
	version    string
	started    time.Time

	// pingInterval / livenessTimeout are the heartbeat knobs, seeded from the
	// defaults above and fixed for the Server's lifetime. They are fields
	// rather than package constants so tests can drive the reaper in
	// milliseconds instead of minutes; nothing in production sets them.
	pingInterval    time.Duration
	livenessTimeout time.Duration

	// watchInterval is how often the socket watchdog re-asserts that the
	// socket path still names ln (socket.go). Seeded from
	// defaultSocketWatchInterval; tests shrink it so a pass runs in
	// milliseconds.
	watchInterval time.Duration

	// lnMu guards ln and bound, which the socket watchdog replaces underneath
	// the accept loop when the socket file is removed out from under a live
	// daemon (#578). bound is the identity (dev+inode) of the socket file ln
	// is listening on, so "the path exists" is never mistaken for "the path is
	// still mine".
	lnMu  sync.Mutex
	ln    net.Listener
	bound os.FileInfo

	done chan struct{}
	once sync.Once
	wg   sync.WaitGroup

	subMu sync.Mutex
	subs  map[chan protocol.EventMsg]struct{}

	// remoteMu guards the remote-SSH status fields below. SetRemote is
	// called by the daemon entrypoint AFTER NewServer, once startRemote has
	// decided whether the Wish server is up, so opDaemonInfo can report it.
	remoteMu   sync.Mutex
	remoteAddr string
	remoteKeys int

	// connMu guards the set of live client connections and the closing flag.
	// Close() closes each raw socket to unblock its ReadFrame loop; without this
	// a blocked reader never returns and wg.Wait() hangs at shutdown (a
	// listener/`done` close does not interrupt an accepted connection's read).
	connMu  sync.Mutex
	conns   map[*conn]struct{}
	closing bool
}

// Options configure a Server.
type Options struct {
	Manager  *supervisor.Manager
	Registry *attach.Registry
	// Scheduler exposes next-fire times for scheduled harnesses in list and
	// describe (ADR-0013). Optional: nil leaves NextRun empty.
	Scheduler  *scheduler.Scheduler
	SocketPath string
	ConfigPath string // for the reload op
	Version    string

	// PingInterval and LivenessTimeout override the heartbeat defaults. Zero
	// (the production case) means defaultPingInterval / defaultLivenessTimeout;
	// tests shrink them so the reaper runs in milliseconds.
	PingInterval    time.Duration
	LivenessTimeout time.Duration

	// SocketWatchInterval overrides how often the socket watchdog checks that
	// the socket path still names this server's listener (socket.go). Zero —
	// the production case — means defaultSocketWatchInterval.
	SocketWatchInterval time.Duration
}

// NewServer builds a Server. It does not listen until Listen is called.
func NewServer(opts Options) *Server {
	ping := opts.PingInterval
	if ping <= 0 {
		ping = defaultPingInterval
	}
	liveness := opts.LivenessTimeout
	if liveness <= 0 {
		liveness = defaultLivenessTimeout
	}
	watch := opts.SocketWatchInterval
	if watch <= 0 {
		watch = defaultSocketWatchInterval
	}
	return &Server{
		mgr:             opts.Manager,
		reg:             opts.Registry,
		sched:           opts.Scheduler,
		socketPath:      opts.SocketPath,
		configPath:      opts.ConfigPath,
		version:         opts.Version,
		started:         time.Now(),
		pingInterval:    ping,
		livenessTimeout: liveness,
		watchInterval:   watch,
		done:            make(chan struct{}),
		subs:            make(map[chan protocol.EventMsg]struct{}),
		conns:           make(map[*conn]struct{}),
	}
}

// Listen binds the Unix socket at the configured path with mode 0600 (ADR-0008).
// The parent directory is created (0700) when it is a fallback location without
// $XDG_RUNTIME_DIR.
//
// A stale socket left by a crashed daemon is removed so the bind succeeds, but
// only after a probe shows nothing answers on it: an unconditional remove here
// lets a second `harness daemon` unlink a live daemon's socket, which leaves
// the first one supervising happily and unreachable by every client (#578).
// When something does answer, Listen returns an error wrapping ErrSocketInUse
// and binds nothing.
func (s *Server) Listen() error { return s.bind() }

// SocketPath returns the bound socket path.
func (s *Server) SocketPath() string { return s.socketPath }

// SetRemote records the running remote SSH server's bind address and allowlist
// size for daemon_info. Call with an empty addr to mark it as not running.
func (s *Server) SetRemote(addr string, keys int) {
	s.remoteMu.Lock()
	defer s.remoteMu.Unlock()
	s.remoteAddr = addr
	s.remoteKeys = keys
}

// Remote reports the running remote SSH server's bind address ("" when off)
// and its resolved allowlist size.
func (s *Server) Remote() (string, int) {
	s.remoteMu.Lock()
	defer s.remoteMu.Unlock()
	return s.remoteAddr, s.remoteKeys
}

// Serve accepts connections until Close. It also starts the event relay that
// fans Manager lifecycle events out to subscribed connections, and the socket
// watchdog that re-binds the socket if it is removed under a live daemon
// (socket.go). Blocks until the listener is closed.
//
// Starting the watchdog here rather than at the call site is deliberate: every
// daemon that serves gets it, and there is no wiring for a future caller to
// forget.
func (s *Server) Serve() {
	s.goInternal(s.relayLoop)
	s.goInternal(s.watchSocket)
	for {
		ln := s.listener()
		if ln == nil {
			return // Serve without Listen; nothing to accept on
		}
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				return // closing
			default:
			}
			// The watchdog re-bound: our listener was closed on purpose and
			// the replacement is already published, so pick it up.
			if s.listener() != ln {
				continue
			}
			// A listener closed with no replacement is a shutdown we did not
			// see on `done`; retrying would spin on it forever.
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// Transient accept error; safe to retry.
			continue
		}
		if !s.goInternal(func() { s.handleConn(conn) }) {
			_ = conn.Close() // accepted as Close ran; nobody will serve it
		}
	}
}

// goInternal starts one of the server's own goroutines and registers it with
// the shutdown WaitGroup, reporting false (and starting nothing) when the
// server is already closing.
//
// The registration happens under connMu — the lock Close takes before it waits
// — so an Add can never land after Wait has started. Adding from inside the
// goroutine that `go srv.Serve()` spawned is a WaitGroup misuse the race
// detector fails on, and worse, a Close racing a starting goroutine returns
// without waiting for it.
func (s *Server) goInternal(fn func()) bool {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.closing {
		return false
	}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		fn()
	}()
	return true
}

// Close stops accepting, tears down the listener + socket file, closes every
// live client connection (so its blocked ReadFrame loop returns), and waits for
// in-flight connection goroutines to finish. Without closing the client sockets
// the reader loops would block forever and wg.Wait() would hang, so a clean
// daemon shutdown (and the mgr.Close state flush after it) never happens while a
// TUI/attach/CLI client is connected.
func (s *Server) Close() {
	s.once.Do(func() {
		close(s.done)
		if ln := s.listener(); ln != nil {
			_ = ln.Close()
		}
		// Only unlink the socket when it is still ours: an unconditional
		// remove at shutdown is the startup foot-gun pointed the other way —
		// daemon A exiting would delete daemon B's socket (#578).
		s.removeOwnSocket()
		s.connMu.Lock()
		s.closing = true
		for c := range s.conns {
			_ = c.raw.Close() // unblock this connection's ReadFrame loop
		}
		s.connMu.Unlock()
	})
	s.wg.Wait()
}

// registerConn adds c to the live set so Close can reach it. It returns false if
// the server is already closing, in which case the caller must not serve the
// connection (it races an in-flight Accept against Close).
func (s *Server) registerConn(c *conn) bool {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	if s.closing {
		return false
	}
	s.conns[c] = struct{}{}
	return true
}

// unregisterConn drops c from the live set (called from teardown).
func (s *Server) unregisterConn(c *conn) {
	s.connMu.Lock()
	delete(s.conns, c)
	s.connMu.Unlock()
}

// ConnCount reports the number of live client connections. It lets callers and
// tests observe that connections are reaped when a client disconnects (e.g. a
// remote SSH session closing must not leak its two daemon connections).
func (s *Server) ConnCount() int {
	s.connMu.Lock()
	defer s.connMu.Unlock()
	return len(s.conns)
}

// --- event relay ----------------------------------------------------------

// relayLoop reads the Manager's lifecycle event stream and broadcasts each as a
// protocol EventMsg to every subscribed connection (SPEC-0002 REQ "Event
// Subscription").
func (s *Server) relayLoop() {
	events, cancel := s.mgr.Events()
	defer cancel()
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				return
			}
			s.broadcast(toEventMsg(ev))
		case <-s.done:
			return
		}
	}
}

// broadcast delivers ev to every subscriber without blocking (a full subscriber
// queue drops the event for that client only — bounded, lossy fan-out; the
// client repaints from the current state, SPEC-0002 REQ "Backpressure
// Isolation").
func (s *Server) broadcast(ev protocol.EventMsg) {
	s.subMu.Lock()
	for ch := range s.subs {
		select {
		case ch <- ev:
		default:
		}
	}
	s.subMu.Unlock()
}

// subscribe registers a new event queue; unsubscribe removes it.
func (s *Server) subscribe() chan protocol.EventMsg {
	ch := make(chan protocol.EventMsg, 128)
	s.subMu.Lock()
	s.subs[ch] = struct{}{}
	s.subMu.Unlock()
	return ch
}

func (s *Server) unsubscribe(ch chan protocol.EventMsg) {
	s.subMu.Lock()
	delete(s.subs, ch)
	s.subMu.Unlock()
}

// toEventMsg projects a supervisor.Event onto the wire EventMsg. The three
// supervisor kinds map 1:1 to the first three protocol event kinds (SPEC-0002).
func toEventMsg(ev supervisor.Event) protocol.EventMsg {
	m := protocol.EventMsg{Name: ev.Name}
	switch ev.Kind {
	case supervisor.EventStateChanged:
		m.Kind = protocol.EvStateChanged
		m.From = string(ev.From)
		m.To = string(ev.To)
	case supervisor.EventExited:
		m.Kind = protocol.EvExited
		m.Code = ev.Code
	case supervisor.EventFlapping:
		m.Kind = protocol.EvFlapping
		m.Restarts = ev.Restarts
		m.NextRetryInMs = ev.NextRetryIn.Milliseconds()
	case supervisor.EventRunStarted, supervisor.EventRunFinished:
		// SPEC-0008 REQ "Lifecycle Events" (#120).
		m.Kind = protocol.EvJobRunStarted
		if ev.Kind == supervisor.EventRunFinished {
			m.Kind = protocol.EvJobRunFinished
		}
		m.RunID = ev.Run.RunID
		m.Trigger = string(ev.Run.Trigger)
		m.Outcome = string(ev.Run.Outcome)
		m.ExitCode = ev.Run.ExitCode
		if ev.Run.EndedAt != nil {
			m.DurationMs = ev.Run.EndedAt.Sub(ev.Run.StartedAt).Milliseconds()
		}
	case supervisor.EventScheduleChanged:
		m.Kind = protocol.EvJobScheduleChanged
		if !ev.NextRun.IsZero() {
			m.NextRunAt = ev.NextRun.Format(time.RFC3339)
		}
	case supervisor.EventHoursChanged:
		// SPEC-0012 REQ "Operating Hours Visibility".
		m.Kind = protocol.EvHoursChanged
		m.InHours = ev.InHours
		if !ev.HoursNext.IsZero() {
			m.HoursNext = ev.HoursNext.Format(time.RFC3339)
		}
	}
	return m
}

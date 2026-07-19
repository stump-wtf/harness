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

	"gitea.stump.rocks/stump.wtf/harness/internal/attach"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/supervisor"
)

// pingInterval is how often the daemon PINGs each connection so a dead client's
// write fails and its sessions get reaped (SPEC-0002 REQ "Backpressure
// Isolation": "PING/PONG heartbeats SHALL detect dead clients").
const pingInterval = 15 * time.Second

// Server serves the protocol on a Unix socket.
type Server struct {
	mgr        *supervisor.Manager
	reg        *attach.Registry
	socketPath string
	configPath string
	version    string
	started    time.Time

	ln   net.Listener
	done chan struct{}
	once sync.Once
	wg   sync.WaitGroup

	subMu sync.Mutex
	subs  map[chan protocol.EventMsg]struct{}

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
	Manager    *supervisor.Manager
	Registry   *attach.Registry
	SocketPath string
	ConfigPath string // for the reload op
	Version    string
}

// NewServer builds a Server. It does not listen until Listen is called.
func NewServer(opts Options) *Server {
	return &Server{
		mgr:        opts.Manager,
		reg:        opts.Registry,
		socketPath: opts.SocketPath,
		configPath: opts.ConfigPath,
		version:    opts.Version,
		started:    time.Now(),
		done:       make(chan struct{}),
		subs:       make(map[chan protocol.EventMsg]struct{}),
		conns:      make(map[*conn]struct{}),
	}
}

// Listen binds the Unix socket at the configured path with mode 0600 (ADR-0008),
// removing any stale socket file first. The parent directory is created (0700)
// when it is a fallback location without $XDG_RUNTIME_DIR.
func (s *Server) Listen() error {
	if err := protocol.EnsureSocketDir(s.socketPath); err != nil {
		return err
	}
	// A leftover socket from a crashed daemon would make Listen fail with
	// "address already in use"; remove it (it is ours, under our 0700 dir).
	if err := os.Remove(s.socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	ln, err := net.Listen("unix", s.socketPath)
	if err != nil {
		return err
	}
	if err := os.Chmod(s.socketPath, protocol.SocketMode); err != nil {
		_ = ln.Close()
		return err
	}
	s.ln = ln
	return nil
}

// SocketPath returns the bound socket path.
func (s *Server) SocketPath() string { return s.socketPath }

// Serve accepts connections until Close. It also starts the event relay that
// fans Manager lifecycle events out to subscribed connections. Blocks until the
// listener is closed.
func (s *Server) Serve() {
	s.wg.Add(1)
	go s.relayLoop()
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.done:
				return // closing
			default:
			}
			// Transient accept error; a closed listener falls through the
			// done check above, so this is safe to retry.
			continue
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.handleConn(conn)
		}()
	}
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
		if s.ln != nil {
			_ = s.ln.Close()
		}
		_ = os.Remove(s.socketPath)
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

// --- event relay ----------------------------------------------------------

// relayLoop reads the Manager's lifecycle event stream and broadcasts each as a
// protocol EventMsg to every subscribed connection (SPEC-0002 REQ "Event
// Subscription").
func (s *Server) relayLoop() {
	defer s.wg.Done()
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
	}
	return m
}

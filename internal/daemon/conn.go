package daemon

// Governing: SPEC-0002 REQ "Handshake And Versioning" (HELLO + proto-major
// check; mismatch → structured ERROR then clean close), REQ "Message Framing"
// (control and attach multiplexed on one connection), REQ "Event Subscription"
// (push EVENT frames after a HELLO that wants events), REQ "Attach Session"
// (ATTACH_OPEN/DATA/RESIZE/CLOSE per session), and REQ "Backpressure Isolation"
// ("PING/PONG heartbeats SHALL detect dead clients so their sessions get
// reaped" — the liveness reaper below, plus ADR-0003 for the smallest-attached-
// wins minimum a stale session would otherwise pin).

import (
	"encoding/json"
	"net"
	"sync"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/attach"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// conn is one live client connection: the framed transport plus its attach
// sessions. Reads happen on a single goroutine (the handler loop); writes are
// serialized by protocol.Conn's mutex, so the event forwarder, attach pumps,
// control responder, and heartbeat can all write concurrently.
type conn struct {
	srv *Server
	pc  *protocol.Conn
	raw net.Conn
	sub chan protocol.EventMsg

	closed    chan struct{}
	closeOnce sync.Once

	mu       sync.Mutex
	sessions map[uint32]*attach.Session

	// liveMu guards the liveness bookkeeping the heartbeat reaper reads (#183).
	// It is deliberately not mu: mu is held while attach sessions are looked up
	// and torn down, and the reaper must be able to sample liveness without
	// queueing behind that.
	liveMu sync.Mutex
	// lastAlive is when this connection last produced an inbound frame — see
	// markAlive for why any frame, not just PONG, counts.
	lastAlive time.Time
	// sawPong records that this client has answered at least one PING. It is
	// the compatibility guard on eviction; see stale().
	sawPong bool
}

// handleConn runs the full lifecycle for one accepted connection.
func (s *Server) handleConn(raw net.Conn) {
	c := &conn{
		srv:       s,
		pc:        protocol.NewConn(raw),
		raw:       raw,
		closed:    make(chan struct{}),
		sessions:  make(map[uint32]*attach.Session),
		lastAlive: time.Now(),
	}
	// Register before serving so a concurrent Close can reach this socket; if the
	// server is already shutting down, close immediately and bail.
	if !s.registerConn(c) {
		_ = raw.Close()
		return
	}
	defer c.teardown()

	if !c.handshake() {
		return
	}
	c.loop()
}

// handshake performs the HELLO exchange and proto-major version check. It
// returns false (after sending a structured ERROR and closing) on any mismatch
// or malformed opener.
func (c *conn) handshake() bool {
	// First frame MUST be HELLO.
	f, err := c.pc.ReadFrame()
	if err != nil {
		return false
	}
	if f.Type != protocol.TypeHello {
		_ = c.pc.WriteError(0, protocol.ErrBadRequest, "expected HELLO, got %s", f.Type)
		return false
	}
	var hello protocol.Hello
	if err := json.Unmarshal(f.Payload, &hello); err != nil {
		_ = c.pc.WriteError(0, protocol.ErrBadRequest, "malformed HELLO: %v", err)
		return false
	}
	clientMajor, err := protocol.Major(hello.ProtoVersion)
	if err != nil {
		_ = c.pc.WriteError(0, protocol.ErrBadRequest, "malformed proto_version %q", hello.ProtoVersion)
		return false
	}
	if clientMajor != protocol.ProtoMajor {
		// SPEC-0002 REQ "Handshake And Versioning": a clear version-mismatch
		// ERROR, then close cleanly — never garble.
		_ = c.pc.WriteError(0, protocol.ErrVersionMismatch,
			"client proto v%d unsupported; daemon proto v%d", clientMajor, protocol.ProtoMajor)
		return false
	}

	// Reply HELLO with our versions + capabilities.
	reply := protocol.Hello{
		ProtoVersion:  protocol.ProtoVersion,
		DaemonVersion: c.srv.version,
		Capabilities:  []string{"control", "events", "attach"},
	}
	if err := c.pc.WriteJSON(protocol.TypeHello, &reply); err != nil {
		return false
	}

	// Honor an events subscription (SPEC-0002 REQ "Event Subscription"). A
	// one-shot CLI omits "events" and never gets a forwarder.
	if wants(hello.Wants, "events") {
		c.sub = c.srv.subscribe()
		c.srv.wg.Add(1)
		go c.forwardEvents()
	}
	c.srv.wg.Add(1)
	go c.heartbeat()
	c.srv.wg.Add(1)
	go c.reap()
	return true
}

// wants reports whether want is in the list.
func wants(list []string, want string) bool {
	for _, w := range list {
		if w == want {
			return true
		}
	}
	return false
}

// forwardEvents pushes subscribed EVENT frames to the client until the
// connection closes.
func (c *conn) forwardEvents() {
	defer c.srv.wg.Done()
	for {
		select {
		case ev := <-c.sub:
			if err := c.pc.WriteJSON(protocol.TypeEvent, &ev); err != nil {
				return
			}
		case <-c.srv.done:
			return
		case <-c.closed:
			return
		}
	}
}

// markAlive records proof that the client is still processing its socket. It is
// called for every inbound frame, and pong marks the frame as a PONG.
//
// Any inbound frame counts, not just PONG: a client that is issuing control
// requests, attach input, or resizes is demonstrably transacting on this
// connection and is not the wedged-and-gone case this reaper exists for.
// Counting only PONG would discard evidence we already have, for no gain: it
// cannot detect anything the wider signal misses, and it can only ever evict
// sooner — and a false eviction destroys a working session, which is strictly
// worse than carrying a stale one for another interval. PONG then serves as the
// *solicited* variant: it guarantees an otherwise idle-but-healthy client still
// refreshes its liveness once per ping interval, so silence really does mean
// silence rather than "nothing to say".
//
// Known limitation: a client whose read side is wedged while its write side
// keeps emitting frames would be treated as alive. Neither of our clients can
// be in that state — both answer PING from the same read loop that would be
// stuck — and mis-classifying that hypothetical is the safe direction.
func (c *conn) markAlive(pong bool) {
	c.liveMu.Lock()
	c.lastAlive = time.Now()
	if pong {
		c.sawPong = true
	}
	c.liveMu.Unlock()
}

// stale reports whether this connection has gone silent long enough to reap
// (#183). It is deliberately conservative in one specific way.
//
// Compatibility guard: a client that has never PONGed at all is a client that
// does not speak PONG, not a wedged one, and reaping it would kill a working
// session on an older build. Only a connection that proved it answers PINGs and
// then stopped is eligible. PING/PONG are frame types 9/10 in the base
// protocol, so there is no ProtoMinor that separates "answers PONG" from
// "doesn't" — gating on the HELLO version would gate on nothing, and a version
// is a claim where an observed PONG is evidence. Hence behaviour, not version.
//
// The edge this honestly leaves open: a client that wedges *before* its first
// PONG — inside the first ping interval of connecting — is never eligible, and
// still pins the minimum until the daemon restarts. Narrowing that would mean
// reaping clients we have no evidence about, which trades a rare stuck session
// for routinely killing live ones.
func (c *conn) stale() bool {
	c.liveMu.Lock()
	defer c.liveMu.Unlock()
	if !c.sawPong {
		return false
	}
	return time.Since(c.lastAlive) > c.srv.livenessTimeout
}

// heartbeat PINGs the client periodically (SPEC-0002 REQ "Backpressure
// Isolation"). A failed write means the client is gone.
//
// It does not reap: the eviction decision lives in reap(), on its own
// goroutine, because this one can block indefinitely. See reap() for why that
// separation is load-bearing.
func (c *conn) heartbeat() {
	defer c.srv.wg.Done()
	t := time.NewTicker(c.srv.pingInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if err := c.pc.WriteFrame(protocol.TypePing, nil); err != nil {
				return
			}
		case <-c.srv.done:
			return
		case <-c.closed:
			return
		}
	}
}

// reap closes a connection that has gone silent (#183): a client that keeps
// absorbing PINGs without ever answering, because its socket receive buffer
// makes our writes succeed long after it stopped reading.
//
// This runs on its own goroutine, and that is the whole point. It used to
// share heartbeat's loop, which made the reaper reachable only between PING
// writes — and a wedged client is precisely the case where that write does not
// return. Once the client stops reading, its socket send buffer fills, the
// session pump blocks inside net.Write holding the protocol Conn's write
// mutex, and the next PING blocks acquiring that mutex. The reaper never got
// another tick, so the connection it existed to evict pinned the mux minimum
// forever. Backpressure disarmed the backpressure defense.
//
// Only the socket buffer size decided how long that took to bite, which is why
// it read as platform-specific: ~8 KiB on macOS fills during a single attach
// repaint, where Linux's ~208 KiB default absorbs it and hides the bug until
// enough output accumulates.
//
// So reap touches nothing that can block on the peer: stale() takes only the
// liveness mutex, and Close on the raw socket is what unblocks everyone else.
// Closing unblocks loop()'s ReadFrame and the wedged Write, and lets
// handleConn's deferred teardown() detach the sessions through the one
// existing path — Mux.Detach then recomputes smallest-attached-wins for
// whoever is left, so a surviving healthy client gets its geometry back
// (ADR-0003).
func (c *conn) reap() {
	defer c.srv.wg.Done()
	t := time.NewTicker(c.srv.pingInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			if c.stale() {
				_ = c.raw.Close()
				return
			}
		case <-c.srv.done:
			return
		case <-c.closed:
			return
		}
	}
}

// loop reads and dispatches frames until EOF or error.
func (c *conn) loop() {
	for {
		f, err := c.pc.ReadFrame()
		if err != nil {
			return // EOF or transport error → teardown
		}
		// Every frame that arrives is proof the client is still reading and
		// writing this socket (#183).
		c.markAlive(f.Type == protocol.TypePong)
		switch f.Type {
		case protocol.TypeControlReq:
			c.handleControl(f.Payload)
		case protocol.TypeAttachOpen:
			c.handleAttachOpen(f.Payload)
		case protocol.TypeAttachData:
			c.handleAttachData(f.Payload)
		case protocol.TypeAttachResize:
			c.handleAttachResize(f.Payload)
		case protocol.TypeAttachClose:
			c.handleAttachClose(f.Payload)
		case protocol.TypePing:
			_ = c.pc.WriteFrame(protocol.TypePong, nil)
		case protocol.TypePong:
			// Liveness ack; already recorded by markAlive above.
		default:
			_ = c.pc.WriteError(0, protocol.ErrBadRequest, "unexpected frame %s", f.Type)
		}
	}
}

// --- attach data plane ----------------------------------------------------

func (c *conn) handleAttachOpen(payload []byte) {
	id, rest, err := protocol.DecodeAttach(payload)
	if err != nil {
		_ = c.pc.WriteError(0, protocol.ErrBadRequest, "%v", err)
		return
	}
	var open protocol.AttachOpen
	if err := json.Unmarshal(rest, &open); err != nil {
		_ = c.pc.WriteError(0, protocol.ErrBadRequest, "malformed ATTACH_OPEN: %v", err)
		return
	}
	// The harness must exist (SPEC-0002 REQ "Control Operations" structured
	// failure, applied to attach).
	if _, ok := c.srv.mgr.Snapshot(open.Name); !ok {
		_ = c.pc.WriteError(0, protocol.ErrUnknownHarness, "unknown harness %q", open.Name)
		return
	}
	mode := open.Mode
	if mode != protocol.AttachRO {
		mode = protocol.AttachRW
	}
	mux := c.srv.reg.Mux(open.Name)
	writeFn := func(data []byte) error {
		return c.pc.WriteFrame(protocol.TypeAttachData, protocol.EncodeAttach(id, data))
	}
	sess := mux.Attach(id, mode, open.Cols, open.Rows, writeFn)
	c.mu.Lock()
	c.sessions[id] = sess
	c.mu.Unlock()
}

func (c *conn) handleAttachData(payload []byte) {
	id, rest, err := protocol.DecodeAttach(payload)
	if err != nil {
		return
	}
	if sess := c.session(id); sess != nil {
		sess.Input(rest)
	}
}

func (c *conn) handleAttachResize(payload []byte) {
	id, rest, err := protocol.DecodeAttach(payload)
	if err != nil {
		return
	}
	sess := c.session(id)
	if sess == nil {
		return
	}
	var rz protocol.AttachResize
	if err := json.Unmarshal(rest, &rz); err != nil {
		return
	}
	sess.Resize(rz.Cols, rz.Rows)
}

func (c *conn) handleAttachClose(payload []byte) {
	id, _, err := protocol.DecodeAttach(payload)
	if err != nil {
		return
	}
	c.mu.Lock()
	sess := c.sessions[id]
	delete(c.sessions, id)
	c.mu.Unlock()
	if sess != nil {
		sess.Detach()
	}
}

// session looks up a live attach session by id.
func (c *conn) session(id uint32) *attach.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.sessions[id]
}

// teardown detaches every session, unsubscribes events, and closes the socket.
// It is deferred by handleConn and runs exactly once.
func (c *conn) teardown() {
	c.closeOnce.Do(func() { close(c.closed) })
	c.mu.Lock()
	sessions := make([]*attach.Session, 0, len(c.sessions))
	for _, s := range c.sessions {
		sessions = append(sessions, s)
	}
	c.sessions = map[uint32]*attach.Session{}
	c.mu.Unlock()
	for _, s := range sessions {
		s.Detach()
	}
	if c.sub != nil {
		c.srv.unsubscribe(c.sub)
	}
	c.srv.unregisterConn(c)
	_ = c.pc.Close()
}

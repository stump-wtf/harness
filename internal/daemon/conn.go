package daemon

// Governing: SPEC-0002 REQ "Handshake And Versioning" (HELLO + proto-major
// check; mismatch → structured ERROR then clean close), REQ "Message Framing"
// (control and attach multiplexed on one connection), REQ "Event Subscription"
// (push EVENT frames after a HELLO that wants events), and REQ "Attach Session"
// (ATTACH_OPEN/DATA/RESIZE/CLOSE per session).

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
}

// handleConn runs the full lifecycle for one accepted connection.
func (s *Server) handleConn(raw net.Conn) {
	c := &conn{
		srv:      s,
		pc:       protocol.NewConn(raw),
		raw:      raw,
		closed:   make(chan struct{}),
		sessions: make(map[uint32]*attach.Session),
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

// heartbeat PINGs the client periodically; a failed write means the client is
// gone and unblocks teardown of its sessions (SPEC-0002 REQ "Backpressure
// Isolation").
func (c *conn) heartbeat() {
	defer c.srv.wg.Done()
	t := time.NewTicker(pingInterval)
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

// loop reads and dispatches frames until EOF or error.
func (c *conn) loop() {
	for {
		f, err := c.pc.ReadFrame()
		if err != nil {
			return // EOF or transport error → teardown
		}
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
			// Liveness ack; nothing to do.
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
	_ = c.pc.Close()
}

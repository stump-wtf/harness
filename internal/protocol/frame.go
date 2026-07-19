// Package protocol is the single framed wire contract spoken between the
// harness clients (TUI/CLI/SSH) and the daemon, over the local Unix socket.
//
// Governing: SPEC-0002 (daemon-protocol) REQ "Message Framing" — every message
// is `uint32 length (BE)` + `uint8 type` + payload, with control payloads JSON
// and attach payloads raw bytes tagged with a session id so several attach
// sessions multiplex one connection; ADR-0004 (one framed protocol carries
// both the control plane and the attach data plane over the same socket).
package protocol

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"sync"
)

// Type is the one-byte frame discriminator. The set maps 1:1 to the frame
// types SPEC-0002 REQ "Message Framing" enumerates.
type Type uint8

const (
	// TypeHello is the handshake in both directions (SPEC-0002 REQ "Handshake
	// And Versioning").
	TypeHello Type = 1
	// TypeControlReq is a JSON control-plane request (list/start/…).
	TypeControlReq Type = 2
	// TypeControlResp is a JSON control-plane response.
	TypeControlResp Type = 3
	// TypeEvent is a pushed lifecycle EVENT (SPEC-0002 REQ "Event
	// Subscription").
	TypeEvent Type = 4
	// TypeAttachOpen opens an attach session (SPEC-0002 REQ "Attach Session").
	TypeAttachOpen Type = 5
	// TypeAttachData carries raw terminal bytes in either direction, tagged
	// with a session id.
	TypeAttachData Type = 6
	// TypeAttachResize requests a viewport resize for a session.
	TypeAttachResize Type = 7
	// TypeAttachClose tears down a single attach session.
	TypeAttachClose Type = 8
	// TypePing / TypePong are the heartbeat that reaps dead clients (SPEC-0002
	// REQ "Backpressure Isolation").
	TypePing Type = 9
	TypePong Type = 10
	// TypeError is a structured failure frame (SPEC-0002 REQ "Control
	// Operations": machine code + human message).
	TypeError Type = 11
)

// String renders the frame type for logs/tests.
func (t Type) String() string {
	switch t {
	case TypeHello:
		return "HELLO"
	case TypeControlReq:
		return "CONTROL_REQ"
	case TypeControlResp:
		return "CONTROL_RESP"
	case TypeEvent:
		return "EVENT"
	case TypeAttachOpen:
		return "ATTACH_OPEN"
	case TypeAttachData:
		return "ATTACH_DATA"
	case TypeAttachResize:
		return "ATTACH_RESIZE"
	case TypeAttachClose:
		return "ATTACH_CLOSE"
	case TypePing:
		return "PING"
	case TypePong:
		return "PONG"
	case TypeError:
		return "ERROR"
	default:
		return fmt.Sprintf("TYPE(%d)", uint8(t))
	}
}

// MaxFrameSize caps a single frame's length field so a malformed or hostile
// peer cannot make us allocate unbounded memory. Attach chunks are far smaller;
// control JSON is tiny. 16 MiB is comfortably above any legitimate frame.
const MaxFrameSize = 16 << 20

// Frame is one decoded wire message: a type plus its raw payload bytes. The
// payload is JSON for control frames and raw (session-tagged) bytes for attach
// frames — the codecs in messages.go interpret it per type.
type Frame struct {
	Type    Type
	Payload []byte
}

// Conn wraps a duplex byte stream (the Unix socket) with framed read/write.
// Writes are serialized by a mutex so frames from concurrent producers (the
// event relay, control responses, and multiple attach sessions) never
// interleave on the wire — each WriteFrame emits one whole frame atomically,
// which is what makes multiplexing many attach sessions on one connection safe
// (SPEC-0002 REQ "Message Framing": "both flow over the same connection without
// corrupting either stream").
type Conn struct {
	rw io.ReadWriteCloser
	br *bufio.Reader

	wmu sync.Mutex
	bw  *bufio.Writer
}

// NewConn wraps rw with buffered framed I/O.
func NewConn(rw io.ReadWriteCloser) *Conn {
	return &Conn{
		rw: rw,
		br: bufio.NewReader(rw),
		bw: bufio.NewWriter(rw),
	}
}

// ReadFrame reads one frame. The 4-byte big-endian length covers the type byte
// plus the payload; a length of zero or one that under-runs the type byte, or
// one exceeding MaxFrameSize, is a protocol error.
func (c *Conn) ReadFrame() (Frame, error) {
	var lenBuf [4]byte
	if _, err := io.ReadFull(c.br, lenBuf[:]); err != nil {
		return Frame{}, err
	}
	n := binary.BigEndian.Uint32(lenBuf[:])
	if n < 1 {
		return Frame{}, fmt.Errorf("protocol: frame length %d too small (need >=1 for type byte)", n)
	}
	if n > MaxFrameSize {
		return Frame{}, fmt.Errorf("protocol: frame length %d exceeds max %d", n, MaxFrameSize)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(c.br, buf); err != nil {
		return Frame{}, err
	}
	return Frame{Type: Type(buf[0]), Payload: buf[1:]}, nil
}

// WriteFrame writes one whole frame atomically and flushes it. It is safe for
// concurrent callers.
func (c *Conn) WriteFrame(t Type, payload []byte) error {
	if len(payload)+1 > MaxFrameSize {
		return fmt.Errorf("protocol: payload %d exceeds max frame size", len(payload))
	}
	c.wmu.Lock()
	defer c.wmu.Unlock()
	var hdr [5]byte
	binary.BigEndian.PutUint32(hdr[:4], uint32(len(payload)+1))
	hdr[4] = byte(t)
	if _, err := c.bw.Write(hdr[:]); err != nil {
		return err
	}
	if len(payload) > 0 {
		if _, err := c.bw.Write(payload); err != nil {
			return err
		}
	}
	return c.bw.Flush()
}

// Close closes the underlying stream.
func (c *Conn) Close() error { return c.rw.Close() }

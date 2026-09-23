// Package client dials the daemon's Unix socket and speaks the framed protocol
// (SPEC-0002) on behalf of the scriptable CLI and the TUI.
//
// Governing: SPEC-0002 (client half of the contract: HELLO handshake, control
// request/response, EVENT subscription, attach frames); ADR-0002 (clients are
// thin — they connect, render, and can die at any moment); ADR-0004 (local
// transport is the Unix socket). The CLI verbs in cmd/harness are thin wrappers
// over the typed control calls here.
package client

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/stump-wtf/harness/internal/protocol"
)

// HandshakeTimeout bounds the HELLO exchange in Dial. A daemon answers HELLO
// from the connection's own goroutine the moment it reads one, so anything
// near this long is a daemon that is not serving. Without a bound, a wedged
// daemon — the kernel still accepts into its backlog — hangs every CLI call
// and the TUI's startup forever, and the "not answering" state this package
// reports could only ever be reached by a daemon that hung up. A var so tests
// can shrink it.
var HandshakeTimeout = 10 * time.Second

// NoHandshakeError is the "socket present, nobody answering" state: the dial
// succeeded — something is bound to the socket — and then the daemon never
// sent its HELLO. It is neither "no daemon" nor a working one, and a CLI that
// calls it either is telling the operator something false (#578).
//
// Err says which of the two ways it failed: a timeout (os.ErrDeadlineExceeded,
// see TimedOut) is a peer that holds the connection and says nothing; EOF or
// EPIPE is a peer that hung up, which a daemon in the middle of shutting down
// does too.
//
// Socket is filled in by Dial, which is the layer that knows the path.
type NoHandshakeError struct {
	Socket string
	Err    error
}

func (e *NoHandshakeError) Error() string {
	where := ""
	if e.Socket != "" {
		where = " at " + e.Socket
	}
	return fmt.Sprintf("client: the daemon%s accepted the connection but did not answer the handshake: %v", where, e.Err)
}

// TimedOut reports whether the peer held the connection open without answering
// for HandshakeTimeout, as opposed to hanging up on it.
func (e *NoHandshakeError) TimedOut() bool {
	return errors.Is(e.Err, os.ErrDeadlineExceeded)
}

func (e *NoHandshakeError) Unwrap() error { return e.Err }

// Client is a connected control-plane client. It is not safe for concurrent
// control Calls (the scriptable CLI is one request at a time); the attach data
// plane uses the lower-level Conn accessor.
type Client struct {
	pc     *protocol.Conn
	raw    net.Conn
	nextID uint64
	daemon protocol.Hello
}

// Dial connects to the daemon at socketPath, performs the HELLO handshake, and
// verifies the proto major matches (SPEC-0002 REQ "Handshake And Versioning").
// wants is the subscription set (nil/empty for a one-shot CLI; ["events"] for a
// reactive TUI).
func Dial(socketPath, clientVersion string, wants []string) (*Client, error) {
	raw, err := net.Dial("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("client: dial %s: %w", socketPath, err)
	}
	c := &Client{pc: protocol.NewConn(raw), raw: raw}
	// The deadline covers the handshake only; it is cleared before the
	// connection is handed back, since an attach or events stream is
	// legitimately silent for as long as it likes.
	_ = raw.SetDeadline(time.Now().Add(HandshakeTimeout))
	if err := c.handshake(clientVersion, wants); err != nil {
		_ = raw.Close()
		var silent *NoHandshakeError
		if errors.As(err, &silent) {
			silent.Socket = socketPath
		}
		return nil, err
	}
	if err := raw.SetDeadline(time.Time{}); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("client: clearing handshake deadline: %w", err)
	}
	return c, nil
}

// handshake sends HELLO and validates the daemon's reply.
func (c *Client) handshake(clientVersion string, wants []string) error {
	hello := protocol.Hello{
		ProtoVersion:  protocol.ProtoVersion,
		ClientVersion: clientVersion,
		Wants:         wants,
	}
	// Both halves of the exchange fail the same way for the operator: the dial
	// worked, so something is bound to that socket, and it is not talking.
	// Which of the two failed is a timing detail (a peer that hangs up can
	// surface as EPIPE on the write or EOF on the read), so both carry the
	// same type — typed, not text, because the CLI branches on it to avoid
	// calling a bound daemon "not running" (#578).
	if err := c.pc.WriteJSON(protocol.TypeHello, &hello); err != nil {
		return &NoHandshakeError{Err: err}
	}
	f, err := c.pc.ReadFrame()
	if err != nil {
		return &NoHandshakeError{Err: err}
	}
	if f.Type == protocol.TypeError {
		return decodeError(f.Payload)
	}
	if f.Type != protocol.TypeHello {
		return fmt.Errorf("client: expected HELLO, got %s", f.Type)
	}
	if err := json.Unmarshal(f.Payload, &c.daemon); err != nil {
		return fmt.Errorf("client: malformed daemon HELLO: %w", err)
	}
	major, err := protocol.Major(c.daemon.ProtoVersion)
	if err != nil {
		return err
	}
	if major != protocol.ProtoMajor {
		return fmt.Errorf("client: daemon proto v%d incompatible with client proto v%d", major, protocol.ProtoMajor)
	}
	return nil
}

// Conn exposes the underlying framed connection for the attach data plane.
func (c *Client) Conn() *protocol.Conn { return c.pc }

// SetReadDeadline sets a read deadline on the underlying socket (used by
// reactive readers to bound how long they block waiting for the next frame).
func (c *Client) SetReadDeadline(t time.Time) error { return c.raw.SetReadDeadline(t) }

// DaemonVersion returns the daemon version reported in its HELLO.
func (c *Client) DaemonVersion() string { return c.daemon.DaemonVersion }

// Close closes the connection.
func (c *Client) Close() error { return c.raw.Close() }

// call sends a control request and returns the matching CONTROL_RESP, turning a
// structured ERROR into a *protocol.ErrorMsg. Interleaved PING/EVENT frames are
// handled transparently so a subscribed client can still make control calls.
func (c *Client) call(req protocol.ControlReq) (protocol.ControlResp, error) {
	c.nextID++
	req.ID = c.nextID
	if err := c.pc.WriteJSON(protocol.TypeControlReq, &req); err != nil {
		return protocol.ControlResp{}, err
	}
	for {
		f, err := c.pc.ReadFrame()
		if err != nil {
			return protocol.ControlResp{}, err
		}
		switch f.Type {
		case protocol.TypeControlResp:
			var resp protocol.ControlResp
			if err := json.Unmarshal(f.Payload, &resp); err != nil {
				return protocol.ControlResp{}, err
			}
			if resp.ID != req.ID {
				continue // not ours (shouldn't happen on a serial client)
			}
			return resp, nil
		case protocol.TypeError:
			e := errorFrom(f.Payload)
			if e.ID != 0 && e.ID != req.ID {
				continue
			}
			return protocol.ControlResp{}, e
		case protocol.TypePing:
			_ = c.pc.WriteFrame(protocol.TypePong, nil)
		case protocol.TypeEvent, protocol.TypePong, protocol.TypeAttachData:
			// Not relevant to this control call; skip.
		default:
			// Ignore unexpected frames rather than desync.
		}
	}
}

// decodeError parses an ERROR payload into a *protocol.ErrorMsg error.
func decodeError(payload []byte) error { return errorFrom(payload) }

// errorFrom parses an ERROR payload, always returning a non-nil *ErrorMsg.
func errorFrom(payload []byte) *protocol.ErrorMsg {
	e := &protocol.ErrorMsg{}
	if err := json.Unmarshal(payload, e); err != nil {
		e.Code = protocol.ErrInternal
		e.Message = fmt.Sprintf("malformed error frame: %v", err)
	}
	return e
}

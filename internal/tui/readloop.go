package tui

// Governing: SPEC-0002 REQ "Event Subscription" / "Attach Session" / "Message
// Framing"; SPEC-0001 (the reactive TUI). CRITICAL invariant (from the client's
// SHARP EDGE): Client.call() — every typed control op — transparently steps over
// interleaved EVENT/PING/ATTACH_DATA frames while awaiting its CONTROL_RESP, so
// mixing control calls with an event/attach read loop on the SAME Conn swallows
// async frames. We therefore run exactly ONE ReadFrame loop over the
// events+attach connection here, dispatching purely by frame type, and keep the
// typed control helpers on a SEPARATE connection (see model.go). This goroutine
// is the only reader of its Conn.

import (
	"encoding/json"

	tea "github.com/charmbracelet/bubbletea"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// --- async messages the read loop feeds into the Bubble Tea update cycle ---

// eventMsg carries a pushed lifecycle EVENT (state change, exit, flapping,
// config reload, profile change).
type eventMsg struct{ ev protocol.EventMsg }

// attachDataMsg carries raw terminal bytes for one attach session.
type attachDataMsg struct {
	sessionID uint32
	data      []byte
}

// attachErrorMsg carries a structured ERROR that arrived on the events/attach
// connection (e.g. an attach to an unknown harness).
type attachErrorMsg struct{ err *protocol.ErrorMsg }

// disconnectMsg means the events/attach connection dropped — the daemon may be
// fine (ADR-0002); the TUI shows the reconnecting overlay.
type disconnectMsg struct{ err error }

// frameSource is the minimal read/write surface the loop needs — satisfied by
// *protocol.Conn and by a pipe in tests.
type frameSource interface {
	ReadFrame() (protocol.Frame, error)
	WriteFrame(protocol.Type, []byte) error
}

// runReadLoop is the single dispatch loop. It reads frames until an error, and
// for each frame emits at most one tea.Msg onto out (respecting done). PING is
// answered with PONG inline and produces no message. It closes over out but
// never closes it — the caller owns the channel's lifetime.
func runReadLoop(conn frameSource, out chan<- tea.Msg, done <-chan struct{}) {
	for {
		f, err := conn.ReadFrame()
		if err != nil {
			emit(out, done, disconnectMsg{err: err})
			return
		}
		switch f.Type {
		case protocol.TypeEvent:
			var ev protocol.EventMsg
			if json.Unmarshal(f.Payload, &ev) == nil {
				if !emit(out, done, eventMsg{ev: ev}) {
					return
				}
			}
		case protocol.TypeAttachData:
			sid, rest, derr := protocol.DecodeAttach(f.Payload)
			if derr == nil {
				// Copy: the frame's backing buffer is reused by the next read.
				cp := make([]byte, len(rest))
				copy(cp, rest)
				if !emit(out, done, attachDataMsg{sessionID: sid, data: cp}) {
					return
				}
			}
		case protocol.TypePing:
			_ = conn.WriteFrame(protocol.TypePong, nil)
		case protocol.TypeError:
			em := &protocol.ErrorMsg{}
			if json.Unmarshal(f.Payload, em) != nil {
				em.Code = protocol.ErrInternal
				em.Message = "unparseable error frame"
			}
			if !emit(out, done, attachErrorMsg{err: em}) {
				return
			}
		default:
			// CONTROL_RESP/PONG/HELLO are not expected on this connection; ignore
			// rather than desync (the control plane runs on its own conn).
		}
		select {
		case <-done:
			return
		default:
		}
	}
}

// emit sends msg on out unless done fires first. Returns false if done fired
// (the caller should stop).
func emit(out chan<- tea.Msg, done <-chan struct{}, msg tea.Msg) bool {
	select {
	case out <- msg:
		return true
	case <-done:
		return false
	}
}

// waitForFrame is the Bubble Tea Cmd that pulls the next async message off the
// read-loop channel. After handling the returned msg, the model re-issues this
// Cmd to keep draining — the standard bubbletea channel-subscription pattern.
func waitForFrame(ch <-chan tea.Msg) tea.Cmd {
	return func() tea.Msg {
		msg, ok := <-ch
		if !ok {
			return disconnectMsg{err: errChannelClosed}
		}
		return msg
	}
}

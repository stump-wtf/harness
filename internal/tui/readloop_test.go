package tui

import (
	"encoding/json"
	"net"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// pipeConns returns two protocol.Conns wired back-to-back over an in-memory
// pipe: a "daemon" side and a "client" side.
func pipeConns(t *testing.T) (daemon, clientc *protocol.Conn, closeFn func()) {
	t.Helper()
	a, b := net.Pipe()
	return protocol.NewConn(a), protocol.NewConn(b), func() { _ = a.Close(); _ = b.Close() }
}

// TestReadLoopDispatchesByType verifies the SINGLE read loop (the critical
// concurrency invariant from readloop.go): it dispatches EVENT and ATTACH_DATA
// to typed messages and answers PING with PONG inline — so a subscribed TUI
// never has to mix control calls with the async read path on one conn.
func TestReadLoopDispatchesByType(t *testing.T) {
	daemon, clientc, closeFn := pipeConns(t)
	defer closeFn()

	out := make(chan tea.Msg, 8)
	done := make(chan struct{})
	go runReadLoop(clientc, out, done)

	// 1) EVENT frame → eventMsg.
	ev, _ := json.Marshal(protocol.EventMsg{Kind: protocol.EvStateChanged, Name: "crush", To: "running"})
	if err := daemon.WriteFrame(protocol.TypeEvent, ev); err != nil {
		t.Fatalf("write event: %v", err)
	}
	select {
	case msg := <-out:
		em, ok := msg.(eventMsg)
		if !ok || em.ev.Name != "crush" || em.ev.To != "running" {
			t.Fatalf("expected eventMsg for crush→running, got %#v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for eventMsg")
	}

	// 2) ATTACH_DATA frame → attachDataMsg with the right session + bytes.
	if err := daemon.WriteFrame(protocol.TypeAttachData, protocol.EncodeAttach(7, []byte("hi"))); err != nil {
		t.Fatalf("write attach data: %v", err)
	}
	select {
	case msg := <-out:
		ad, ok := msg.(attachDataMsg)
		if !ok || ad.sessionID != 7 || string(ad.data) != "hi" {
			t.Fatalf("expected attachDataMsg{7,\"hi\"}, got %#v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for attachDataMsg")
	}

	// 3) PING → the loop answers PONG inline (no message emitted).
	if err := daemon.WriteFrame(protocol.TypePing, nil); err != nil {
		t.Fatalf("write ping: %v", err)
	}
	f, err := daemon.ReadFrame()
	if err != nil {
		t.Fatalf("reading pong: %v", err)
	}
	if f.Type != protocol.TypePong {
		t.Fatalf("ping was not answered with PONG, got %s", f.Type)
	}

	close(done)
}

// TestReadLoopDisconnect verifies a closed connection surfaces a disconnectMsg
// (drives the reconnecting overlay, ADR-0002).
func TestReadLoopDisconnect(t *testing.T) {
	daemon, clientc, closeFn := pipeConns(t)

	out := make(chan tea.Msg, 1)
	done := make(chan struct{})
	defer close(done)
	go runReadLoop(clientc, out, done)

	closeFn() // drop the connection
	_ = daemon

	select {
	case msg := <-out:
		if _, ok := msg.(disconnectMsg); !ok {
			t.Fatalf("expected disconnectMsg, got %#v", msg)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for disconnectMsg")
	}
}

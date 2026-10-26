package tui

// Governing: SPEC-0001 scenario "Driving a live agent". Issue #952 (upstream
// stump-wtf/harness#27): typed order must survive the trip to the guest.
// Bubble Tea v2 runs every tea.Cmd it gets back from Update in its own
// goroutine, so a keystroke whose Cmd writes straight to the wire races with
// the keystroke typed just before it. These tests drive Update the way the real
// event loop does, serially, and run each returned Cmd concurrently, the way
// Bubble Tea does, so an implementation that writes from the Cmd itself fails
// and one that drains an Update-owned queue passes.

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/stump-wtf/harness/internal/protocol"
)

// wireRecorder is an attachConn that records attach input in the order the
// writes actually executed, which is the only order the guest can see. The
// optional gate lets a test hold one write in flight, the way a slow wire
// would, so the buffered frames behind it are observable.
type wireRecorder struct {
	mu   sync.Mutex
	wire bytes.Buffer
	sids []uint32

	gate    chan struct{} // when non-nil, the first write waits here first
	entered chan struct{} // closed when that write is on the wire
	gateOne sync.Once     // hold only the first write, so the rest stay queued
}

func (w *wireRecorder) AttachInput(sid uint32, data []byte) error {
	if w.gate != nil {
		w.gateOne.Do(func() {
			close(w.entered)
			<-w.gate
		})
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.wire.Write(data)
	w.sids = append(w.sids, sid)
	return nil
}

func (w *wireRecorder) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.wire.String()
}

func (w *wireRecorder) AttachOpen(uint32, string, int, int, protocol.AttachMode) error {
	return nil
}
func (w *wireRecorder) AttachResize(uint32, int, int) error { return nil }
func (w *wireRecorder) AttachClose(uint32) error            { return nil }
func (w *wireRecorder) Conn() *protocol.Conn                { return nil }
func (w *wireRecorder) Close() error                        { return nil }

// attachedRecorder returns a read-write attached Model wired to a fresh
// wireRecorder.
func attachedRecorder() (*Model, *wireRecorder) {
	m, _ := attachedFake(80, 24)
	w := &wireRecorder{entered: make(chan struct{})}
	m.attach = w
	return m, w
}

// TestAttachInputKeepsTypedOrder is the regression test for #952: 600 typed
// keys with a bracketed paste every hundredth, each returned Cmd launched on
// its own goroutine. The guest must receive exactly what was typed, in order.
func TestAttachInputKeepsTypedOrder(t *testing.T) {
	m, w := attachedRecorder()
	sid := m.att.sessionID

	const keys = 600
	var typed []tea.Msg
	var want strings.Builder
	for i := 0; i < keys; i++ {
		if i%100 == 99 {
			paste := fmt.Sprintf("<paste%d>", i)
			typed = append(typed, tea.PasteMsg{Content: paste})
			want.WriteString("\x1b[200~" + paste + "\x1b[201~")
			continue
		}
		r := string(rune('a' + i%26))
		typed = append(typed, runeKey(r))
		want.WriteString(r)
	}

	var wg sync.WaitGroup
	for _, msg := range typed {
		_, cmd := m.Update(msg)
		if cmd == nil {
			continue
		}
		wg.Add(1)
		go func(c tea.Cmd) {
			defer wg.Done()
			c()
		}(cmd)
	}
	wg.Wait()

	if got, expected := w.String(), want.String(); got != expected {
		n := 0
		for n < len(got) && n < len(expected) && got[n] == expected[n] {
			n++
		}
		t.Fatalf("wire order diverges at byte %d of %d: got %q, want %q",
			n, len(expected), truncateForReport(got), truncateForReport(expected))
	}
	for _, got := range w.sids {
		if got != sid {
			t.Fatalf("input written for session %d, want %d", got, sid)
		}
	}
}

func truncateForReport(s string) string {
	if len(s) > 48 {
		return s[:48] + "…"
	}
	return s
}

// TestAttachInputStartsOneDrainPerBurst: a burst must not launch a writer per
// keystroke. With the first write still on the wire, the rest of the burst has
// to be buffered rather than handed to Bubble Tea as extra Cmds.
func TestAttachInputStartsOneDrainPerBurst(t *testing.T) {
	m, w := attachedRecorder()
	w.gate = make(chan struct{})

	var cmds []tea.Cmd
	for _, r := range "abcdef" {
		if _, cmd := m.Update(runeKey(string(r))); cmd != nil {
			cmds = append(cmds, cmd)
		}
	}
	if len(cmds) != 1 {
		close(w.gate)
		t.Fatalf("burst of 6 keys started %d drain Cmds, want 1", len(cmds))
	}

	done := make(chan struct{})
	go func() {
		cmds[0]()
		close(done)
	}()
	close(w.gate)
	<-done

	if got, want := w.String(), "abcdef"; got != want {
		t.Fatalf("wire = %q, want %q", got, want)
	}
}

// TestAttachInputDropOnDetach: keystrokes buffered behind a stalled write
// belong to the session being left. After a detach they must not be flushed
// into whatever the client attaches to next.
func TestAttachInputDropOnDetach(t *testing.T) {
	m, w := attachedRecorder()
	w.gate = make(chan struct{})

	_, first := m.Update(runeKey("x"))
	if first == nil {
		close(w.gate)
		t.Fatal("first keystroke should hand back a drain Cmd")
	}
	if _, cmd := m.Update(runeKey("y")); cmd != nil {
		close(w.gate)
		t.Fatal("a keystroke behind an in-flight write must not start a second drain")
	}

	done := make(chan struct{})
	go func() {
		first()
		close(done)
	}()
	<-w.entered // "x" is on the wire, so "y" is the buffered one
	drain(m.detach())
	close(w.gate)
	<-done

	if got, want := w.String(), "x"; got != want {
		t.Fatalf("wire after detach = %q, want %q (buffered input must not outlive the session)", got, want)
	}
}

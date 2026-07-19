package attach

// Governing: SPEC-0002 REQ "Attach Session" (snapshot → scrollback tail → live;
// smallest-attached-wins resize; ro ignores input; close tears down only that
// session) and REQ "Backpressure Isolation" (the PTY reader MUST NOT block on
// any client; bounded per-session queues; overflow coalesces to a fresh
// snapshot); ADR-0003 (one x/vt emulator per harness; resize policy); ADR-0007
// (ring + backpressure); ADR-0008 (read-only attach).

import (
	"sync"

	"github.com/charmbracelet/x/vt"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// Default emulator size before any client attaches (ADR-0003: a fresh PTY is
// 80×24 until an attach resizes it).
const (
	defaultCols = 80
	defaultRows = 24
)

// queueCap bounds each attach session's outbound queue (in chunks). When a slow
// client cannot drain it, the next fan-out coalesces the queue down to a single
// fresh snapshot (SPEC-0002 REQ "Backpressure Isolation"). It is deliberately
// modest: a client that falls this far behind is better served by one repaint
// than a long backlog.
const queueCap = 256

// Mux is the per-harness data-plane hub: the x/vt emulator + scrollback ring
// fed by raw PTY output, plus the set of live attach sessions. It implements
// io.Writer so the supervisor's ExtraOut hook tees raw PTY bytes straight into
// it (alongside the durable log). Every field is guarded by mu; the single PTY
// reader goroutine (Write) and the many connection goroutines (Attach/Resize/
// Detach) all serialize through it, and Write never blocks on a client.
type Mux struct {
	name     string
	onResize func(cols, rows int)
	onInput  func(p []byte)

	mu         sync.Mutex
	term       *vt.Terminal
	ring       *ring
	cols, rows int
	sessions   map[*Session]struct{}
}

// newMux builds a Mux for a harness. onResize is invoked when the
// smallest-attached-wins size changes (to resize the real PTY); onInput
// delivers read-write attach keystrokes to the PTY. Either may be nil.
func newMux(name string, ringLines int, onResize func(cols, rows int), onInput func(p []byte)) *Mux {
	return &Mux{
		name:     name,
		onResize: onResize,
		onInput:  onInput,
		term:     vt.NewTerminal(defaultCols, defaultRows),
		ring:     newRing(ringLines),
		cols:     defaultCols,
		rows:     defaultRows,
		sessions: make(map[*Session]struct{}),
	}
}

// Name returns the harness name this mux serves.
func (m *Mux) Name() string { return m.name }

// Write feeds raw PTY bytes into the emulator, the scrollback ring, and every
// attached session's bounded queue. It is the supervisor's ExtraOut sink. It
// MUST NOT block on any client (SPEC-0002 REQ "Backpressure Isolation"): all
// per-session sends are non-blocking and overflow coalesces to a snapshot.
func (m *Mux) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, _ = m.term.Write(p)
	m.ring.Write(p)
	if len(m.sessions) > 0 {
		// io.Copy reuses its buffer, so the fan-out slice must be a private
		// copy the sessions can hold and read after we return. One copy is
		// shared read-only across all sessions.
		cp := make([]byte, len(p))
		copy(cp, p)
		for s := range m.sessions {
			s.enqueueLocked(cp)
		}
	}
	return len(p), nil
}

// Attach opens a new session (SPEC-0002 REQ "Attach Session"). It queues, in
// order and before any live byte can reach this session, the current screen
// snapshot then a bounded scrollback tail, then joins the live fan-out — all
// under mu, so no Write can interleave a live chunk ahead of the snapshot. That
// is the "full screen snapshot before any live bytes" guarantee. write sends one
// ATTACH_DATA payload to the client; it may block (a slow client), which is
// precisely what triggers coalescing without ever stalling Write.
func (m *Mux) Attach(id uint32, mode protocol.AttachMode, cols, rows int, write func([]byte) error) *Session {
	s := &Session{
		id:     id,
		mode:   mode,
		mux:    m,
		cols:   cols,
		rows:   rows,
		write:  write,
		out:    make(chan []byte, queueCap),
		closed: make(chan struct{}),
	}
	m.mu.Lock()
	s.enqueueLocked(renderScreen(m.term)) // 1. screen snapshot
	if tail := m.ring.Tail(); len(tail) > 0 {
		s.enqueueLocked(tail) // 2. bounded scrollback tail
	}
	m.sessions[s] = struct{}{}
	m.applyResizeLocked() // recompute smallest-attached-wins with this client
	m.mu.Unlock()

	s.wg.Add(1)
	go s.pump() // 3. live stream drains from here on
	return s
}

// Resize records a session's new viewport and re-applies the
// smallest-attached-wins policy (ADR-0003).
func (m *Mux) Resize(s *Session, cols, rows int) {
	m.mu.Lock()
	s.cols, s.rows = cols, rows
	m.applyResizeLocked()
	m.mu.Unlock()
}

// Detach tears down a single session and re-applies the resize policy for the
// clients that remain — the harness and other sessions are untouched (SPEC-0002
// REQ "Attach Session"). Safe to call more than once.
func (m *Mux) Detach(s *Session) {
	m.mu.Lock()
	if _, ok := m.sessions[s]; ok {
		delete(m.sessions, s)
		m.applyResizeLocked()
	}
	m.mu.Unlock()
	s.stop()
}

// Input forwards a read-write session's keystrokes to the PTY; a read-only
// session discards them so the PTY never sees the input (ADR-0008).
func (m *Mux) Input(s *Session, p []byte) {
	if s.mode == protocol.AttachRO {
		return // dropped: the PTY never sees read-only input
	}
	if m.onInput != nil {
		m.onInput(p)
	}
}

// SessionCount reports the number of live sessions (for tests).
func (m *Mux) SessionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// Size returns the current authoritative emulator size (for tests).
func (m *Mux) Size() (cols, rows int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cols, m.rows
}

// renderSnapshotLocked renders the current screen. Caller holds mu.
func (m *Mux) renderSnapshotLocked() []byte { return renderScreen(m.term) }

// applyResizeLocked recomputes the authoritative PTY size as the smallest
// viewport across attached sessions and, if it changed, resizes the emulator
// and the real PTY (ADR-0003 "the smallest attached viewport wins"). With no
// sessions the last size is retained. Caller holds mu.
func (m *Mux) applyResizeLocked() {
	minC, minR := 0, 0
	first := true
	for s := range m.sessions {
		if s.cols <= 0 || s.rows <= 0 {
			continue
		}
		if first {
			minC, minR, first = s.cols, s.rows, false
			continue
		}
		if s.cols < minC {
			minC = s.cols
		}
		if s.rows < minR {
			minR = s.rows
		}
	}
	if first { // no session with a valid size
		return
	}
	if minC == m.cols && minR == m.rows {
		return
	}
	m.cols, m.rows = minC, minR
	m.term.Resize(minC, minR)
	if m.onResize != nil {
		m.onResize(minC, minR)
	}
}

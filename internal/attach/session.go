package attach

// Governing: SPEC-0002 REQ "Attach Session" and REQ "Backpressure Isolation" —
// each attach session has a bounded outbound queue; a slow client that cannot
// drain it has its queued increments coalesced into a single fresh snapshot so
// it repaints to the true screen rather than replaying a stale backlog, and the
// PTY reader (Mux.Write) is never stalled. ADR-0008 (read-only sessions).

import (
	"sync"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// Session is one client's attach to one harness. The client-chosen id tags the
// ATTACH_DATA frames on that client's connection so several sessions multiplex
// it. out is the bounded queue; a dedicated pump goroutine drains it to the
// client via write. Only the pump calls write, so frames for this session never
// interleave with themselves.
type Session struct {
	id    uint32
	mode  protocol.AttachMode
	mux   *Mux
	write func([]byte) error

	// cols/rows are guarded by mux.mu (set in Attach/Resize, read in
	// applyResizeLocked).
	cols, rows int

	out    chan []byte
	closed chan struct{}
	once   sync.Once
	wg     sync.WaitGroup
}

// ID returns the session's client-chosen id.
func (s *Session) ID() uint32 { return s.id }

// Input forwards client keystrokes to the harness PTY, dropping them for a
// read-only session (ADR-0008).
func (s *Session) Input(p []byte) { s.mux.Input(s, p) }

// Resize applies a new client viewport, re-running the smallest-attached-wins
// policy (ADR-0003).
func (s *Session) Resize(cols, rows int) { s.mux.Resize(s, cols, rows) }

// Detach tears down just this session; the harness and other sessions continue.
func (s *Session) Detach() { s.mux.Detach(s) }

// enqueueLocked queues one outbound chunk without blocking (caller holds
// mux.mu, so the PTY reader is never stalled). On overflow it coalesces: the
// queued increments are dropped and replaced by a single fresh screen snapshot,
// so the slow client repaints to the true screen instead of the backlog
// (SPEC-0002 REQ "Backpressure Isolation").
func (s *Session) enqueueLocked(chunk []byte) {
	select {
	case s.out <- chunk:
		return
	default:
	}
	// Queue full: drop the backlog, substitute a snapshot.
	s.drain()
	snap := s.mux.renderSnapshotLocked() // mux.mu held by caller
	select {
	case s.out <- snap:
	default:
		// The pump is wedged mid-write on the socket; even the snapshot won't
		// fit. Dropping is safe — the next fan-out coalesces again, and PING
		// timeouts eventually reap a truly dead client.
	}
}

// drain empties the outbound queue non-blockingly. The pump may be concurrently
// receiving; both are ordinary channel ops and safe.
func (s *Session) drain() {
	for {
		select {
		case <-s.out:
		default:
			return
		}
	}
}

// pump drains the outbound queue to the client. A write error means the client
// is gone: it detaches itself so the harness and other sessions carry on.
func (s *Session) pump() {
	defer s.wg.Done()
	for {
		select {
		case chunk := <-s.out:
			if err := s.write(chunk); err != nil {
				// Client is gone. Detach asynchronously so this method never
				// depends on its own goroutine (keeps it safe if Detach ever
				// grows a wg.Wait); Detach is idempotent with an explicit close.
				go s.mux.Detach(s)
				return
			}
		case <-s.closed:
			return
		}
	}
}

// stop signals the pump to exit. Idempotent. It does not wait for a blocked
// socket write to return; the connection teardown unblocks that.
func (s *Session) stop() {
	s.once.Do(func() { close(s.closed) })
}

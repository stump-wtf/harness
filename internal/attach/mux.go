package attach

// Governing: SPEC-0002 REQ "Attach Session" (snapshot → scrollback tail → live;
// smallest-attached-wins resize; ro ignores input; close tears down only that
// session) and REQ "Backpressure Isolation" (the PTY reader MUST NOT block on
// any client; bounded per-session queues; overflow coalesces to a fresh
// snapshot); ADR-0003 (one x/vt emulator per harness; resize policy); ADR-0007
// (byte-bounded ring + backpressure); ADR-0008 (read-only attach).

import (
	"bytes"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"

	"github.com/stump-wtf/harness/internal/mousectl"
	"github.com/stump-wtf/harness/internal/protocol"
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

// A fresh session's queue must hold the snapshot plus a full scrollback replay,
// or Attach itself would trip the coalescing meant for slow clients and drop
// the tail it just queued. This fails to compile if the two drift apart.
var _ [queueCap - 1 - maxTailFrames]struct{}

// The bracketed-paste brackets a client wraps a paste in (DECSET ?2004).
var (
	bracketedPasteStart = []byte("\x1b[200~")
	bracketedPasteEnd   = []byte("\x1b[201~")
)

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
	// nudgeDelays is this Mux's copy of the SIGWINCH re-assert schedule, taken
	// at construction so the goroutine never reads the package var (see
	// newMux).
	nudgeDelays []time.Duration
	// onNudge re-delivers SIGWINCH to the guest's process group (#182). It may
	// be nil (tests).
	onNudge func()

	mu       sync.Mutex
	term     vt.Terminal
	ring     *ring
	sessions map[*Session]struct{}
	// lastWrite is when the emulator last received PTY bytes. It is what
	// "no output change" means for stuck-prompt detection (issue #735): the
	// screen itself can look unchanged across a repaint that wrote identical
	// cells, but a repaint is still activity, and bytes are the only honest
	// record of it. Zero until the first Write.
	lastWrite time.Time
	// bracketedPaste shadows the guest's DECSET ?2004 state, which x/vt tracks
	// but exposes no reader for (the same reason vtView shadows DECTCEM).
	// Guarded by mu: written from the EnableMode/DisableMode callbacks, which
	// fire under term.Write in Mux.Write, and read in Input.
	bracketedPaste bool
	// mouse shadows the guest's mouse-reporting DEC modes for the same reason
	// bracketedPaste does: a client attaches to a screen snapshot, so a guest
	// that enabled the mouse before the attach would otherwise stay invisible to
	// it, and the client would keep the wheel for its own scrollback while the
	// guest is waiting for events. Guarded by mu; serialized into every
	// snapshot. Governing: ADR-0055.
	mouse mousectl.Shadow

	// sizeMu guards the authoritative viewport, and only that. It is
	// deliberately not mu: the supervisor's actor loop reads the size (through
	// Registry.SizeFor) to born-size a freshly spawned PTY, while onResize is
	// invoked from *under* mu and blocks until that same actor loop drains the
	// resize command. Reading the size under mu would close the cycle — a
	// client resize landing while the harness is (re)starting would wedge the
	// supervisor and this mux against each other for good, taking the PTY
	// reader down with them (ADR-0003).
	sizeMu     sync.Mutex
	cols, rows int
}

// newMux builds a Mux whose ring keeps up to ringLines lines within the default
// byte budget; see newMuxLimits.
func newMux(name string, ringLines int, onResize func(cols, rows int), onInput func(p []byte), onNudge func()) *Mux {
	return newMuxLimits(name, RingLimits{Lines: ringLines}, onResize, onInput, onNudge)
}

// newMuxLimits builds a Mux for a harness, its scrollback ring bounded by lim.
// onResize is invoked when the smallest-attached-wins size changes (to resize
// the real PTY); onInput delivers read-write attach keystrokes to the PTY;
// onNudge re-delivers SIGWINCH to the guest's process group (see
// reassertWinch). Any may be nil.
func newMuxLimits(name string, lim RingLimits, onResize func(cols, rows int), onInput func(p []byte), onNudge func()) *Mux {
	m := &Mux{
		name:     name,
		onResize: onResize,
		onInput:  onInput,
		onNudge:  onNudge,
		// Captured per-Mux at construction rather than read from the package
		// var inside the goroutine: reassertWinch outlives the caller by the
		// length of the whole schedule, so a test that shrinks the global for
		// determinism would otherwise be writing it while a previous test's
		// nudge goroutine is still ranging over it (a real -race failure, and
		// an intermittent one, since it depends on which goroutines are still
		// asleep).
		nudgeDelays: winchNudgeDelays,
		term:        newEmulator(defaultCols, defaultRows),
		ring:        newRing(lim),
		cols:        defaultCols,
		rows:        defaultRows,
		sessions:    make(map[*Session]struct{}),
	}
	// Nothing reads this emulator's scrollback: attach replays history from
	// the raw-byte ring, and a snapshot renders only the screen. x/vt's
	// default keeps 10,000 rows of 112-byte cells, ~85 MiB per harness at 80
	// columns, so cap it (x/vt's minimum is 1; the alternate screen has no
	// public knob). Governing: SPEC-0002 REQ "Emulator Memory".
	m.term.SetScrollbackSize(1)
	// Shadow the guest's bracketed-paste mode. Only the emulator on THIS side
	// ever sees the guest's ?2004h: a client attaches to a screen snapshot
	// (renderScreen emits cells, cursor and SGR — no modes), so a client-side
	// emulator has no way to learn that an agent TUI which started before the
	// attach has the mode on. See Input.
	if e, ok := m.term.(*vt.Emulator); ok {
		e.SetCallbacks(vt.Callbacks{
			EnableMode: func(mode ansi.Mode) {
				if mode == ansi.ModeBracketedPaste {
					m.bracketedPaste = true
					return
				}
				m.mouse.Note(mode, true)
			},
			DisableMode: func(mode ansi.Mode) {
				if mode == ansi.ModeBracketedPaste {
					m.bracketedPaste = false
					return
				}
				m.mouse.Note(mode, false)
			},
		})
	}
	go m.pumpReplies()
	return m
}

// pumpReplies drains synthesized replies the vt emulator writes back when it
// auto-answers a query the child sent it — DECRQM mode reports (CSI ? Ps $ p,
// e.g. the ?2026 synchronized-output probe nearly every Bubble Tea app sends
// in its first frame), cursor position reports, and the like — and forwards
// them through onInput, the same seam a read-write attach session's real
// keystrokes already use to reach the child's PTY. A real terminal delivers
// query replies and typed input on the same channel; this is that channel's
// other producer.
//
// x/vt synthesizes these replies by writing into an internal io.Pipe and
// expects the caller to drain it via Read (Emulator.Read). That pipe is
// unbuffered: a write blocks until something reads the paired end. Without
// this goroutine, nothing ever does — so the first query the child emits
// blocks Write() forever, and Write() is exactly what readOutput's io.Copy
// calls on every byte the child writes to its terminal. That stops draining
// the real PTY, which backpressures the child's own stdout writes solid,
// with the OS process still alive — invisible to a liveness check like
// `harness list` (stump.wtf/harness#142).
//
// Runs until Registry.Remove drops the Mux and calls releaseReplies, which
// closes the emulator's input pipe so the Read below returns EOF. Before
// that existed the loop never returned, and every Mux ever removed stayed
// reachable through its parked goroutine.
func (m *Mux) pumpReplies() {
	buf := make([]byte, 4096)
	for {
		n, err := m.term.Read(buf)
		if n > 0 && m.onInput != nil {
			m.onInput(append([]byte(nil), buf[:n]...))
		}
		if err != nil {
			return
		}
	}
}

// releaseReplies ends pumpReplies by closing the emulator's input pipe: the
// pump's parked Read returns EOF, and a reply the emulator synthesizes later
// fails at once instead of blocking Write. It deliberately avoids
// Emulator.Close, whose unsynchronised `closed` flag races the parked Read
// (stump.wtf/harness#142). No lock is needed: m.term is never reassigned, and
// io.Pipe is safe to close from any goroutine. Safe to call more than once.
//
// Governing: SPEC-0002 REQ "Emulator Memory"; ADR-0003.
func (m *Mux) releaseReplies() {
	if c, ok := m.term.InputPipe().(io.Closer); ok {
		_ = c.Close()
	}
}

// winchNudgeDelays is the decaying SIGWINCH re-assert schedule armed by every
// Attach (stump.wtf/harness#182). The kernel only raises SIGWINCH on an actual
// dimension change, so the resize an attach applies is silently lost when the
// guest has not installed a handler yet — which for a real agent TUI (LSPs,
// MCP servers booting for seconds) is exactly when attaches land. The sizes
// are already correct by then; only the notification was missed, so the nudge
// re-delivers it to the process group (never the bare pid: crush/claude run
// their renderer as a child sharing the group).
//
// Deliberately generous rather than clever: an extra SIGWINCH is harmless by
// design (default-ignored without a handler; a running TUI re-reads a size
// that is already right and repaints nothing), so the schedule simply spans
// the boot window instead of trying to detect readiness.
var winchNudgeDelays = []time.Duration{
	300 * time.Millisecond,
	900 * time.Millisecond,
	2 * time.Second,
	4 * time.Second,
}

// reassertWinch fires onNudge on the nudge schedule. Bounded, so a Mux that is
// never closed still leaks nothing.
func (m *Mux) reassertWinch() {
	for _, d := range m.nudgeDelays {
		time.Sleep(d)
		if m.onNudge == nil {
			return
		}
		m.onNudge()
	}
}

// Name returns the harness name this mux serves.
func (m *Mux) Name() string { return m.name }

// SessionInfo describes one live attach session for visibility (#183): who is
// attached, at what viewport, for how long, and whether it is a session the
// smallest-attached-wins minimum currently comes from.
type SessionInfo struct {
	ID        uint32
	Mode      protocol.AttachMode
	Cols      int
	Rows      int
	CreatedAt time.Time
	// SetsMin marks a session whose viewport defines the current minimum on
	// at least one axis — the row that answers "why is my guest 80 columns
	// wide?" without lsof on the daemon and stty on the guest's tty.
	SetsMin bool
}

// MuxSnapshot is a point-in-time view of a Mux for `describe` (#183): the
// authoritative viewport plus every live session.
type MuxSnapshot struct {
	Cols     int
	Rows     int
	Sessions []SessionInfo
}

// Snapshot returns the authoritative viewport and the live sessions with the
// minimum-setter flagged. The minimum is recomputed here exactly as
// applyResizeLocked computes it, under the same lock, so the flag can never
// disagree with the policy that actually resized the PTY.
func (m *Mux) Snapshot() MuxSnapshot {
	m.mu.Lock()
	minC, minR := 0, 0
	first := true
	for s := range m.sessions {
		if s.cols <= 0 || s.rows <= 0 {
			continue
		}
		if first || s.cols < minC {
			minC = s.cols
		}
		if first || s.rows < minR {
			minR = s.rows
		}
		first = false
	}
	sessions := make([]SessionInfo, 0, len(m.sessions))
	for s := range m.sessions {
		sessions = append(sessions, SessionInfo{
			ID:        s.id,
			Mode:      s.mode,
			Cols:      s.cols,
			Rows:      s.rows,
			CreatedAt: s.createdAt,
			SetsMin:   !first && (s.cols == minC || s.rows == minR),
		})
	}
	m.mu.Unlock()

	sort.Slice(sessions, func(i, j int) bool { return sessions[i].ID < sessions[j].ID })
	cols, rows := m.Size()
	return MuxSnapshot{Cols: cols, Rows: rows, Sessions: sessions}
}

// Write feeds raw PTY bytes into the emulator, the scrollback ring, and every
// attached session's bounded queue. It is the supervisor's ExtraOut sink. It
// MUST NOT block on any client (SPEC-0002 REQ "Backpressure Isolation"): all
// per-session sends are non-blocking and overflow coalesces to a snapshot.
func (m *Mux) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, _ = m.term.Write(p)
	m.lastWrite = time.Now()
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
// snapshot then the scrollback tail, then joins the live fan-out — all under
// mu, so no Write can interleave a live chunk ahead of the snapshot or the
// tail. That is the "full screen snapshot before any live bytes" guarantee.
// write sends one ATTACH_DATA payload to the client; it may block (a slow
// client), which is precisely what triggers coalescing without ever stalling
// Write.
//
// The tail is queued as views into the ring's storage, at most one storage
// chunk each (ring.tailFrames), never as one copy of the whole ring. The copy
// cost a ring-sized allocation per attach, and a tail past 16 MiB could not be
// sent at all: it exceeded protocol.MaxFrameSize, the write failed, and the
// session tore itself down straight after the snapshot. The frames share the
// session's bounded queue with live output, so a client too slow to take them
// is coalesced to a fresh snapshot like any other (SPEC-0002 REQ "Backpressure
// Isolation").
func (m *Mux) Attach(id uint32, mode protocol.AttachMode, cols, rows int, write func([]byte) error) *Session {
	s := &Session{
		id:        id,
		mode:      mode,
		mux:       m,
		cols:      cols,
		rows:      rows,
		write:     write,
		out:       make(chan []byte, queueCap),
		closed:    make(chan struct{}),
		createdAt: time.Now(),
	}
	m.mu.Lock()
	s.enqueueLocked(m.snapshotLocked()) // 1. screen snapshot + the modes it cannot show
	for _, f := range m.ring.tailFrames() {
		s.enqueueLocked(f) // 2. scrollback tail, one storage chunk per frame
	}
	m.sessions[s] = struct{}{}
	m.applyResizeLocked() // recompute smallest-attached-wins with this client
	m.mu.Unlock()

	s.wg.Add(1)
	go s.pump() // 3. live stream drains from here on
	// 4. Re-assert the size over the guest's boot window (#182): this attach
	// just applied smallest-attached-wins, and the kernel's SIGWINCH for it is
	// lost if the guest has no handler installed yet.
	go m.reassertWinch()
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
	p = m.unwrapPasteIfUnsupported(p)
	if m.onInput != nil {
		m.onInput(p)
	}
}

// unwrapPasteIfUnsupported strips the ESC[200~/ESC[201~ brackets from a
// client's paste when the guest has not enabled DECSET ?2004.
//
// A client cannot make this call itself. It attaches to a screen snapshot —
// renderScreen emits cells, cursor and SGR, never modes — so a client-side
// emulator cannot know that an agent TUI which started before the attach has
// bracketed paste on. The emulator here does know: it is fed every byte the
// guest ever wrote, and x/vt's own Emulator.Paste gates on exactly this mode.
//
// Sending the brackets to a guest that never asked for them is not a cosmetic
// problem — they arrive as literal input. A plain shell reads
// "^[[200~ls -la^[[201~" as the command line, so the paste is corrupted rather
// than merely unstyled, which is worse than the drop this PR is fixing.
func (m *Mux) unwrapPasteIfUnsupported(p []byte) []byte {
	if !bytes.HasPrefix(p, bracketedPasteStart) || !bytes.HasSuffix(p, bracketedPasteEnd) {
		return p
	}
	m.mu.Lock()
	on := m.bracketedPaste
	m.mu.Unlock()
	if on {
		return p
	}
	return p[len(bracketedPasteStart) : len(p)-len(bracketedPasteEnd)]
}

// SessionCount reports the number of live sessions (for tests).
func (m *Mux) SessionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}

// Size returns the current authoritative (smallest-attached-wins) viewport. The
// supervisor reads it on every spawn to born-size the harness's PTY, so it must
// stay answerable while the data plane is busy — see sizeMu.
func (m *Mux) Size() (cols, rows int) {
	m.sizeMu.Lock()
	defer m.sizeMu.Unlock()
	return m.cols, m.rows
}

// setSize publishes a new authoritative viewport.
func (m *Mux) setSize(cols, rows int) {
	m.sizeMu.Lock()
	m.cols, m.rows = cols, rows
	m.sizeMu.Unlock()
}

// renderSnapshotLocked renders the current screen for a coalesced client.
// Caller holds mu.
func (m *Mux) renderSnapshotLocked() []byte { return m.snapshotLocked() }

// snapshotLocked is the attach snapshot: the screen repaint plus the guest's
// mouse-reporting modes. Cells, cursor and SGR survive a repaint; a DECSET does
// not, so a client that attaches after the guest enabled the mouse (a
// long-running Claude Code turns it on once, at startup) would otherwise be
// deaf to it forever. The modes go last so the client paints the screen before
// its own input routing learns who owns the wheel.
//
// Governing: SPEC-0002 REQ "Attach Session", ADR-0055. Deliberately absent from
// Mux.AnsiScreen, whose bytes are written to a real terminal by capture.
func (m *Mux) snapshotLocked() []byte {
	return append(renderScreen(m.term), m.mouse.Serialize()...)
}

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
	if curC, curR := m.Size(); minC == curC && minR == curR {
		return
	}
	m.setSize(minC, minR)
	m.term.Resize(minC, minR)
	if m.onResize != nil {
		m.onResize(minC, minR)
	}
}

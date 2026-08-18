package tui

// The dashboard's live preview, as a real attach session.
//
// Governing: SPEC-0001 REQ "Dashboard" (live read-only tail of the selected
// harness), SPEC-0002 REQ "Attach Session", ADR-0003 (one emulator per
// harness; smallest-attached-wins resize), ADR-0008 (read-only attach).
// Issue #200.
//
// The preview used to poll the `logs` control op for a 200-line tail and
// replay it through a client-side emulator. That is a viewer the daemon never
// hears about, and the daemon derives a harness's PTY size purely from its
// attach sessions — so a harness nobody had pressed ↵ on stayed at the 80×24
// it was born at for its entire life, and every full-screen agent inside one
// laid out for a terminal a quarter the size of the pane displaying it. The
// preview was faithfully rendering a screen that was itself wrong.
//
// So the preview stops being a special case and becomes what it always
// described itself as: a read-only attach, sized to the pane. Everything that
// makes attached mode work — the viewport negotiation, the PTY resize, the
// SIGWINCH re-assertion over the guest's boot window, the snapshot-then-live
// stream — applies unchanged, because it is the same code path. Nothing new
// was added to the protocol and there is no second sizing policy to keep in
// sync with ADR-0003.

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// peekSettleDelay is how long the preview's target must hold still before a
// session is opened for it.
//
// Holding j down walks the selection through every harness in the list, and
// each stop would otherwise open a session, resize the guest's PTY, deliver a
// SIGWINCH, and tear it all down again a few milliseconds later. The poll-based
// tail still repaints immediately on every selection change, so this delay
// costs nothing visible — it only decides when the *sizing* follows.
const peekSettleDelay = 250 * time.Millisecond

// peekSyncMsg asks for the peek session to be reconciled with what the
// dashboard is showing. gen is the generation it was scheduled at: a newer
// target supersedes it, which is what makes the delay a debounce rather than
// a queue.
type peekSyncMsg struct{ gen uint64 }

// peekTargetChanged is what a selection change calls instead of peekCmd. It
// keeps the immediate tail repaint — the preview must not go blank while the
// user scrolls — and schedules the session reconcile behind the settle delay.
func (m *Model) peekTargetChanged() tea.Cmd {
	m.peekGen++
	gen := m.peekGen
	return tea.Batch(
		m.peekCmd(),
		tea.Tick(peekSettleDelay, func(time.Time) tea.Msg { return peekSyncMsg{gen: gen} }),
	)
}

// wantPeekSession reports the harness the preview should hold a session on and
// the geometry to report for it, or ok=false when it should hold none.
//
// Attached mode is deliberately excluded. The attach reports the whole window
// and the preview would report the pane; smallest-attached-wins would hand the
// guest to the smaller of the two, so the dashboard's own preview would clamp
// the user's full-window attach to a third of it. Only one of the two is ever
// on screen, so only one ever holds a session.
func (m *Model) wantPeekSession() (name string, cols, rows int, ok bool) {
	if m.attach == nil || m.conn != startOK || m.mode != modeDashboard || m.quitting {
		return "", 0, 0, false
	}
	sel, has := m.selectedHarness()
	if !has {
		return "", 0, 0, false
	}
	cols, rows = m.peekViewport()
	if cols < 1 || rows < 1 {
		// Size not known yet (#183): a client that cannot measure itself must
		// not define geometry for everyone else attached to this harness.
		return "", 0, 0, false
	}
	return sel.Name, cols, rows, true
}

// syncPeekSession reconciles the open session against wantPeekSession: no-op
// when it already matches, a resize when only the geometry moved, a
// close-then-open when the selection did.
func (m *Model) syncPeekSession() tea.Cmd {
	name, cols, rows, want := m.wantPeekSession()
	if !want {
		return m.closePeekSession()
	}
	conn := m.attach

	if m.peekSess != 0 && m.peekSessName == name {
		if cols == m.peekCols && rows == m.peekRows {
			return nil
		}
		m.peekCols, m.peekRows = cols, rows
		m.peekView.resize(cols, rows)
		sid := m.peekSess
		return func() tea.Msg { _ = conn.AttachResize(sid, cols, rows); return nil }
	}

	closePrev := m.closePeekSession()
	sid := m.nextSessionID()
	m.peekSess, m.peekSessName = sid, name
	m.peekCols, m.peekRows = cols, rows
	// Re-use the emulator across selections rather than building one per
	// session: a fresh vtView starts a reply pump that can never be stopped
	// without racing the emulator's close flag (see vtView.pumpReplies).
	if m.peekView == nil {
		m.peekView = newVTView(cols, rows)
	} else {
		m.peekView.reset(cols, rows)
	}
	// Close and open in ONE command rather than two batched ones: tea.Batch
	// runs its commands concurrently, so the open could reach the daemon first
	// and the mux would briefly hold both sessions — resizing the guest twice
	// for one selection change. Both calls are synchronous frame writes, so
	// doing them in order here puts them on the wire in order.
	return func() tea.Msg {
		if closePrev != nil {
			closePrev()
		}
		_ = conn.AttachOpen(sid, name, cols, rows, protocol.AttachRO)
		return nil
	}
}

// closePeekSession drops the session if one is open. Callers that are about to
// open something else on the same harness must sequence this before it.
func (m *Model) closePeekSession() tea.Cmd {
	sid := m.peekSess
	m.peekSess, m.peekSessName = 0, ""
	m.peekCols, m.peekRows = 0, 0
	if sid == 0 || m.attach == nil {
		return nil
	}
	conn := m.attach
	return func() tea.Msg { _ = conn.AttachClose(sid); return nil }
}

// peekLive reports whether the preview is being driven by a live session for
// the harness currently selected — i.e. whether viewPeek should render the
// streamed screen rather than the polled tail.
func (m *Model) peekLive() bool {
	if m.peekSess == 0 || m.peekView == nil {
		return false
	}
	sel, ok := m.selectedHarness()
	return ok && sel.Name == m.peekSessName
}

// nextSessionID allocates the next attach session id.
//
// One counter serves both the preview and attached mode. Ids must be unique
// across BOTH, because the read loop routes ATTACH_DATA by id alone and the
// two are open on the same connection; monotonic ids additionally mean a frame
// still in flight from a just-closed session is dropped rather than written
// into whatever took its place (the reason the hop already incremented).
func (m *Model) nextSessionID() uint32 {
	if m.sessionSeq < sessionBase {
		m.sessionSeq = sessionBase
	} else {
		m.sessionSeq++
	}
	return m.sessionSeq
}

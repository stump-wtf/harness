package tui

// Coverage for issue #200: the dashboard's preview holds a real read-only
// attach session sized to the pane, so the daemon knows how big the viewer is
// and the guest's PTY follows it.
//
// The failure this guards against is not visual. A preview that reports no
// viewport still renders — it just renders a guest stuck at the 80×24 it was
// born at — so nearly every assertion here is about what reaches the daemon.

import (
	"testing"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// peekModel is a connected dashboard on a wide window with a selection.
func peekModel() (*Model, *fakeAttach) {
	fc := &fakeController{harnesses: sampleHarnesses()}
	fa := &fakeAttach{}
	m := New(Options{})
	m.ctrl, m.attach = fc, fa
	m.conn = startOK
	m.harnesses = fc.harnesses
	m.profiles = nil
	m.showAll = true
	m.w, m.h = 175, 48
	m.sel = 0
	return m, fa
}

// TestPeekOpensReadOnlySessionAtPaneSize is the core of #200. Before this the
// dashboard told the daemon nothing, so a harness nobody had pressed ↵ on
// stayed at its born 80×24 for its entire life.
func TestPeekOpensReadOnlySessionAtPaneSize(t *testing.T) {
	m, fa := peekModel()

	drain(m.syncPeekSession())

	if len(fa.opens) != 1 {
		t.Fatalf("preview opened %d sessions, want 1: %v", len(fa.opens), fa.opens)
	}
	sel, _ := m.selectedHarness()
	if fa.opens[0] != sel.Name {
		t.Errorf("opened on %q, want the selected %q", fa.opens[0], sel.Name)
	}
	if fa.openModes[0] != protocol.AttachRO {
		t.Errorf("mode = %v, want read-only — the preview must never accept input (ADR-0008)",
			fa.openModes[0])
	}
	wantCols, wantRows := m.peekViewport()
	if got := fa.openSizes[0]; got.cols != wantCols || got.rows != wantRows {
		t.Errorf("reported %dx%d, want the pane's %dx%d", got.cols, got.rows, wantCols, wantRows)
	}
	// Guard the fixture itself: if the pane were ≤ 80 columns this test would
	// pass against the very bug it exists to catch.
	if wantCols <= 80 {
		t.Fatalf("fixture too narrow to prove anything: pane is %d cols", wantCols)
	}
}

// TestPeekReportsNothingWhenBlind pins the #183 discipline: a client that
// cannot measure itself must not define geometry for everyone else attached to
// that harness. A fallback here would clamp other clients' guests.
func TestPeekReportsNothingWhenBlind(t *testing.T) {
	m, fa := peekModel()
	m.w, m.h = 0, 0

	drain(m.syncPeekSession())

	if len(fa.opens) != 0 {
		t.Errorf("opened a session with no known window size: %v", fa.opens)
	}
}

// TestPeekResizesRatherThanReopening: a pane that changed size must move the
// existing session, not churn a new one — reopening re-sends the whole screen
// snapshot and makes the preview flicker.
func TestPeekResizesRatherThanReopening(t *testing.T) {
	m, fa := peekModel()
	drain(m.syncPeekSession())
	opens := len(fa.opens)

	// Unchanged geometry: nothing at all should go out.
	drain(m.syncPeekSession())
	if len(fa.opens) != opens || len(fa.resizes) != 0 {
		t.Errorf("an unchanged pane produced traffic: opens=%v resizes=%v", fa.opens, fa.resizes)
	}

	m.h = 30
	drain(m.syncPeekSession())
	if len(fa.opens) != opens {
		t.Errorf("a resize reopened the session: %v", fa.opens)
	}
	if len(fa.resizes) != 1 {
		t.Fatalf("resizes = %v, want exactly 1", fa.resizes)
	}
	wantCols, wantRows := m.peekViewport()
	if fa.resizes[0].cols != wantCols || fa.resizes[0].rows != wantRows {
		t.Errorf("resized to %dx%d, want the pane's %dx%d",
			fa.resizes[0].cols, fa.resizes[0].rows, wantCols, wantRows)
	}
}

// TestPeekSwitchesHarnessOnSelectionChange: the old session must be released,
// or walking the list leaves a trail of sessions clamping every harness it
// touched.
func TestPeekSwitchesHarnessOnSelectionChange(t *testing.T) {
	m, fa := peekModel()
	drain(m.syncPeekSession())
	firstName, firstID := fa.opens[0], fa.openSizes[0].sid

	m.moveSel(1)
	drain(m.syncPeekSession())

	if len(fa.closes) != 1 || fa.closes[0] != firstID {
		t.Errorf("closes = %v, want the previous session %d released", fa.closes, firstID)
	}
	if len(fa.opens) != 2 {
		t.Fatalf("opens = %v, want a second session for the new selection", fa.opens)
	}
	if fa.opens[1] == firstName {
		t.Errorf("reopened on %q — the selection moved", fa.opens[1])
	}
	if fa.openSizes[1].sid == firstID {
		t.Errorf("reused session id %d; a frame still in flight from the old session would be misrouted", firstID)
	}
}

// TestAttachClosesPeekBeforeOpening is the trap #200 calls out. The preview is
// open on this same harness at a THIRD of the window, and
// smallest-attached-wins (ADR-0003) hands the guest to whichever is smaller —
// so if the preview outlives the attach by even a moment, pressing ↵ gives a
// full-window terminal painting at the preview's geometry.
func TestAttachClosesPeekBeforeOpening(t *testing.T) {
	m, fa := peekModel()
	drain(m.syncPeekSession())
	peekID := fa.openSizes[0].sid
	fa.order = nil // from here on, only the attach's traffic

	sel, _ := m.selectedHarness()
	drain(m.attachTo(sel, 0))

	if len(fa.closes) == 0 || fa.closes[0] != peekID {
		t.Fatalf("closes = %v, want the preview session %d closed", fa.closes, peekID)
	}
	if m.peekSess != 0 {
		t.Errorf("peekSess = %d, want the preview released while attached", m.peekSess)
	}
	// Ordering is the actual invariant, and it holds only because the close
	// runs inside the open's command rather than batched beside it.
	if len(fa.order) < 2 || fa.order[0] != "close" || fa.order[len(fa.order)-1] != "open" {
		t.Errorf("wire order = %v, want every close before the open", fa.order)
	}
	// And the attach reports the WHOLE window, not the pane.
	wantCols, wantRows := m.attachReportSize()
	last := fa.openSizes[len(fa.openSizes)-1]
	if last.cols != wantCols || last.rows != wantRows {
		t.Errorf("attach reported %dx%d, want the full window's %dx%d",
			last.cols, last.rows, wantCols, wantRows)
	}
}

// TestPeekHoldsNoSessionWhileAttached: only one of the two surfaces is on
// screen at a time, so only one may ever hold a session.
func TestPeekHoldsNoSessionWhileAttached(t *testing.T) {
	m, fa := peekModel()
	m.mode = modeAttached
	m.att = newAttachState("crush-signal", protocol.AttachRW, 1, 175, 47)

	drain(m.syncPeekSession())

	if len(fa.opens) != 0 {
		t.Errorf("preview opened a session in attached mode: %v", fa.opens)
	}
}

// TestPeekDebouncesSelectionWalk: holding j walks the whole list, and a session
// per stop would resize the guest's PTY and SIGWINCH a live agent on every one.
// Only the generation still current when its timer fires may reconcile.
func TestPeekDebouncesSelectionWalk(t *testing.T) {
	m, fa := peekModel()

	// Four rapid steps. The commands are deliberately not drained — the timers
	// are what we are simulating, and only their generations matter.
	staleGen := m.peekGen + 1
	for range 4 {
		m.peekTargetChanged()
		m.moveSel(1)
	}

	// The first step's timer fires late; it is superseded and must do nothing.
	if _, cmd := m.Update(peekSyncMsg{gen: staleGen}); cmd != nil {
		drain(cmd)
	}
	if len(fa.opens) != 0 {
		t.Errorf("a superseded generation opened a session: %v", fa.opens)
	}

	// The current one reconciles, once.
	if _, cmd := m.Update(peekSyncMsg{gen: m.peekGen}); cmd != nil {
		drain(cmd)
	}
	if len(fa.opens) != 1 {
		t.Fatalf("opens = %v, want exactly one session for the settled selection", fa.opens)
	}
	sel, _ := m.selectedHarness()
	if fa.opens[0] != sel.Name {
		t.Errorf("opened on %q, want the harness the walk landed on, %q", fa.opens[0], sel.Name)
	}
}

// TestPeekStreamRendersIntoThePane closes the loop: bytes arriving on the
// preview's session reach its emulator and show up in the pane, and bytes from
// any other session do not.
func TestPeekStreamRendersIntoThePane(t *testing.T) {
	m, _ := peekModel()
	drain(m.syncPeekSession())

	m.Update(attachDataMsg{sessionID: m.peekSess, data: []byte("hello from the guest")})

	if !m.peekLive() {
		t.Fatal("peek should be live once a session is open for the selection")
	}
	got := m.viewPeek(100, m.bodyHeight())
	if !containsStr(got, "hello from the guest") {
		t.Errorf("streamed bytes did not reach the pane:\n%s", got)
	}

	m.Update(attachDataMsg{sessionID: m.peekSess + 99, data: []byte("STALE")})
	if got := m.viewPeek(100, m.bodyHeight()); containsStr(got, "STALE") {
		t.Error("a frame from another session was written into the preview")
	}
}

// TestPeekLiveKeepsTheLogPoll: the stream and the tail are not the same data.
// peekView holds the visible SCREEN, which is all the pane needs; the `logs`
// tail is the 200 lines of HISTORY that peekLines() hands to attached
// scrollback. Suppressing the poll as "duplicate traffic" froze m.peek.text at
// the moment the session went live, so attaching after watching a harness for
// two minutes and scrolling back showed the buffer as it was two minutes ago.
func TestPeekLiveKeepsTheLogPoll(t *testing.T) {
	m, _ := peekModel()
	fc := m.ctrl.(*fakeController)

	drain(m.peekCmd())
	before := fc.logCalls
	if before == 0 {
		t.Fatal("peekCmd polled nothing")
	}

	drain(m.syncPeekSession())
	if !m.peekLive() {
		t.Fatal("preview did not go live")
	}
	_, cmd := m.onTick()
	drain(cmd)

	if fc.logCalls <= before {
		t.Error("the scrollback seed stopped refreshing once the preview went live")
	}
}

// TestPeekTickDefersToAnOutstandingDebounce: reconciling on the tick regardless
// of the debounce is still reconciling on change, just on a one-second grid —
// holding j would open a session on whatever row the tick caught, resize that
// guest's PTY and SIGWINCH it, then tear it down on the next tick. That is the
// churn peekSettleDelay exists to prevent.
func TestPeekTickDefersToAnOutstandingDebounce(t *testing.T) {
	m, _ := peekModel()
	drain(m.syncPeekSession())
	m.peekSyncedGen = m.peekGen
	settled := m.peekSess
	if settled == 0 {
		t.Fatal("expected a settled session to start from")
	}

	// A selection change with its debounce still in flight.
	m.sel = 1
	drain(m.peekTargetChanged())
	if m.peekSyncedGen == m.peekGen {
		t.Fatal("peekTargetChanged did not leave a sync outstanding")
	}

	_, cmd := m.onTick()
	drain(cmd)
	if m.peekSess != settled {
		t.Errorf("the tick re-opened the session behind the debounce: %d -> %d", settled, m.peekSess)
	}

	// Once the debounce lands, the reconcile happens.
	mi, cmd := m.Update(peekSyncMsg{gen: m.peekGen})
	drain(cmd)
	if mi.(*Model).peekSess == settled {
		t.Error("the settled sync did not move the session to the new selection")
	}
}

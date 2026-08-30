package tui

// Coverage for issue #290: the preview flashed the log tail for one settle
// delay and then went blank, permanently, on every headless harness.
//
// #200 made the preview a real read-only attach session and had viewPeek
// switch to that session's screen the moment it opened. For a guest that draws
// something that is right. For a guest that draws NOTHING — `crush --channels
// server:signal`, `claude -p`, any agent whose work happens over a socket —
// the session's emulator is blank at open and stays blank for the process's
// entire life, so the switch replaced the one thing the pane had (the polled
// `logs` tail: the harness's own lifecycle lines and whatever the guest wrote
// before the session existed) with an empty grid.
//
// The rule these tests pin: an open session is not enough to take the pane.
// It has to have painted.

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// peekPaintedModel is a connected dashboard whose selected harness has a
// non-empty polled tail — the state the pane is in the instant before the
// debounced session lands.
func peekPaintedModel(t *testing.T) (*Model, *fakeAttach) {
	t.Helper()
	m, fa := peekModel()
	sel, ok := m.selectedHarness()
	if !ok {
		t.Fatal("fixture has no selection")
	}
	m.peek = logsMsg{name: sel.Name, text: "INFO state changed from=starting to=running"}
	return m, fa
}

// feedPeek delivers one ATTACH_DATA frame on the open preview session.
func feedPeek(m *Model, data string) {
	m.Update(attachDataMsg{sessionID: m.peekSess, data: []byte(data)})
}

// TestPeekKeepsTailUntilTheGuestPaints is the regression itself: a session is
// open, the guest has sent nothing, and the pane must still show the tail.
func TestPeekKeepsTailUntilTheGuestPaints(t *testing.T) {
	m, _ := peekPaintedModel(t)

	drain(m.syncPeekSession())
	if m.peekSess == 0 {
		t.Fatal("no preview session opened — fixture is not exercising the bug")
	}
	if m.peekLive() {
		t.Error("peekLive with a session that has never painted: the empty grid would erase the tail")
	}

	view := m.viewPeek(100, 30)
	if !strings.Contains(view, "state changed from=starting to=running") {
		t.Errorf("preview dropped the tail once the session opened:\n%s", view)
	}
}

// TestPeekSwitchesToLiveOnceTheGuestPaints keeps #200 intact: the moment the
// guest draws, its screen is the pane's truth and the tail steps aside.
func TestPeekSwitchesToLiveOnceTheGuestPaints(t *testing.T) {
	m, _ := peekPaintedModel(t)
	drain(m.syncPeekSession())

	feedPeek(m, "\x1b[?1049h\x1b[H\x1b[2JLIVE GUEST SCREEN")

	if !m.peekLive() {
		t.Fatal("guest painted but the preview is still on the polled tail")
	}
	view := m.viewPeek(100, 30)
	if !strings.Contains(view, "LIVE GUEST SCREEN") {
		t.Errorf("preview is not showing the live screen:\n%s", view)
	}
	if strings.Contains(view, "state changed from=starting to=running") {
		t.Errorf("live screen and polled tail both rendered:\n%s", view)
	}
}

// TestPeekIgnoresWhitespaceOnlyPaint: a guest that only moves the cursor or
// clears its screen has not painted. Treating "bytes arrived" as "has content"
// would reintroduce the blank pane for every guest that emits a lone escape
// sequence on attach.
func TestPeekIgnoresWhitespaceOnlyPaint(t *testing.T) {
	m, _ := peekPaintedModel(t)
	drain(m.syncPeekSession())

	feedPeek(m, "\x1b[H\x1b[2J   \r\n   \r\n")

	if m.peekLive() {
		t.Error("a screen of spaces counted as painted")
	}
	if view := m.viewPeek(100, 30); !strings.Contains(view, "state changed") {
		t.Errorf("preview blanked on a whitespace-only paint:\n%s", view)
	}
}

// TestPeekPaintLatchResetsOnSelectionChange: the latch belongs to the session,
// not to the model. Carrying it across a hop would show the previous harness's
// screen for the new one — and then never fall back when the new one is quiet.
func TestPeekPaintLatchResetsOnSelectionChange(t *testing.T) {
	m, _ := peekPaintedModel(t)
	drain(m.syncPeekSession())
	feedPeek(m, "FIRST GUEST")
	if !m.peekLive() {
		t.Fatal("first guest never went live — fixture is wrong")
	}

	m.sel = 1
	sel, _ := m.selectedHarness()
	m.peek = logsMsg{name: sel.Name, text: "second harness tail"}
	drain(m.syncPeekSession())

	if m.peekPainted {
		t.Error("paint latch survived the session change")
	}
	view := m.viewPeek(100, 30)
	if strings.Contains(view, "FIRST GUEST") {
		t.Errorf("previous guest's screen leaked into the new selection:\n%s", view)
	}
	if !strings.Contains(view, "second harness tail") {
		t.Errorf("preview is not showing the new selection's tail:\n%s", view)
	}
}

// TestPeekPaintLatchResetsOnClose pins the other half: a closed session leaves
// nothing behind that could claim the pane.
func TestPeekPaintLatchResetsOnClose(t *testing.T) {
	m, _ := peekPaintedModel(t)
	drain(m.syncPeekSession())
	feedPeek(m, "PAINTED")

	drain(m.closePeekSession())

	if m.peekPainted {
		t.Error("paint latch survived closePeekSession")
	}
	if m.peekLive() {
		t.Error("peekLive with no open session")
	}
}

// TestPeekDataFromAnotherSessionDoesNotLatch: ATTACH_DATA is routed by id, and
// a frame still in flight from a closed session must not mark the current one
// painted (nextSessionID's whole reason for being monotonic).
func TestPeekDataFromAnotherSessionDoesNotLatch(t *testing.T) {
	m, _ := peekPaintedModel(t)
	drain(m.syncPeekSession())

	m.Update(attachDataMsg{sessionID: m.peekSess + 99, data: []byte("STALE FRAME")})

	if m.peekPainted {
		t.Error("a frame from another session latched the preview")
	}
}

// TestVTViewBlank is the primitive underneath: content, not bytes, is what
// counts as painted.
func TestVTViewBlank(t *testing.T) {
	v := newVTView(20, 5)
	if !v.blank() {
		t.Error("a fresh emulator is not blank")
	}
	v.write([]byte("\x1b[H\x1b[2J"))
	if !v.blank() {
		t.Error("clear-screen counted as content")
	}
	v.write([]byte("   \r\n  \r\n"))
	if !v.blank() {
		t.Error("spaces counted as content")
	}
	v.write([]byte("x"))
	if v.blank() {
		t.Error("a printed character did not count as content")
	}
	v.reset(20, 5)
	if !v.blank() {
		t.Error("reset did not return the emulator to blank")
	}
}

// compile-time guard: attachDataMsg is what the read loop delivers, so these
// tests exercise the real path rather than a test-only shortcut.
var _ tea.Msg = attachDataMsg{}
var _ protocol.AttachMode = protocol.AttachRO

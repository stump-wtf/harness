package tui

// Guest mouse ownership, client side (issue #954, upstream
// stump-wtf/harness#29). A fullscreen guest (alternate screen, DECSET 1003 +
// 1006) scrolls its own transcript from the wheel and PgUp; on main the client
// took both for its own scrollback, so such a guest could not be scrolled
// through `harness attach` at all. These pin the tmux/screen rule: the guest
// owns the mouse while it has mouse reporting on, and nothing else changes for
// a guest that never asked.
//
// The modes are fed through the view's emulator rather than set on a field,
// because that is the only way this end learns them: the daemon's snapshot ends
// with its shadowed DECSETs, and everything after is whatever the guest writes.

import (
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/stump-wtf/harness/internal/protocol"
)

// guestWith returns an attached read-write Model whose guest has just written
// the given control bytes, plus the wire they would be forwarded to.
func guestWith(t *testing.T, control string) (*Model, *wireRecorder) {
	t.Helper()
	m, w := attachedRecorder()
	m.att.view.write([]byte(control))
	return m, w
}

// drive sends one message through Update and runs the returned Cmd the way the
// event loop would, so the write lands on the recorded wire.
func drive(m *Model, msg tea.Msg) {
	_, cmd := m.Update(msg)
	if cmd != nil {
		cmd()
	}
}

func TestWheelForwardsToGuestThatOwnsTheMouse(t *testing.T) {
	m, w := guestWith(t, "\x1b[?1000h\x1b[?1006h")

	drive(m, tea.MouseWheelMsg{X: 10, Y: 5, Button: tea.MouseWheelUp})

	if got, want := w.String(), "\x1b[<64;11;6M"; got != want {
		t.Errorf("wire = %q, want SGR wheel-up at the guest's 11,6", got)
	}
	if m.att.substate != substateInteractive {
		t.Errorf("substate = %v, want interactive: the wheel belonged to the guest, so harness scrollback must not open", m.att.substate)
	}
}

func TestWheelStillOpensScrollbackForGuestThatNeverAsked(t *testing.T) {
	m, w := guestWith(t, "echo hi\r\n")

	drive(m, tea.MouseWheelMsg{X: 10, Y: 5, Button: tea.MouseWheelUp})

	if w.String() != "" {
		t.Errorf("wire = %q, want nothing: a guest without mouse reporting gets no mouse bytes", w.String())
	}
	if m.att.substate != substateScrollback {
		t.Error("harness scrollback must still answer the wheel for a guest that never asked for the mouse")
	}
}

func TestLegacyEncodingWhenGuestDidNotAskForSGR(t *testing.T) {
	m, w := guestWith(t, "\x1b[?1000h")

	drive(m, tea.MouseWheelMsg{X: 10, Y: 5, Button: tea.MouseWheelUp})

	want := []byte{0x1b, '[', 'M', 32 + 64, 32 + 11, 32 + 6}
	if got := w.String(); got != string(want) {
		t.Errorf("wire = %q, want the three-byte legacy form", got)
	}
}

func TestMode9GuestGetsPressesButNoReleases(t *testing.T) {
	m, w := guestWith(t, "\x1b[?9h")

	drive(m, tea.MouseClickMsg{X: 3, Y: 2, Button: tea.MouseLeft})
	drive(m, tea.MouseReleaseMsg{X: 3, Y: 2, Button: tea.MouseLeft})

	want := string([]byte{0x1b, '[', 'M', 32, 32 + 4, 32 + 3})
	if got := w.String(); got != want {
		t.Errorf("wire = %q, want the press alone (%q): X10 reports no release, so sending one is input the guest never asked for", got, want)
	}
}

func TestReleaseUsesSGRReleaseTerminator(t *testing.T) {
	m, w := guestWith(t, "\x1b[?1000h\x1b[?1006h")

	drive(m, tea.MouseReleaseMsg{X: 3, Y: 2, Button: tea.MouseLeft})

	if got, want := w.String(), "\x1b[<0;4;3m"; got != want {
		t.Errorf("wire = %q, want %q", got, want)
	}
}

func TestHoverOnlyReachesAnAnyEventGuest(t *testing.T) {
	m, w := guestWith(t, "\x1b[?1000h\x1b[?1006h")
	drive(m, tea.MouseMotionMsg{X: 1, Y: 1, Button: tea.MouseNone})
	if w.String() != "" {
		t.Errorf("wire = %q, want nothing: 1000 asks for no hover events", w.String())
	}

	m2, w2 := guestWith(t, "\x1b[?1003h\x1b[?1006h")
	drive(m2, tea.MouseMotionMsg{X: 1, Y: 1, Button: tea.MouseNone})
	if got, want := w2.String(), "\x1b[<35;2;2M"; got != want {
		t.Errorf("wire = %q, want %q", got, want)
	}
}

func TestDragNeedsButtonEventTracking(t *testing.T) {
	m, w := guestWith(t, "\x1b[?1000h\x1b[?1006h")
	drive(m, tea.MouseMotionMsg{X: 4, Y: 4, Button: tea.MouseLeft})
	if w.String() != "" {
		t.Errorf("wire = %q, want nothing: 1000 alone reports no drags", w.String())
	}

	m2, w2 := guestWith(t, "\x1b[?1002h\x1b[?1006h")
	drive(m2, tea.MouseMotionMsg{X: 4, Y: 4, Button: tea.MouseLeft})
	if got, want := w2.String(), "\x1b[<32;5;5M"; got != want {
		t.Errorf("wire = %q, want %q", got, want)
	}
}

func TestModifierBitsTravelWithTheEvent(t *testing.T) {
	m, w := guestWith(t, "\x1b[?1002h\x1b[?1006h")

	drive(m, tea.MouseMotionMsg{X: 4, Y: 4, Button: tea.MouseLeft, Mod: tea.ModCtrl})

	if got, want := w.String(), "\x1b[<48;5;5M"; got != want {
		t.Errorf("wire = %q, want %q (drag 32 + ctrl 16)", got, want)
	}
}

func TestShiftMotionBelongsToTheOperatorNotTheGuest(t *testing.T) {
	m, w := guestWith(t, "\x1b[?1002h\x1b[?1006h")

	drive(m, tea.MouseMotionMsg{X: 4, Y: 4, Button: tea.MouseLeft, Mod: tea.ModShift})

	if w.String() != "" {
		t.Errorf("wire = %q, want nothing: shift means the operator is selecting text, even while the guest owns the mouse", w.String())
	}
	if !m.mouseReleased {
		t.Error("shift-motion must release the grab (#49) rather than forward a drag")
	}
}

func TestPgUpReachesGuestThatOwnsTheMouse(t *testing.T) {
	m, w := guestWith(t, "\x1b[?1003h\x1b[?1006h")

	drive(m, tea.KeyPressMsg{Code: tea.KeyPgUp})

	if got, want := w.String(), "\x1b[5~"; got != want {
		t.Errorf("wire = %q, want %q: a fullscreen guest pages its own transcript with PgUp", got, want)
	}
	if m.att.substate != substateInteractive {
		t.Error("PgUp belonged to the guest, so harness scrollback must not open")
	}
}

func TestPgUpStillOpensScrollbackWithoutMouseReporting(t *testing.T) {
	m, w := guestWith(t, "echo hi\r\n")

	drive(m, tea.KeyPressMsg{Code: tea.KeyPgUp})

	if w.String() != "" {
		t.Errorf("wire = %q, want nothing", w.String())
	}
	if m.att.substate != substateScrollback {
		t.Error("PgUp must keep opening harness scrollback for a guest that never asked for the mouse")
	}
}

func TestPrefixScrollbackStillOpensWhileGuestOwnsMouse(t *testing.T) {
	m, w := guestWith(t, "\x1b[?1003h\x1b[?1006h")

	drive(m, tea.KeyPressMsg{Code: 'b', Mod: tea.ModCtrl})
	drive(m, runeKey("["))

	if m.att.substate != substateScrollback {
		t.Error("^b [ is how you read harness's own history while a guest holds the mouse; it must still work")
	}
	if w.String() != "" {
		t.Errorf("wire = %q, want nothing: the chord is harness's, not the guest's", w.String())
	}
}

func TestReadOnlyAttachKeepsTheWheel(t *testing.T) {
	m, _ := attachedFake(80, 24)
	m.att = newAttachState(m.harnesses[0].Name, protocol.AttachRO, sessionBase, 80, 23)
	m.att.view.write([]byte("\x1b[?1000h\x1b[?1006h"))

	drive(m, tea.MouseWheelMsg{X: 10, Y: 5, Button: tea.MouseWheelUp})

	if m.att.substate != substateScrollback {
		t.Error("a read-only viewer has no input to forward, so the wheel is its only way to scroll")
	}
}

func TestScrollbackSubstateKeepsNavigating(t *testing.T) {
	m, w := guestWith(t, "\x1b[?1000h\x1b[?1006h")
	m.att.enterScrollback(m.peekLines(), m.scrollbackHeight())
	wireBefore := w.String()

	drive(m, tea.MouseWheelMsg{X: 10, Y: 5, Button: tea.MouseWheelUp})

	if w.String() != wireBefore {
		t.Error("the frozen scrollback owns the wheel, not the guest")
	}
	if m.att.substate != substateScrollback {
		t.Error("wheel inside scrollback must navigate it, not leave it")
	}
}

func TestShiftClickStillReleasesTheGrab(t *testing.T) {
	m, w := guestWith(t, "\x1b[?1000h\x1b[?1006h")

	drive(m, tea.MouseClickMsg{X: 3, Y: 2, Button: tea.MouseLeft, Mod: tea.ModShift})

	if w.String() != "" {
		t.Errorf("wire = %q, want nothing: shift-click is the operator selecting text, not the guest's click", w.String())
	}
	if !m.mouseReleased {
		t.Error("shift-click must still release the mouse grab (#49)")
	}
}

func TestEventOnTheStatusBarIsNotForwarded(t *testing.T) {
	m, w := guestWith(t, "\x1b[?1000h\x1b[?1006h")
	rows := m.att.view.rows

	drive(m, tea.MouseWheelMsg{X: 5, Y: rows, Button: tea.MouseWheelUp})

	if w.String() != "" {
		t.Errorf("wire = %q, want nothing: the bottom row is harness's status bar, not the guest's screen", w.String())
	}
	if m.att.substate != substateInteractive {
		t.Error("an event on the status bar must not open scrollback either")
	}
}

func TestAllMotionAskedOfTheTerminalOnlyForAnyEventGuest(t *testing.T) {
	m, _ := guestWith(t, "\x1b[?1000h")
	if got := m.View().MouseMode; got != tea.MouseModeCellMotion {
		t.Errorf("MouseMode = %v, want cell motion: asking for every motion the guest did not want is noise on the wire", got)
	}

	m2, _ := guestWith(t, "\x1b[?1003h")
	if got := m2.View().MouseMode; got != tea.MouseModeAllMotion {
		t.Errorf("MouseMode = %v, want all motion: a 1003 guest cannot receive hover otherwise", got)
	}
}

func TestHopClearsThePreviousGuestsMouse(t *testing.T) {
	m, _ := guestWith(t, "\x1b[?1003h")
	if !m.guestOwnsMouse() {
		t.Fatal("setup: the guest should own the mouse")
	}

	m.att.view.reset(80, 23)

	if m.guestOwnsMouse() {
		t.Error("a hop must not inherit the previous guest's wheel ownership")
	}
	if got := m.View().MouseMode; got != tea.MouseModeCellMotion {
		t.Errorf("MouseMode = %v after the hop, want cell motion", got)
	}
}

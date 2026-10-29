package tui

// Guest mouse ownership while attached.
//
// Governing: ADR-0055 (the guest owns the mouse while it has mouse reporting
// on), SPEC-0001 REQ "Attached Mode", and the tmux/screen behaviour the reporter
// validated against a real fullscreen Claude Code (issue #954, upstream
// stump-wtf/harness#29): a guest that asked for the mouse gets the wheel, the
// clicks and PgUp; a guest that never asked keeps harness scrollback.
//
// The mode shadow lives on the view's emulator (vtView.mouse), fed by the same
// callbacks that shadow DECTCEM and by the daemon's snapshot for anything that
// happened before the attach. Everything below turns that shadow into bytes on
// the wire, and just as importantly decides when NOT to send any: a guest that
// enabled mode 9 alone never asked for releases, and forwarding hover to a
// guest below 1003 would feed it bytes it never requested.

import (
	"fmt"

	tea "charm.land/bubbletea/v2"

	"github.com/stump-wtf/harness/internal/mousectl"
)

// X10 wire button codes (DEC and SGR agree on these): press codes are the
// button index, release is 3, motion with a button held is that button's code
// with bit 5 set, motion with no button is 35, and the wheel is 64 upward.
const (
	mouseWireRelease = 3
	mouseWireDragBit = 32
	mouseWireHover   = 35
	// legacyMaxCoord is the largest coordinate the three-byte legacy encoding
	// can carry (it stores the coordinate offset by 32 in one byte).
	legacyMaxCoord = 222
)

// wheelCodes maps a wheel button to its wire code. The sideways wheels exist
// only where a terminal can report them, and both encodings carry the code as a
// number, so they need no special case beyond being listed.
var wheelCodes = map[tea.MouseButton]int{
	tea.MouseWheelUp:    64,
	tea.MouseWheelDown:  65,
	tea.MouseWheelLeft:  66,
	tea.MouseWheelRight: 67,
}

// pressCodes maps a button press to its wire code.
var pressCodes = map[tea.MouseButton]int{
	tea.MouseLeft:   0,
	tea.MouseMiddle: 1,
	tea.MouseRight:  2,
}

// guestOwnsMouse reports whether mouse events should reach the guest instead of
// harness's own scrollback: an interactive read-write attach whose guest has
// mouse reporting on. A read-only attach keeps the mouse because its input is
// dropped by the daemon anyway (ADR-0008), so the wheel is the only scrolling
// that viewer has; the scrollback substate keeps it for the same reason.
func (m *Model) guestOwnsMouse() bool {
	if m.att == nil || m.att.view == nil || m.att.readOnly() {
		return false
	}
	return m.att.substate == substateInteractive && m.att.view.mouse.Owned()
}

// guestWantsAllMotion reports whether the guest asked for hover events
// (DECSET 1003). It decides what the client asks its OWN terminal for: asking
// for all-motion when the guest only enabled 1000 would make the terminal
// report every pointer move to harness for nothing.
func (m *Model) guestWantsAllMotion() bool {
	return m.guestOwnsMouse() && m.att.view.mouse.Hovers()
}

// forwardGuestMouse encodes one mouse event for a guest that owns the mouse and
// queues it as attach input. It reports false when the event is not the guest's
// (no mouse reporting on, wrong substate, or the event landed outside the
// guest's viewport, since the status bar is harness's own surface), and the
// caller then handles it as harness scrollback.
func (m *Model) forwardGuestMouse(msg tea.MouseMsg) (tea.Cmd, bool) {
	if !m.guestOwnsMouse() {
		return nil, false
	}
	ev := msg.Mouse()
	view := m.att.view
	if ev.X < 0 || ev.Y < 0 || ev.X >= view.cols || ev.Y >= view.rows {
		return nil, false
	}
	shadow := &view.mouse
	code, release, ok := wireButton(msg, shadow)
	if !ok {
		return nil, false
	}
	code |= mouseModBits(ev.Mod)
	// Legacy coordinates are 1-based and capped; clamp into the guest's
	// viewport the way a terminal would report a pointer at its edge.
	x, y := ev.X+1, ev.Y+1
	if shadow.SGR() {
		term := "M"
		if release {
			term = "m"
		}
		return m.queueAttachInput(m.att.sessionID, []byte(fmt.Sprintf("\x1b[<%d;%d;%d%s", code, x, y, term))), true
	}
	if x > legacyMaxCoord || y > legacyMaxCoord {
		// Unrepresentable in three bytes, and the guest did not ask for SGR.
		return nil, false
	}
	return m.queueAttachInput(m.att.sessionID, []byte{0x1b, '[', 'M', byte(32 + code), byte(32 + x), byte(32 + y)}), true
}

// wireButton classifies one event against what the guest's mode asked for and
// returns its wire code plus whether it is a release (which only changes the SGR
// terminator). ok is false for an event the guest did not ask to receive.
func wireButton(msg tea.MouseMsg, shadow *mousectl.Shadow) (code int, release, ok bool) {
	switch ev := msg.(type) {
	case tea.MouseWheelMsg:
		c, known := wheelCodes[ev.Button]
		return c, false, known
	case tea.MouseClickMsg:
		c, known := pressCodes[ev.Button]
		return c, false, known && shadow.Presses()
	case tea.MouseReleaseMsg:
		if !shadow.Releases() {
			return 0, false, false
		}
		// SGR names the released button and ends with 'm'; the legacy encoding
		// has no button to name, so 3 is the release event itself.
		c, known := pressCodes[ev.Button]
		if !known {
			c = mouseWireRelease
		}
		return c, true, true
	case tea.MouseMotionMsg:
		if ev.Button == tea.MouseNone {
			if !shadow.Hovers() {
				return 0, false, false
			}
			return mouseWireHover, false, true
		}
		c, known := pressCodes[ev.Button]
		if !known {
			return 0, false, false
		}
		if !shadow.Drags() {
			return 0, false, false
		}
		return c | mouseWireDragBit, false, true
	}
	return 0, false, false
}

// mouseModBits folds the terminal's modifier report into the X10 code bits.
func mouseModBits(mod tea.KeyMod) int {
	bits := 0
	if mod.Contains(tea.ModShift) {
		bits |= 4
	}
	if mod.Contains(tea.ModAlt) {
		bits |= 8
	}
	if mod.Contains(tea.ModCtrl) {
		bits |= 16
	}
	return bits
}

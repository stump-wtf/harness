// Package mousectl tracks who owns the mouse while a guest terminal is being
// mirrored, and states which events the guest's own mode asks for.
//
// Governing: ADR-0055 (mouse ownership follows the guest's mouse-reporting
// modes), ADR-0003 (the daemon owns the x/vt emulator; clients mirror its
// screen), and SPEC-0002 REQ "Attach Session" (a snapshot must teach a late
// client what it cannot reconstruct from the bytes it missed).
//
// Both ends of an attach have to answer "did the guest ask for the mouse", and
// they answer it from different vantage points: the daemon's emulator sees every
// DECSET the guest ever issued, while a client only sees the ones issued after
// it attached — the same asymmetry bracketed paste already works around (see
// Mux.unwrapPasteIfUnsupported). So the daemon shadows the modes and ships them
// in the snapshot, and both ends read the events out of one table rather than
// two that can drift apart and disagree about who owns the wheel.
package mousectl

import "github.com/charmbracelet/x/ansi"

// modes are the DEC modes that decide mouse ownership, in the order Serialize
// emits them. ModeMouseExtSgr (1006) announces no events of its own — it only
// picks the SGR encoding — so it is tracked and never implies ownership.
var modes = []struct {
	mode  ansi.Mode
	set   string
	reset string
}{
	{ansi.ModeMouseX10, ansi.SetModeMouseX10, ansi.ResetModeMouseX10},
	{ansi.ModeMouseNormal, ansi.SetModeMouseNormal, ansi.ResetModeMouseNormal},
	{ansi.ModeMouseButtonEvent, ansi.SetModeMouseButtonEvent, ansi.ResetModeMouseButtonEvent},
	{ansi.ModeMouseAnyEvent, ansi.SetModeMouseAnyEvent, ansi.ResetModeMouseAnyEvent},
	{ansi.ModeMouseExtSgr, ansi.SetModeMouseExtSgr, ansi.ResetModeMouseExtSgr},
}

// indexOf maps a tracked mode to its slot in Shadow.on. Built once at init: the
// callbacks that feed a Shadow run under an emulator write.
var indexOf = func() map[ansi.Mode]int {
	m := make(map[ansi.Mode]int, len(modes))
	for i, spec := range modes {
		m[spec.mode] = i
	}
	return m
}()

// Shadow is the set of mouse-reporting modes a guest currently has on, shadowed
// from an x/vt EnableMode/DisableMode callback. x/vt fires those callbacks under
// Emulator.Write, so the enclosing type's own lock guards a Shadow; the zero
// value is a valid all-off shadow.
type Shadow struct {
	on []bool
}

// Note records that the guest enabled or disabled a mode. It reports whether
// the mode is one a Shadow tracks, so a caller that shadows other modes in the
// same callback can tell what it consumed.
func (s *Shadow) Note(mode ansi.Mode, enabled bool) bool {
	i, ok := indexOf[mode]
	if !ok {
		return false
	}
	if s.on == nil {
		s.on = make([]bool, len(modes))
	}
	s.on[i] = enabled
	return true
}

// Clear turns every tracked mode off. A client calls it when it starts a new
// attach: a RIS clears the emulator's modes without necessarily announcing that
// it did, and a shadow carried over from the previous session would keep
// forwarding a wheel the new guest never asked for.
func (s *Shadow) Clear() {
	if s.on != nil {
		s.on = make([]bool, len(s.on))
	}
}

// On reports whether the guest has a mode enabled.
func (s *Shadow) On(mode ansi.Mode) bool {
	i, ok := indexOf[mode]
	return ok && i < len(s.on) && s.on[i]
}

// Owned reports whether the guest asked for the mouse at all. While it is set
// the wheel and PgUp belong to the guest, which is what tmux and screen do;
// while it is clear, harness keeps them and its own scrollback answers the
// wheel (ADR-0055).
func (s *Shadow) Owned() bool {
	return s.Presses()
}

// Presses reports whether the guest wants button presses. Mode 9 (X10) reports
// presses only, and reports the wheel as a press, which is why the wheel can
// reach a mode-9-only guest.
func (s *Shadow) Presses() bool {
	return s.On(ansi.ModeMouseX10) || s.tracking()
}

// Releases reports whether the guest wants button releases. X10 alone never
// delivers one, so a mode-9-only guest must not receive release bytes.
func (s *Shadow) Releases() bool { return s.tracking() }

// Drags reports whether the guest wants motion while a button is held.
func (s *Shadow) Drags() bool {
	return s.On(ansi.ModeMouseButtonEvent) || s.On(ansi.ModeMouseAnyEvent)
}

// Hovers reports whether the guest wants motion with no button held. Only 1003
// asks for it; forwarding hover to a guest that did not ask would feed it
// bytes it never requested.
func (s *Shadow) Hovers() bool { return s.On(ansi.ModeMouseAnyEvent) }

// SGR reports whether the guest asked for SGR (1006) event encoding rather
// than the legacy three-byte form.
func (s *Shadow) SGR() bool { return s.On(ansi.ModeMouseExtSgr) }

// tracking reports whether any mode that reports the modern event set is on.
func (s *Shadow) tracking() bool {
	return s.On(ansi.ModeMouseNormal) || s.Drags()
}

// Serialize renders the shadow as DECSET bytes. Every tracked mode is written
// explicitly, on or off, so a snapshot is authoritative: a client that enabled
// a mode from bytes it has since forgotten cannot be left believing the guest
// still has it.
func (s *Shadow) Serialize() []byte {
	var b []byte
	for i, spec := range modes {
		if i < len(s.on) && s.on[i] {
			b = append(b, spec.set...)
		} else {
			b = append(b, spec.reset...)
		}
	}
	return b
}

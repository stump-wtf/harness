package tui

// Governing: SPEC-0001 scenario "Driving a live agent" — attached interactive
// input must forward faithfully to the harness's PTY.
//
// keyToBytes is the single translation layer between the operator's keyboard
// and a live agent, and before this file roughly three of its ~18 branches were
// exercised (and only incidentally, through attached-mode tests that assert the
// *write happened*, not what was written). A regression in any other branch
// silently corrupts every keystroke that reaches the agent — the arrow keys
// stop navigating, Ctrl-C stops interrupting, Enter stops submitting — with no
// error anywhere, because the daemon faithfully forwards whatever bytes it is
// handed.
//
// The encodings below are not arbitrary: they are what a real terminal emits,
// which is what a program reading the slave side of the PTY expects. Pinning
// them means a future refactor of the key model (Bubble Tea has already changed
// it once — v2 dropped KeyCtrlA..Z in favour of a Ctrl modifier) cannot quietly
// change the wire bytes.

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestKeyToBytesControlKeys pins the non-printable encodings one by one.
func TestKeyToBytesControlKeys(t *testing.T) {
	tests := []struct {
		name string
		msg  tea.KeyPressMsg
		want []byte
	}{
		// Line discipline. Enter is CR, not LF: a PTY in canonical mode
		// translates CR→NL itself (ICRNL), and sending NL directly bypasses
		// that, which is how "the agent ignores my Enter" bugs start.
		{"enter is CR", tea.KeyPressMsg{Code: tea.KeyEnter}, []byte{'\r'}},
		{"tab", tea.KeyPressMsg{Code: tea.KeyTab}, []byte{'\t'}},
		// DEL (0x7f), not BS (0x08) — readline and every TUI expect DEL.
		{"backspace is DEL", tea.KeyPressMsg{Code: tea.KeyBackspace}, []byte{0x7f}},
		{"delete is CSI 3~", tea.KeyPressMsg{Code: tea.KeyDelete}, []byte("\x1b[3~")},
		{"escape", tea.KeyPressMsg{Code: tea.KeyEscape}, []byte{0x1b}},

		// Cursor keys. These are the "normal" (CSI) forms rather than the
		// application-keypad (SS3, \x1bO*) forms; a full-screen guest that
		// wants SS3 requests it via DECCKM and the emulator handles that.
		{"up", tea.KeyPressMsg{Code: tea.KeyUp}, []byte("\x1b[A")},
		{"down", tea.KeyPressMsg{Code: tea.KeyDown}, []byte("\x1b[B")},
		{"right", tea.KeyPressMsg{Code: tea.KeyRight}, []byte("\x1b[C")},
		{"left", tea.KeyPressMsg{Code: tea.KeyLeft}, []byte("\x1b[D")},

		{"home", tea.KeyPressMsg{Code: tea.KeyHome}, []byte("\x1b[H")},
		{"end", tea.KeyPressMsg{Code: tea.KeyEnd}, []byte("\x1b[F")},
		{"pgup", tea.KeyPressMsg{Code: tea.KeyPgUp}, []byte("\x1b[5~")},
		{"pgdown", tea.KeyPressMsg{Code: tea.KeyPgDown}, []byte("\x1b[6~")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := keyToBytes(tt.msg)
			if string(got) != string(tt.want) {
				t.Errorf("keyToBytes(%v) = %q, want %q", tt.msg.Code, got, tt.want)
			}
		})
	}
}

// TestKeyToBytesCtrlLetters covers the Ctrl+A..Z arithmetic across the whole
// range rather than spot-checking, because the mapping is computed
// (code - 'a' + 1) rather than tabulated — an off-by-one would shift every
// control code at once, and the ones that matter most (Ctrl-C interrupt,
// Ctrl-D EOF) sit in the middle of the range where a spot check might miss.
func TestKeyToBytesCtrlLetters(t *testing.T) {
	for r := 'a'; r <= 'z'; r++ {
		want := byte(r-'a') + 1
		got := keyToBytes(tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl})
		if len(got) != 1 || got[0] != want {
			t.Errorf("ctrl+%c = %#v, want [%#x]", r, got, want)
		}
	}
	// The three that carry real semantics, named explicitly so a failure
	// reads as the behavior that broke rather than as an arithmetic slip.
	for _, tc := range []struct {
		key  rune
		want byte
		what string
	}{
		{'c', 0x03, "interrupt (SIGINT)"},
		{'d', 0x04, "EOF"},
		{'z', 0x1a, "suspend (SIGTSTP)"},
	} {
		got := keyToBytes(tea.KeyPressMsg{Code: tc.key, Mod: tea.ModCtrl})
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("ctrl+%c (%s) = %#v, want [%#x]", tc.key, tc.what, got, tc.want)
		}
	}
}

// TestKeyToBytesCtrlShiftReachesPTY is the #178 regression, with the shape
// taken from a real Ghostty session (Kitty keyboard protocol, flags=1):
// Ctrl+Shift+C arrives as ModCtrl|ModShift, and the old exact-equality Ctrl
// check swallowed it — the keystroke never reached the guest. Asserted
// positively: every Ctrl±Shift letter must produce its control byte.
func TestKeyToBytesCtrlShiftReachesPTY(t *testing.T) {
	for r := 'a'; r <= 'z'; r++ {
		got := keyToBytes(tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl | tea.ModShift})
		want := byte(r-'a') + 1
		if len(got) != 1 || got[0] != want {
			t.Errorf("ctrl+shift+%c = %#v, want [%#x] — the chord was swallowed", r, got, want)
		}
	}
}

// TestKeyToBytesCtrlAltIsEscPrefixed replaces the old negative-only guard
// (which `nil` satisfied — total swallowing read as success). Ctrl+Alt+C is
// still never a BARE interrupt, but it must also send something: the terminal
// convention for an Alt+Ctrl chord is ESC + the control code.
func TestKeyToBytesCtrlAltIsEscPrefixed(t *testing.T) {
	got := keyToBytes(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl | tea.ModAlt})
	if string(got) != "\x1b\x03" {
		t.Errorf("ctrl+alt+c = %#v, want ESC + 0x03 (alt prefix, not a bare interrupt, not nothing)", got)
	}
}

// TestKeyToBytesAltPrefixesEsc covers the modifier's meaning everywhere it can
// appear (#178 flagged the pre-existing loss: Alt+a used to forward a bare
// "a"). Alt/Meta prefix ESC on the wire, whatever the base key.
func TestKeyToBytesAltPrefixesEsc(t *testing.T) {
	tests := []struct {
		name string
		msg  tea.KeyPressMsg
		want string
	}{
		{"alt+rune", tea.KeyPressMsg{Code: 'a', Text: "a", Mod: tea.ModAlt}, "\x1ba"},
		{"meta+rune (same wire bytes as alt)", tea.KeyPressMsg{Code: 'a', Text: "a", Mod: tea.ModMeta}, "\x1ba"},
		{"alt+shift+rune", tea.KeyPressMsg{Code: 'a', Text: "A", Mod: tea.ModAlt | tea.ModShift}, "\x1bA"},
		{"alt+left (word-back in readline)", tea.KeyPressMsg{Code: tea.KeyLeft, Mod: tea.ModAlt}, "\x1b\x1b[D"},
		{"alt+enter", tea.KeyPressMsg{Code: tea.KeyEnter, Mod: tea.ModAlt}, "\x1b\r"},
		{"alt+backspace (word-delete)", tea.KeyPressMsg{Code: tea.KeyBackspace, Mod: tea.ModAlt}, "\x1b\x7f"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := keyToBytes(tt.msg); string(got) != tt.want {
				t.Errorf("keyToBytes(%v) = %q, want %q", tt.msg.Code, got, tt.want)
			}
		})
	}
}

// TestKeyToBytesNoChordReturnsNothing pins the invariant from #178: every
// chord a real terminal can report over the shapes keyToBytes handles —
// Ctrl±(Shift|Alt) letters and Alt over text, specials, and control keys —
// must produce bytes. A chord that encodes to nothing is a keystroke silently
// dropped on the floor.
func TestKeyToBytesNoChordReturnsNothing(t *testing.T) {
	mods := []tea.KeyMod{
		tea.ModCtrl,
		tea.ModCtrl | tea.ModShift,
		tea.ModCtrl | tea.ModAlt,
		tea.ModCtrl | tea.ModAlt | tea.ModShift,
		tea.ModAlt,
		tea.ModAlt | tea.ModShift,
	}
	type shape struct {
		name string
		msg  tea.KeyPressMsg
	}
	letters := []shape{
		{"a", tea.KeyPressMsg{Code: 'a', Text: "a"}},
		{"c", tea.KeyPressMsg{Code: 'c', Text: "c"}},
		{"z", tea.KeyPressMsg{Code: 'z', Text: "z"}},
	}
	specials := []shape{
		{"enter", tea.KeyPressMsg{Code: tea.KeyEnter}},
		{"left", tea.KeyPressMsg{Code: tea.KeyLeft}},
		{"backspace", tea.KeyPressMsg{Code: tea.KeyBackspace}},
		{"space", tea.KeyPressMsg{Code: ' ', Text: " "}},
	}
	for _, mod := range mods {
		for _, s := range append(append([]shape{}, letters...), specials...) {
			msg := s.msg
			msg.Mod = mod
			if got := keyToBytes(msg); len(got) == 0 {
				t.Errorf("%+v chord over %s produced no bytes — the keystroke never reaches the guest", mod, s.name)
			}
		}
	}
}

// TestKeyToBytesPrintableText covers the Text passthrough, including the cases
// most likely to be dropped by a naive `len(Text) == 1` style check: space
// (which v1 modelled as its own key type), multi-byte UTF-8, and a paste-shaped
// multi-rune Text.
func TestKeyToBytesPrintableText(t *testing.T) {
	tests := []struct {
		name string
		text string
	}{
		{"letter", "a"},
		{"digit", "7"},
		{"space", " "},
		{"punctuation", "/"},
		{"multibyte utf-8", "é"},
		{"emoji", "🙂"},
		{"multi-rune batch", "hello"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := keyToBytes(tea.KeyPressMsg{Text: tt.text})
			if string(got) != tt.text {
				t.Errorf("keyToBytes(Text=%q) = %q, want %q", tt.text, got, tt.text)
			}
		})
	}
}

// TestKeyToBytesUnencodableIsNil pins that an unrecognized key produces no
// bytes at all rather than something arbitrary. Returning nil is what lets the
// caller skip the PTY write entirely; returning an empty-but-non-nil slice, or
// a stray byte, would inject noise into the agent's stdin.
func TestKeyToBytesUnencodableIsNil(t *testing.T) {
	// A function key with no Text and no encoding branch.
	if got := keyToBytes(tea.KeyPressMsg{Code: tea.KeyF5}); got != nil {
		t.Errorf("unencodable key returned %#v, want nil", got)
	}
	// A bare modifier press.
	if got := keyToBytes(tea.KeyPressMsg{Mod: tea.ModShift}); got != nil {
		t.Errorf("bare modifier returned %#v, want nil", got)
	}
}

// TestKeyToBytesControlKeysBeatText guards the branch ordering. A KeyPressMsg
// can carry BOTH a Code the switch handles and a Text the fallthrough would
// return; the switch must win, or Enter would forward as "\n"-ish text instead
// of CR depending on how the input driver populated the message.
func TestKeyToBytesControlKeysBeatText(t *testing.T) {
	got := keyToBytes(tea.KeyPressMsg{Code: tea.KeyEnter, Text: "\n"})
	if string(got) != "\r" {
		t.Errorf("Enter carrying Text=%q encoded as %q, want %q — the Code switch must precede the Text fallthrough", "\n", got, "\r")
	}
}

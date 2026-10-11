package agentpkg

// Governing: ADR-0044 (nothing from a remote is executed), SPEC-0026 REQ-2
// (the package README is rendered with terminal control sequences
// stripped).
//
// @joestump-agent 10/09/2026 - Added for harness#929.

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSanitizeTerminal(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"plain text survives", "Set TRIAGE_REPOS.\n\tthen run it", "Set TRIAGE_REPOS.\n\tthen run it"},
		{"non-ASCII survives", "café ✓ 日本語 🚀", "café ✓ 日本語 🚀"},

		// The issue's cases.
		{"OSC 8 hyperlink, ST terminated", "see \x1b]8;;https://evil.example\x1b\\the docs\x1b]8;;\x1b\\ now", "see the docs now"},
		{"OSC 8 hyperlink, BEL terminated", "see \x1b]8;id=1;https://evil.example\athe docs\x1b]8;;\a now", "see the docs now"},
		{"CSI erase display", "before\x1b[2Jafter", "beforeafter"},
		{"CSI with params and SGR", "\x1b[1;31mred\x1b[0m \x1b[?1049h\x1b[10;20H", "red "},
		{"raw 8-bit CSI", "a\x9b2Jb", "ab"},
		{"raw 8-bit OSC to BEL", "a\x9d0;title\x07b", "ab"},
		{"raw 8-bit OSC to raw ST", "a\x9d8;;https://x\x9cb", "ab"},
		{"UTF-8 encoded C1 CSI", "a\u009b2Jb", "ab"},
		{"UTF-8 encoded C1 OSC to encoded ST", "a\u009d8;;https://x\u009cb", "ab"},
		{"lone C1 control", "a\u0085b\x85c", "abc"},

		// Other escape and string forms.
		{"OSC 52 clipboard write", "x\x1b]52;c;cm0gLXJmIH4=\ay", "xy"},
		{"OSC window title", "\x1b]0;pwned\x1b\\ok", "ok"},
		{"DCS payload", "a\x1bPq#0;2;0;0;0#0~~\x1b\\b", "ab"},
		{"APC and PM payloads", "a\x1b_payload\x1b\\b\x1b^pm\x1b\\c", "abc"},
		{"SOS payload", "a\x1bXsos\x1b\\b", "ab"},
		{"two-byte escape", "a\x1bcb\x1b7c", "abc"},
		{"nF escape with intermediate", "a\x1b(Bb", "ab"},

		// C0 controls.
		{"CRLF becomes LF", "one\r\ntwo\r\n", "one\ntwo\n"},
		{"bare CR cannot overprint", "safe text\rEVIL", "safe textEVIL"},
		{"BEL BS NUL DEL VT FF dropped", "a\a\b\x00\x7f\v\fb", "ab"},
		{"tab and newline kept", "a\tb\nc", "a\tb\nc"},

		// Unterminated and malformed sequences.
		{"unterminated OSC stops at the newline", "a\x1b]8;;https://x\nnext line", "a\nnext line"},
		{"unterminated OSC at end of input", "a\x1b]0;title", "a"},
		{"unterminated CSI at end of input", "a\x1b[12;", "a"},
		{"trailing ESC", "a\x1b", "a"},
		{"ESC before newline", "a\x1b\nb", "a\nb"},
		{"ESC inside OSC aborts it and starts a CSI", "a\x1b]0;t\x1b[2Jb", "ab"},
		{"CSI interrupted by a newline", "a\x1b[3\n1m", "a\n1m"},
		{"invalid UTF-8 outside C1 becomes U+FFFD", "a\xffb\xc3", "a�b�"},
		{"empty", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SanitizeTerminal(tc.in)
			if got != tc.want {
				t.Fatalf("SanitizeTerminal(%q) = %q, want %q", tc.in, got, tc.want)
			}
			assertInert(t, got)
		})
	}
}

// Whatever goes in, nothing a terminal interprets comes out: no ESC, no C0
// control but newline and tab, no DEL, no C1 control, and valid UTF-8.
func TestSanitizeTerminalOutputIsAlwaysInert(t *testing.T) {
	// Every byte value, alone and as the start of a sequence, plus a
	// shuffled mix of introducers and payloads.
	var inputs []string
	for b := 0; b < 256; b++ {
		inputs = append(inputs, string([]byte{byte(b)}), string([]byte{0x1b, byte(b), 'x'}), string([]byte{byte(b), '[', '2', 'J'}))
	}
	inputs = append(inputs,
		"\x1b\x1b[\x1b]\x9b\x9d\x1b\\\x9c\x07",
		strings.Repeat("\x1b]8;;", 50)+"tail",
		"\xc2\x9b\xc2\x9d\xc2\x9c\xc2",
	)
	for _, in := range inputs {
		assertInert(t, SanitizeTerminal(in))
	}
}

func assertInert(t *testing.T, s string) {
	t.Helper()
	if !utf8.ValidString(s) {
		t.Fatalf("output is not valid UTF-8: %q", s)
	}
	for _, r := range s {
		switch {
		case r == '\n' || r == '\t':
		case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
			t.Fatalf("output carries control %U: %q", r, s)
		}
	}
}

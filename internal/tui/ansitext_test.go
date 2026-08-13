package tui

import (
	"strings"
	"testing"
)

// TestInertTextDropsNonSGR verifies that cursor addressing, erases, and other
// non-SGR sequences are stripped while SGR and printable text survive.
func TestInertTextDropsNonSGR(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain text", "hello world", "hello world"},
		{"SGR preserved", "\x1b[31mred\x1b[0m", "\x1b[31mred\x1b[0m\x1b[0m"},
		{"CUP dropped", "\x1b[2;5Htext", "text"},
		{"ED dropped", "\x1b[2Jtext", "text"},
		{"EL dropped", "\x1b[Ktext", "text"},
		{"tab becomes space", "a\tb", "a b"},
		{"CR dropped", "\rtext", "text"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := inertText(tt.in, 0)
			if got != tt.want {
				t.Errorf("inertText(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestInertLinesThreadsStateAcrossBoundaries verifies that escape payloads
// split across line boundaries are fully suppressed — no payload bytes leak
// through as text (issue #146).
func TestInertLinesThreadsStateAcrossBoundaries(t *testing.T) {
	// DCS sixel payload split across 3 lines. The introducer \x1bP starts on
	// line 1, payload continues on lines 2-3, terminator \x1b\\ ends line 3.
	dcsLines := []string{
		"BEFORE",
		"\x1bP;1q#0;2;0;0;0#1;2;100;100;0",
		"#1~~@@vv@@~~$",
		"#0??}}GG}}??-",
		"\x1b\\AFTER",
	}
	out := inertLines(dcsLines)

	// BEFORE and AFTER must survive.
	if !strings.Contains(out[0], "BEFORE") {
		t.Errorf("line 0 should contain BEFORE, got %q", out[0])
	}
	if !strings.Contains(out[4], "AFTER") {
		t.Errorf("line 4 should contain AFTER, got %q", out[4])
	}

	// Payload lines must be empty or contain only safe residuals.
	for i := 1; i <= 3; i++ {
		cleaned := strings.ReplaceAll(out[i], "\x1b[0m", "") // strip SGR reset
		cleaned = strings.TrimSpace(cleaned)
		if cleaned != "" {
			t.Errorf("line %d should be empty after stripping SGR, got %q", i, out[i])
		}
	}
}

// TestInertLinesUnterminatedPayload verifies that a payload still open at the
// end of the buffer suppresses the tail rather than flushing it as text.
func TestInertLinesUnterminatedPayload(t *testing.T) {
	lines := []string{
		"OK",
		"\x1bP;1qpayload-without-terminator",
		"more-payload-bytes",
	}
	out := inertLines(lines)

	if !strings.Contains(out[0], "OK") {
		t.Errorf("line 0 should contain OK, got %q", out[0])
	}
	for i := 1; i < len(out); i++ {
		cleaned := strings.ReplaceAll(out[i], "\x1b[0m", "")
		cleaned = strings.TrimSpace(cleaned)
		if cleaned != "" {
			t.Errorf("line %d should be empty (unterminated payload), got %q", i, out[i])
		}
	}
}

// TestInertLinesOSCPayload verifies OSC payloads split across lines are
// suppressed. Uses ST (\x1b\\) as the terminator since that is what the
// ansi library recognizes for stateful OSC parsing.
func TestInertLinesOSCPayload(t *testing.T) {
	st := "\x1b\\"
	lines := []string{
		"BEFORE",
		"\x1b]0;window title with",
		"a newline in it" + st,
		"AFTER",
	}
	out := inertLines(lines)

	if !strings.Contains(out[0], "BEFORE") {
		t.Errorf("line 0 should contain BEFORE, got %q", out[0])
	}
	if !strings.Contains(out[3], "AFTER") {
		t.Errorf("line 3 should contain AFTER, got %q", out[3])
	}
	for i := 1; i <= 2; i++ {
		cleaned := strings.ReplaceAll(out[i], "\x1b[0m", "")
		cleaned = strings.TrimSpace(cleaned)
		if cleaned != "" {
			t.Errorf("line %d should be empty (OSC payload), got %q", i, out[i])
		}
	}
}

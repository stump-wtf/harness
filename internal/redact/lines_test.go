package redact

import (
	"strings"
	"testing"
)

// PEM fixtures are assembled at run time so no contiguous key block exists in
// this source for the secret scan to match. The body is low-entropy filler:
// what matters is its shape, not its value.
var (
	pemBeginLine = "-----BEGIN " + "OPENSSH PRIVATE KEY-----"
	pemEndLine   = "-----END " + "OPENSSH PRIVATE KEY-----"
	pemBodyLine  = strings.Repeat("QUJD", 16)
)

func maskAll(lines []string) []string {
	var l Lines
	out := make([]string, len(lines))
	for i, ln := range lines {
		out[i] = l.String(ln)
	}
	return out
}

// TestLinesMasksKeyBody: String masks a key only when the whole block is in
// one string. Fed a row at a time — which is how the durable log is written
// and served — the body lines after the BEGIN line are the key itself.
func TestLinesMasksKeyBody(t *testing.T) {
	got := maskAll([]string{"$ cat id_ed25519", pemBeginLine, pemBodyLine, pemBodyLine, pemEndLine, "$ ls"})
	for i, ln := range got {
		if strings.Contains(ln, pemBodyLine) {
			t.Errorf("line %d kept key material: %q", i, ln)
		}
	}
	if got[0] != "$ cat id_ed25519" || got[5] != "$ ls" {
		t.Errorf("lines outside the block changed: %q", got)
	}
	if got[4] != pemEndLine {
		t.Errorf("END line = %q, want it kept so the log shows where the block ended", got[4])
	}
}

// TestLinesKeyBodyInsideTUIFrame: an agent TUI draws its tool output inside a
// frame, so a body row arrives as "│ <base64> │" or "⎿ <base64>".
func TestLinesKeyBodyInsideTUIFrame(t *testing.T) {
	got := maskAll([]string{"  ⎿  " + pemBeginLine, "     " + pemBodyLine, "│ " + pemBodyLine + " │"})
	for i, ln := range got {
		if strings.Contains(ln, pemBodyLine) {
			t.Errorf("line %d kept key material: %q", i, ln)
		}
	}
}

// TestLinesClippedKeyEndsAtNonBody: a key whose END never arrives — a clipped
// cat, a TUI fold — must cost a few lines of masking, not the rest of the log.
func TestLinesClippedKeyEndsAtNonBody(t *testing.T) {
	got := maskAll([]string{pemBeginLine, pemBodyLine, "     … +20 lines (ctrl+o to expand)", "go test ./..."})
	if strings.Contains(got[1], pemBodyLine) {
		t.Errorf("body kept: %q", got[1])
	}
	if got[2] != "     … +20 lines (ctrl+o to expand)" || got[3] != "go test ./..." {
		t.Errorf("masking ran past the end of the block: %q", got[2:])
	}
}

// TestLinesWholeBlockOnOneLine: a block String already masked in full (a JSON
// string with escaped newlines) must not leave the stream in key mode.
func TestLinesWholeBlockOnOneLine(t *testing.T) {
	one := `"key": "` + pemBeginLine + `\n` + pemBodyLine + `\n` + pemEndLine + `"`
	got := maskAll([]string{one, "QUJD"})
	if strings.Contains(got[0], pemBodyLine) {
		t.Errorf("one-line block kept key material: %q", got[0])
	}
	if got[1] != "QUJD" {
		t.Errorf("stream stayed in key mode after a complete one-line block: %q", got[1])
	}
}

// TestLinesIsIdempotent: the durable log is masked when written and again
// when served, so a second pass must change nothing.
func TestLinesIsIdempotent(t *testing.T) {
	once := maskAll([]string{pemBeginLine, pemBodyLine, pemEndLine, "password=hunter2", "ok"})
	twice := maskAll(once)
	if strings.Join(once, "\n") != strings.Join(twice, "\n") {
		t.Errorf("not idempotent:\n%q\n%q", once, twice)
	}
}

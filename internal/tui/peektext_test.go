package tui

// Governing: #280 — the chat view (dashboard peek tail + attached scrollback)
// must show what the agent DID, not one line per second a tool ran. These
// tests pin the consumption-side sanitizer: contentless lines dropped,
// per-tick status chatter collapsed, real lines kept verbatim.

import (
	"strings"
	"testing"
)

// noisyTail is what a pre-#279 raw log tail looks like while a tool runs:
// escape-laden repaint frames, one per second, whose only change is the
// elapsed counter and the spinner glyph.
const noisyTail = "\x1b[?1049h\x1b[2J\x1b[H" +
	"\x1b[38;5;212m✻ Working (1s)…\x1b[0m\r\n" +
	"\x1b[38;5;212m✻ Working (2s)…\x1b[0m\r\n" +
	"\x1b[38;5;212m⠙ Working (3s)…\x1b[0m\r\n" +
	"\r\n" +
	"   \x1b[2K\r\n" +
	"\x1b[32mtool: bash — ls -la\x1b[0m\r\n" +
	"\x1b[38;5;212m✻ Working (4s)…\x1b[0m\r\n"

// TestSanitizeTailLinesCollapsesTickerChatter is the #280 regression: the
// per-second elapsed-counter lines collapse to one, blank/padding lines drop,
// and the meaningful tool line survives.
func TestSanitizeTailLinesCollapsesTickerChatter(t *testing.T) {
	got := sanitizeTailLines(noisyTail)
	var plain []string
	for _, ln := range got {
		plain = append(plain, chatterSignature(ln))
	}
	joined := strings.Join(plain, "\n")
	// The consecutive run (1s/2s/3s + blanks) collapses to one line; the
	// ticker resumed after the tool line is a separate occurrence and kept.
	if n := strings.Count(joined, "Working (#s)"); n != 2 {
		t.Errorf("ticker line appears %d times, want 2 (collapsed run + post-tool resume):\n%s", n, joined)
	}
	if !strings.Contains(joined, "tool: bash — ls - la") && !strings.Contains(joined, "tool: bash - ls - la") {
		// Signature collapses digits and whitespace; match loosely.
		if !strings.HasPrefix(joined, "") || !strings.Contains(strings.ToLower(joined), "tool: bash") {
			t.Errorf("tool line lost; got:\n%s", joined)
		}
	}
	if len(got) != 3 {
		t.Errorf("got %d lines, want 3 (ticker run collapsed to one, tool line, resumed ticker):\n%q", len(got), got)
	}
}

// TestSanitizeTailLinesKeepsVerbatim verifies kept lines are returned with
// their styling intact — inertness is applied downstream by inertLines, and
// TestScrollbackPreservesColor depends on SGR surviving this filter.
func TestSanitizeTailLinesKeepsVerbatim(t *testing.T) {
	got := sanitizeTailLines(noisyTail)
	for _, ln := range got {
		if strings.Contains(ln, "\x1b[32m") || strings.Contains(ln, "\x1b[38;5;212m") {
			return // styling preserved
		}
	}
	t.Errorf("no styling survived:\n%q", got)
}

// TestSanitizeTailLinesDistinctLinesSurvive verifies that genuinely different
// consecutive lines are all kept — the collapse must not eat real output.
func TestSanitizeTailLinesDistinctLinesSurvive(t *testing.T) {
	in := "reading config\nwrote 3 files\nran tests: 12 passed\n"
	got := sanitizeTailLines(in)
	if len(got) != 3 {
		t.Errorf("got %d lines, want 3:\n%q", len(got), got)
	}
}

// TestPeekLinesSanitizesHistory verifies the frozen-chat entry path: a noisy
// peek tail yields collapsed history lines for the attached harness.
func TestPeekLinesSanitizesHistory(t *testing.T) {
	m := scrollbackModelWithScreen(120, 40, noisyTail, "")
	lines := m.att.scroll.plain
	n := 0
	for _, ln := range lines {
		if strings.Contains(ln, "Working (") {
			n++
		}
	}
	// The live-screen frame appended by enterScrollback may add one more;
	// the history portion itself must be collapsed to a single ticker line.
	if n > 2 {
		t.Errorf("ticker appears %d times in frozen chat, want <= 2:\n%q", n, lines)
	}
}

// TestLogsMsgIdenticalFetchIsNoOp verifies the #280 idle-churn half: a poll
// returning the same tail does not replace the stored peek.
func TestLogsMsgIdenticalFetchIsNoOp(t *testing.T) {
	m := scrollbackModelWithScreen(120, 40, "stable tail\n", "")
	first := m.peek
	m.Update(logsMsg{name: m.harnesses[0].Name, text: first.text})
	if m.peek != first {
		t.Error("identical fetch replaced the peek state")
	}
	m.Update(logsMsg{name: m.harnesses[0].Name, text: "stable tail\nnew line\n"})
	if m.peek.text == first.text {
		t.Error("changed fetch did not replace the peek state")
	}
}

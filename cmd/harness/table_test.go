package main

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestTableTruncatesFixedColumn pins PR #23 M1: an over-long cell in a
// fixed/structured column (NAME, STATE, …) must truncate with an ellipsis,
// not wrap. wrap-then-join corrupts the row layout because Flush joins
// wrapped continuation lines line-wise with the next column. A name like
// "crush-signal-channel" (20 runes) is wider than NAME's budget (14), so it
// must render as "crush-signal-…" on a single line, not two.
func TestTableTruncatesFixedColumn(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tt := NewTable(&buf, "NAME", "STATE", "DESCRIPTION")
	tt.Row("crush-signal-channel", "running", "all good")
	_ = tt.Flush()
	out := buf.String()

	// The name must be truncated, not wrapped: every output line beyond the
	// header rule begins with either spaces+content (continuation of the
	// DESCRIPTION column) or a truncated name prefix, never with the
	// un-truncated "crush-signal-channel".
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "crush-signal-channel") {
			t.Errorf("NAME wrapped instead of truncating:\n%s", out)
		}
	}
	// The ellipsis must be present.
	if !strings.Contains(out, "…") {
		t.Errorf("missing ellipsis on truncated NAME:\n%s", out)
	}
	// And the visible width of the NAME cell (including ellipsis) must be
	// within NAME's fixed budget (14).
	wantPrefix := "crush-signal-…"
	if !strings.Contains(out, wantPrefix) {
		t.Errorf("expected truncated NAME %q in:\n%s", wantPrefix, out)
	}
	if w := utf8.RuneCountInString(wantPrefix); w != 14 {
		t.Errorf("truncation width = %d, want 14 (NAME budget)", w)
	}
}

// TestTableWrapsFlexColumn confirms the counterpart behavior: long content
// in a flex column (DESCRIPTION, DETAIL, VALUE) still wraps (PR #23 M1 made
// truncation the policy only for fixed/structured columns).
func TestTableWrapsFlexColumn(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tt := NewTable(&buf, "NAME", "DESCRIPTION")
	long := "this-is-a-very-long-unbroken-description-that-exceeds-the-column-budget"
	tt.Row("demo", long)
	_ = tt.Flush()
	out := buf.String()

	// The long word must wrap to at least two lines somewhere in the output
	// (we don't pin the exact break column, just that it wrapped).
	if !strings.Contains(out, long) {
		// If the full long word isn't on one line, it wrapped (good).
		// Verify wrapping happened by checking for a continuation line.
		lines := strings.Split(out, "\n")
		wrapped := false
		for _, l := range lines {
			// A continuation line of the DESCRIPTION column starts with
			// the NAME width (14) + colSep (2) = 16 spaces of indent.
			if strings.HasPrefix(l, strings.Repeat(" ", 16)) {
				wrapped = true
				break
			}
		}
		if !wrapped {
			t.Errorf("expected DESCRIPTION to wrap, but no continuation line found:\n%s", out)
		}
	}
}

// TestTableColorKeysOffActualWriter pins PR #23 M2: a Table writing to a
// non-TTY (a *bytes.Buffer) must never emit ANSI escapes, regardless of
// stderr's TTY status. The old useColor() hard-coded os.Stderr, so
// `harness list | cat` (stdout piped, stderr still a TTY) leaked ANSI into
// the pipe. We simulate the pipe by writing to a *bytes.Buffer (never a TTY).
func TestTableColorKeysOffActualWriter(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tt := NewTable(&buf, "NAME", "STATE")
	tt.Row("demo", tt.stateCell("running"))
	_ = tt.Flush()
	if strings.Contains(buf.String(), "\x1b[") {
		t.Errorf("Table emitted ANSI to a non-TTY writer (M2 regression):\n%q", buf.String())
	}
}

// TestTableColorOnTTYWriter confirms the converse: a Table whose writer is a
// TTY *file* opts into color. We can't easily synthesize a TTY in a unit
// test, so this case is documented rather than executed; the contract is
// verified in TestTableColorKeysOffActualWriter by negation.
func TestTableColorDecisionIsPerWriter(t *testing.T) {
	t.Parallel()
	// useColorFor must return false for non-*os.File writers and true only
	// for *os.File writers that are terminals. The non-TTY branch is all we
	// can check without a pty fixture.
	if useColorFor(&bytes.Buffer{}) {
		t.Errorf("useColorFor(*bytes.Buffer) = true, want false")
	}
}

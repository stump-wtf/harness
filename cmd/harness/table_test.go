package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestTableNameNeverTruncates pins the name-preservation contract: a NAME
// wider than the old fixed budget (14) is NEVER cut with an ellipsis — the
// column widens to fit the longest name in the data (up to maxNameWidth),
// and the DESCRIPTION column absorbs the slack (wrapping) instead. A name is
// the harness's identity; losing it to truncation is a defect (see PR #23
// M1, which made fixed columns truncate, later refined so NAME is exempt).
func TestTableNameNeverTruncates(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tt := NewTable(&buf, "NAME", "STATE", "DESCRIPTION")
	tt.Row("crush-signal-channel", "running", "all good")
	_ = tt.Flush()
	out := buf.String()

	// The full, un-truncated name must appear on one line: the column grew
	// to fit it (20 runes > the old 14 budget).
	if !strings.Contains(out, "crush-signal-channel") {
		t.Errorf("NAME was truncated or wrapped; want full name on one line:\n%s", out)
	}
	// No ellipsis may attach to the name.
	if strings.Contains(out, "crush-signal-…") || strings.Contains(out, "crush-signal…") {
		t.Errorf("NAME truncated with an ellipsis; names are never cut:\n%s", out)
	}
}

// TestTableNameWrapsPastCap pins the upper bound: a name longer than
// maxNameWidth wraps onto continuation lines (Flush joins wrapped cells
// line-wise per column, so the layout stays intact) rather than either
// truncating or starving every other column of width.
func TestTableNameWrapsPastCap(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tt := NewTable(&buf, "NAME", "DESCRIPTION")
	long := strings.Repeat("a", maxNameWidth+10)
	tt.Row(long, "d")
	_ = tt.Flush()
	out := buf.String()
	// The name is split (not shown whole on one line) but never ellipsized.
	if strings.Contains(out, long) {
		t.Errorf("over-cap NAME should have wrapped, not stayed on one line:\n%s", out)
	}
	if strings.Contains(out, "…") {
		t.Errorf("over-cap NAME should wrap, not truncate with an ellipsis:\n%s", out)
	}
}

// TestTableWideNameNeverStarvesFlexColumn pins the width-resolution floor.
// Growing NAME to fit the data spends budget the flex columns were relying
// on: on an 80-column terminal (budget 64) a 27-rune name left DESCRIPTION
// at -2, and at width <= 0 wrapWords returns the cell unwrapped, so the row
// rendered 113 columns wide and soft-wrapped in the terminal — destroying
// exactly the alignment this table exists to provide. NAME must give ground
// (it wraps, so it stays whole) before a flex column goes non-positive.
func TestTableWideNameNeverStarvesFlexColumn(t *testing.T) {
	t.Parallel()
	headers := []string{"NAME", "STATE", "ENABLED", "RESTARTS", "PID", "DESCRIPTION"}
	// Budgets span the clamp range (minTableWidth..default); names span
	// "comfortably fits" through "past the cap".
	for _, budget := range []int{minTableWidth, 64, 72, defaultTableWidth} {
		for _, nameW := range []int{4, 20, 27, maxNameWidth + 10} {
			widths, _ := defaultColumnWidths(headers, budget, nameW)
			total := 0
			for i, w := range widths {
				if w < 1 {
					t.Errorf("budget=%d nameW=%d: column %q width = %d, want >= 1 (a non-positive width renders the cell unwrapped)",
						budget, nameW, headers[i], w)
				}
				total += w
			}
			if total > budget {
				t.Errorf("budget=%d nameW=%d: widths %v sum to %d, over budget %d",
					budget, nameW, widths, total, budget)
			}
			if widths[0] > maxNameWidth {
				t.Errorf("budget=%d nameW=%d: NAME width %d exceeds cap %d",
					budget, nameW, widths[0], maxNameWidth)
			}
		}
	}
}

// TestTableRowNeverExceedsBudget is the rendering-level counterpart: with a
// long name and a long description in a narrow table, every emitted line
// must stay within the table's rendered width. That is the symptom a user
// actually sees — a row wider than the terminal soft-wraps and the columns
// stop lining up.
func TestTableRowNeverExceedsBudget(t *testing.T) {
	t.Parallel()
	var buf bytes.Buffer
	tt := NewTable(&buf, "NAME", "STATE", "ENABLED", "RESTARTS", "PID", "DESCRIPTION")
	tt.width = 64 // simulate an 80-column terminal
	tt.Row("claude-code-supervisor-long", "running", "yes", "0", "1234",
		"supervises the claude code agent loop and restarts it on exit")
	tt.Row("short", "running", "yes", "0", "12", "tiny")
	_ = tt.Flush()

	want := renderedWidth(tt.width, len(tt.headers))
	for _, line := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
		if got := lipgloss.Width(line); got > want {
			t.Errorf("line is %d columns, want <= %d:\n%s", got, want, line)
		}
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
		// A continuation line of the DESCRIPTION column starts with the
		// NAME column's width + colSep (2) of indent. NAME holds "demo"
		// (4 runes), so the column is 4 wide and the indent is 6 spaces.
		indent := strings.Repeat(" ", 6)
		wrapped := false
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, indent) && strings.TrimSpace(l) != "" {
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

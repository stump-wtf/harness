package tui

// Regression coverage for issue #144, added during review of PR #152.
//
// PR #152 fixes the overflow half of #144 — View() no longer exceeds m.h, and
// the 300-case matrix in visual_journeys_test.go asserts exactly that. But
// `len(lines) <= h` is a one-sided bound: a pane that renders far FEWER rows
// than it was given satisfies it just as well as a correct one, and so would
// a View() that returned "". These tests pin the other side of the bound —
// the panes must actually FILL the height budget they are handed.
//
// Both currently fail, by the amounts measured against this branch on a
// 160x45 terminal:
//
//	viewList  clamps content to h-2 and loses 2 harness rows (36 shown, 38 fit)
//	viewPeek  budgets h-5-len(summary) and wastes 3 rows (29 tail, 32 fit)
//
// The cause in both cases is the same: subtracting the Box border a second
// time. bodyHeight() already pays for it — headerRows(3)/footerRows(2)
// over-reserve by exactly 2 relative to the 2-line header and 1-line footer
// actually emitted, and that slack IS the border budget. viewPeek
// additionally reserves a blank-before-summary row that already lives inside
// summary[0], which is the third row.

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// manyHarnessModel builds a model with n plain running harnesses.
func manyHarnessModel(w, h, n int) *Model {
	m := baseModel(w, h)
	var hs []protocol.HarnessInfo
	for i := range n {
		hs = append(hs, protocol.HarnessInfo{
			Name:  fmt.Sprintf("h-%03d", i),
			State: "running",
		})
	}
	m.harnesses = hs
	// baseModel installs sample profiles; visible() would filter every
	// synthetic harness out and the pane would render the zero-state instead
	// of the rows under test.
	m.profiles = nil
	m.showAll = true
	return m
}

var harnessRowRe = regexp.MustCompile(`h-\d{3}`)

// TestListPaneFillsHeightBudget asserts the list renders every row that fits.
//
// The list pane's interior is bodyHeight() rows (lipgloss Box().Height(h) sets
// CONTENT height; the border is added outside it and is already paid for by
// the header/footer over-reservation). Two of those rows are the title and the
// blank beneath it, so with abundant harnesses the pane must show
// bodyHeight()-2 rows.
func TestListPaneFillsHeightBudget(t *testing.T) {
	for _, h := range []int{24, 45, 80} {
		m := manyHarnessModel(160, h, 500)
		want := m.bodyHeight() - 2 // title + blank
		got := len(harnessRowRe.FindAllString(m.viewList(60, m.bodyHeight()), -1))
		if got != want {
			t.Errorf("h=%d: list rendered %d harness rows, want %d (%d rows of pane wasted)",
				h, got, want, want-got)
		}
	}
}

// TestPeekPaneFillsHeightBudget asserts the peek renders every tail line that
// fits: interior(bodyHeight) - head(1) - blank(1) - len(summary).
//
// The summary for this fixture is ["", cmd, backend, exit, restarts, pid] = 6.
func TestPeekPaneFillsHeightBudget(t *testing.T) {
	const summaryLines = 6
	tailRe := regexp.MustCompile(`tail-\d{3}`)

	for _, h := range []int{24, 45, 80} {
		m := manyHarnessModel(160, h, 3)
		m.harnesses[0].PID = 12345
		m.sel = 0

		var b strings.Builder
		for i := range 400 {
			fmt.Fprintf(&b, "tail-%03d\n", i)
		}
		m.peek = logsMsg{name: m.harnesses[0].Name, text: b.String()}

		body := m.bodyHeight()
		want := max(body-2-summaryLines, 1)
		got := len(tailRe.FindAllString(m.viewPeek(95, body), -1))
		if got != want {
			t.Errorf("h=%d: peek rendered %d tail lines, want %d (%d rows of pane wasted)",
				h, got, want, want-got)
		}
	}
}

// TestDashboardPanesLeaveNoTrailingBlankRows is the implementation-agnostic
// version of the two tests above: with abundant content in both panes, the
// row directly above each pane's bottom border must carry content. It catches
// under-fill regardless of how the budget is arithmetically derived, so it
// keeps holding if the layout is restructured.
func TestDashboardPanesLeaveNoTrailingBlankRows(t *testing.T) {
	m := manyHarnessModel(160, 45, 500)
	m.harnesses[0].PID = 12345
	var b strings.Builder
	for i := range 400 {
		fmt.Fprintf(&b, "tail-%03d\n", i)
	}
	m.peek = logsMsg{name: m.harnesses[0].Name, text: b.String()}

	lines := strings.Split(m.View(), "\n")
	bottom := -1
	for i, ln := range lines {
		if strings.Contains(ln, "╰") {
			bottom = i
			break
		}
	}
	if bottom < 1 {
		t.Fatalf("no pane bottom border found in view")
	}
	last := lines[bottom-1]
	// Strip box drawing and whitespace; anything left is real content.
	content := strings.Map(func(r rune) rune {
		if r == '│' || r == ' ' || r == '\t' {
			return -1
		}
		return r
	}, last)
	if content == "" {
		t.Errorf("row above pane bottom border is blank — panes under-fill their height budget:\n%q", last)
	}
}

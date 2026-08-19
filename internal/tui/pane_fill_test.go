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
// blank beneath it, one is the scroll indicator, and each harness costs
// listRowLines (its state row plus the metadata sub-line, #199) — so the pane
// must show as many whole harnesses as divide into what's left.
func TestListPaneFillsHeightBudget(t *testing.T) {
	for _, h := range []int{24, 45, 80} {
		m := manyHarnessModel(160, h, 500)
		// With viewport scrolling (#154), one row is reserved for the scroll
		// indicator (↑N ↓N) when content overflows — which it does at 500
		// harnesses at every tested height.
		want := (m.bodyHeight() - 2 - 1) / listRowLines // title + blank + indicator
		got := len(harnessRowRe.FindAllString(m.viewList(60, m.bodyHeight()), -1))
		if got != want {
			t.Errorf("h=%d: list rendered %d harness rows, want %d (%d rows of pane wasted)",
				h, got, want, want-got)
		}
	}
}

// TestPeekPaneFillsHeightBudget asserts the peek renders every tail line that
// fits: interior(bodyHeight) - head(1) - blank(1).
//
// Since #199 the pane is head + screen and nothing else — the config summary
// that used to claim six rows at the bottom moved under the harness's own row
// in the list.
func TestPeekPaneFillsHeightBudget(t *testing.T) {
	tailRe := regexp.MustCompile(`tail-\d{3}`)

	for _, h := range []int{24, 45, 80} {
		m := manyHarnessModel(160, h, 3)
		m.harnesses[0].PID = 12345
		m.sel = 0

		var b strings.Builder
		for i := range 400 {
			fmt.Fprintf(&b, "tail-%03d\r\n", i)
		}
		m.peek = logsMsg{name: m.harnesses[0].Name, text: b.String()}

		body := m.bodyHeight()
		want := max(body-2, 1)
		got := len(tailRe.FindAllString(m.viewPeek(95, body), -1))
		if got != want {
			t.Errorf("h=%d: peek rendered %d tail lines, want %d (%d rows of pane wasted)",
				h, got, want, want-got)
		}
	}
}

// TestPeekPaneCarriesNoConfigSummary pins the other half of #199: the metadata
// block must be gone from the preview, not merely shortened. It is the pane's
// contract — head plus the guest's screen — and a regression that reintroduces
// a row of it would otherwise only show up as an off-by-one in the test above.
func TestPeekPaneCarriesNoConfigSummary(t *testing.T) {
	m := manyHarnessModel(160, 45, 3)
	m.harnesses[0].PID = 12345
	m.harnesses[0].Adapter = "crush"
	m.harnesses[0].Backend = "tmux"
	m.sel = 0
	m.peek = logsMsg{name: m.harnesses[0].Name, text: "just the guest screen\r\n"}

	got := m.viewPeek(95, m.bodyHeight())
	for _, banned := range []string{"/usr/local/bin/agentd", "tmux", "12345", "restarts"} {
		if strings.Contains(got, banned) {
			t.Errorf("peek pane still renders config summary field %q", banned)
		}
	}
	if !strings.Contains(got, "just the guest screen") {
		t.Error("peek pane dropped the guest screen it exists to show")
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

	lines := strings.Split(m.View().Content, "\n")
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

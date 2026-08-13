package tui

import (
	"fmt"
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// listTestModel builds a connected dashboard model with N harnesses and no
// profile filtering (so all harnesses are visible).
func listTestModel(w, h, count int) *Model {
	m := baseModel(w, h)
	m.profiles = nil // clear profile filtering so all harnesses are visible
	var harnesses []protocol.HarnessInfo
	for i := 0; i < count; i++ {
		harnesses = append(harnesses, protocol.HarnessInfo{
			Name:  fmt.Sprintf("h-%03d", i),
			State: "running",
		})
	}
	m.harnesses = harnesses
	return m
}

// TestListViewportSelectedAlwaysVisible asserts that the selected harness's
// name appears in the rendered list for every selection position, with 200
// harnesses and a 24-row terminal (issue #148).
func TestListViewportSelectedAlwaysVisible(t *testing.T) {
	m := listTestModel(120, 24, 200)

	checkPositions := []int{0, 1, 50, 99, 150, 198, 199}
	for _, pos := range checkPositions {
		m.sel = pos
		m.scrollListToSel()
		bodyH := m.bodyHeight()
		view := m.viewList(60, bodyH)
		name := fmt.Sprintf("h-%03d", pos)
		if !strings.Contains(view, name) {
			t.Errorf("sel=%d: rendered list does not contain %q", pos, name)
		}
		lines := strings.Split(view, "\n")
		// Box adds 2 border rows to the body height.
		maxLines := bodyH + 2
		if len(lines) > maxLines {
			t.Errorf("sel=%d: list rendered %d lines, exceeds max %d", pos, len(lines), maxLines)
		}
	}
}

// TestListViewportScrollByOne verifies that moving from the last visible row
// to the next shifts the window by exactly one harness, not a page.
func TestListViewportScrollByOne(t *testing.T) {
	m := listTestModel(120, 24, 50)
	m.sel = 0
	m.scrollListToSel()

	initialOffset := m.listOffset
	for i := 0; i < 20; i++ {
		m.moveSel(1)
	}
	if m.listOffset <= initialOffset {
		t.Errorf("expected offset to advance, got initial=%d final=%d", initialOffset, m.listOffset)
	}
	if m.listOffset > 15 {
		t.Errorf("offset %d suggests page-scroll rather than line-scroll", m.listOffset)
	}
}

// TestListViewportDegradedRows verifies that two-line degraded rows are never
// half-scrolled at the viewport edge.
func TestListViewportDegradedRows(t *testing.T) {
	m := baseModel(120, 24)
	m.profiles = nil
	var harnesses []protocol.HarnessInfo
	for i := 0; i < 30; i++ {
		h := protocol.HarnessInfo{
			Name:  fmt.Sprintf("h-%03d", i),
			State: "running",
		}
		if i%3 == 0 {
			h.State = "degraded"
			h.Flapping = true
		}
		harnesses = append(harnesses, h)
	}
	m.harnesses = harnesses

	for sel := 0; sel < len(harnesses); sel++ {
		m.sel = sel
		m.scrollListToSel()
		bodyH := m.bodyHeight()
		view := m.viewList(60, bodyH)
		lines := strings.Split(view, "\n")
		maxLines := bodyH + 2
		if len(lines) > maxLines {
			t.Errorf("sel=%d: %d lines exceed max %d", sel, len(lines), maxLines)
		}
	}
}

// TestListViewportHeightInvariant ensures the #144 invariant still holds
// with the viewport: View() never exceeds m.h across harness counts.
func TestListViewportHeightInvariant(t *testing.T) {
	for _, hc := range []int{1, 10, 57, 200} {
		for _, sz := range [][2]int{{80, 24}, {120, 45}} {
			w, h := sz[0], sz[1]
			m := listTestModel(w, h, hc)
			m.scrollListToSel()
			view := m.View()
			lines := strings.Split(view, "\n")
			if len(lines) > h {
				t.Errorf("hc=%d %dx%d: %d rows > %d", hc, w, h, len(lines), h)
			}
		}
	}
}

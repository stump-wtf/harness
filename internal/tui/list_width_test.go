package tui

// Coverage for issue #199: the list pane is sized to its content rather than to
// a fixed fraction of the window, and the peek pane's config summary moved to a
// metadata sub-line under each harness row.
//
// The width half is easy to regress silently — a layout that reserves too many
// columns still renders correctly, it just wastes them — so these tests assert
// the pane SHRINKS for short content, not merely that it fits.

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// widthModel builds a wide-window dashboard over the given harnesses.
func widthModel(w, h int, hs ...protocol.HarnessInfo) *Model {
	m := baseModel(w, h)
	m.profiles = nil
	m.showAll = true
	m.harnesses = hs
	m.sel = 0
	return m
}

// TestListPaneWidthTracksContent is the core of #199: the same window, two
// different sets of harnesses, two different pane widths. The old
// `m.w * 2 / 5` returned the same number for both.
func TestListPaneWidthTracksContent(t *testing.T) {
	const w = 175

	short := widthModel(w, 40,
		protocol.HarnessInfo{Name: "top", State: "running", Adapter: "generic", PID: 5},
	)
	long := widthModel(w, 40,
		protocol.HarnessInfo{
			Name: "a-considerably-longer-harness-name", State: "running",
			Adapter: "generic", PID: 3720939, LastExitCode: 143,
		},
	)

	sw, lw := short.listPaneWidth(), long.listPaneWidth()
	if sw >= lw {
		t.Errorf("pane did not track content: short=%d long=%d, want short < long", sw, lw)
	}
	if old := w * 2 / 5; sw >= old {
		t.Errorf("short-content pane = %d, no narrower than the old fixed %d", sw, old)
	}
	// The long case is what the ceiling exists for: one pathological row must
	// not be allowed to eat the preview.
	if ceiling := w / 3; lw > ceiling {
		t.Errorf("long-content pane = %d, want it capped at %d", lw, ceiling)
	}
}

// TestListPaneWidthBounds pins both clamps. Neither is reachable from the
// content test above, and the floor is what keeps a narrow terminal legible
// rather than collapsing the list to nothing.
func TestListPaneWidthBounds(t *testing.T) {
	tiny := widthModel(60, 14,
		protocol.HarnessInfo{Name: "n", State: "running", Adapter: "generic"},
	)
	if got := tiny.listPaneWidth(); got != minListWidth {
		t.Errorf("pane on a 60-column window = %d, want the %d floor", got, minListWidth)
	}

	// Narrower than the floor: the list takes the window and the peek gets the
	// 1-column minimum viewDashboard clamps it to. The invariant that matters
	// is that nothing exceeds m.w.
	cramped := widthModel(18, 14,
		protocol.HarnessInfo{Name: "n", State: "running", Adapter: "generic"},
	)
	if got := cramped.listPaneWidth(); got > 18 {
		t.Errorf("pane = %d on an 18-column window, wider than the window itself", got)
	}
}

// TestListRowsCarryMetadata asserts the rendered list actually shows the moved
// fields — the peek-pane test only proves they left, not that they arrived.
func TestListRowsCarryMetadata(t *testing.T) {
	m := widthModel(175, 40,
		protocol.HarnessInfo{
			Name: "agent", State: "running", Adapter: "crush",
			Backend: "tmux", PID: 4242, LastExitCode: 143,
		},
	)
	got := m.viewList(m.listPaneWidth(), m.bodyHeight())
	for _, want := range []string{"agent", "crush", "tmux", "exit 143", "pid 4242"} {
		if !strings.Contains(got, want) {
			t.Errorf("list row missing %q:\n%s", want, got)
		}
	}
}

// TestElideLeftPrefersPathSeparators covers the readability rule: a cmd cut to
// fit should read as a path fragment, not start mid-directory.
func TestElideLeftPrefersPathSeparators(t *testing.T) {
	const p = "/home/joestump-agent/.local/bin/claude"

	if got := elideLeft(p, 100); got != p {
		t.Errorf("elideLeft with room to spare = %q, want the input unchanged", got)
	}
	if got := elideLeft(p, 20); got != "…/.local/bin/claude" {
		t.Errorf("elideLeft(20) = %q, want the longest suffix starting at a separator", got)
	}
	// No separator to snap to — fall back to a plain column cut, still bounded.
	if got := elideLeft("averylongprompttexthere", 10); len([]rune(got)) > 10 {
		t.Errorf("elideLeft on separator-free text = %q, wider than the budget", got)
	}
}

// TestMetaLineKeepsTheFactsWhenCramped pins the priority rule: when the line
// cannot hold everything, the pid and exit code survive and the cmd gives way —
// truncating the joined line from the right would do the opposite.
func TestMetaLineKeepsTheFactsWhenCramped(t *testing.T) {
	what, rest := harnessMeta(protocol.HarnessInfo{
		Adapter: "generic", PID: 4242, LastExitCode: 7,
	})
	got := metaLine(what, rest, 34)
	if !strings.Contains(got, "pid 4242") || !strings.Contains(got, "exit 7") {
		t.Errorf("metaLine dropped the facts to keep the cmd: %q", got)
	}
	if len([]rune(got)) > 34 {
		t.Errorf("metaLine = %q (%d runes), over its 34-column budget", got, len([]rune(got)))
	}
}

// TestMetaLineKeepsShortCmd is the other half of the elision rule: the
// minWhatWidth floor gates the ELISION, not the field. listPaneWidth sizes the
// pane to hold `what · rest` whole, so an adapter name that fits in fewer
// than minWhatWidth columns must still render — dropping it left the pane
// sized for a field it then refused to draw, and the name vanished.
func TestMetaLineKeepsShortCmd(t *testing.T) {
	what, rest := harnessMeta(protocol.HarnessInfo{Adapter: "generic"})
	budget := lipgloss.Width(what) + 3 + lipgloss.Width(strings.Join(rest, " · "))
	if got := metaLine(what, rest, budget); !strings.Contains(got, "generic") {
		t.Errorf("metaLine(budget=%d) = %q, dropped the harness kind that fits exactly", budget, got)
	}
}

// TestListRowsRenderShortCmd is the same bug seen through the pane: the width
// the list sizes itself to must be a width the rows actually render in.
func TestListRowsRenderShortCmd(t *testing.T) {
	m := widthModel(120, 24,
		protocol.HarnessInfo{Name: "sh", State: "running", Adapter: "generic"},
	)
	got := m.viewList(m.listPaneWidth(), m.bodyHeight())
	if !strings.Contains(got, "generic") {
		t.Errorf("list pane sized to %d columns still dropped the cmd:\n%s", m.listPaneWidth(), got)
	}
}

// TestMultilinePromptStaysOneLine covers the frame-overflow half: a prompt
// written as a YAML block scalar carries newlines, and the metadata sub-line is
// counted as exactly one line by every height budget in the pane. Rendered as
// three, the dashboard outgrows m.h and the alt screen scrolls (#179).
func TestMultilinePromptStaysOneLine(t *testing.T) {
	h := protocol.HarnessInfo{Name: "multi", State: "running", Prompt: "do a\nthing\nnow"}
	if what, _ := harnessMeta(h); strings.ContainsAny(what, "\n\r\t") {
		t.Errorf("harnessMeta kept whitespace in %q", what)
	}

	hs := []protocol.HarnessInfo{h}
	for i := 0; i < 20; i++ {
		hs = append(hs, protocol.HarnessInfo{
			Name: fmt.Sprintf("h%02d", i), State: "running", Adapter: "generic",
		})
	}
	m := widthModel(300, 20, hs...)
	if n := len(strings.Split(m.viewDashboard(), "\n")); n > m.h {
		t.Errorf("dashboard rendered %d lines into a %d-row window", n, m.h)
	}
}

// TestNarrowMetaLineNeverHalfRendersAField is the #244 regression: the cadence
// field the cockpit gained sits ahead of backend and exit code, so on a narrow
// pane the joined tail overran the budget and the clamp cut THROUGH the last
// field — "exit 0" rendered as "exit", "exi", then "e".
//
// A half-rendered field is worse than a missing one because it still reads as
// a value: "exit" reports an exit code of nothing, and a backend clipped to
// "e" names something that does not exist. Every field on the line must be
// either wholly present or wholly absent, at every width.
func TestNarrowMetaLineNeverHalfRendersAField(t *testing.T) {
	h := protocol.HarnessInfo{
		Name: "stumpcloud-sweep", State: "stopped", Schedule: "30 9 * * *",
		Description: "sweep the fleet and report anything unhealthy",
		Backend:     "native", LastExitCode: 0,
	}
	what, rest := harnessMeta(h)

	// Down to the narrowest budget the pane can actually hand out:
	// minListWidth(24) - border(2) - metaIndent(4) = 18. Below that fitFields
	// has no field boundary left and falls back to a hard clamp by design, so
	// asserting there would pin an unreachable state.
	for budget := 60; budget >= 18; budget-- {
		line := metaLine(what, rest, budget)
		for _, field := range strings.Split(line, " · ") {
			if field == "" || field == what {
				continue // the cmd/prompt is elided deliberately, from the left
			}
			if strings.HasPrefix(what, field) || strings.HasSuffix(what, field) {
				continue
			}
			if !slices.Contains(rest, field) {
				t.Errorf("budget=%d produced partial field %q in %q\n(fields: %v)",
					budget, field, line, rest)
			}
		}
	}
}

// TestScheduledRowKeepsExitCodeWhereItFits pins the boundary the fix restored:
// wherever the exit code still fits alongside the cadence it must be whole,
// and where it does not it must be gone rather than clipped.
func TestScheduledRowKeepsExitCodeWhereItFits(t *testing.T) {
	h := protocol.HarnessInfo{
		Name: "sweep", State: "stopped", Schedule: "30 9 * * *",
		Backend: "native", LastExitCode: 3,
	}
	what, rest := harnessMeta(h)
	for budget := 80; budget >= 1; budget-- {
		line := metaLine(what, rest, budget)
		if strings.Contains(line, "exit") && !strings.Contains(line, "exit 3") {
			t.Fatalf("budget=%d rendered a truncated exit field: %q", budget, line)
		}
	}
}

// TestNarrowMetaLineMarksTheUnavoidableCut pins the one case fitFields cannot
// solve by dropping: a first field wider than the entire budget, where there
// is no field boundary left to cut on.
//
// That branch is reachable, not theoretical. The pane floors metaBudget at
// paneInner(minListWidth) - metaIndent = 18, and an ordinary model name clears
// it — "model claude-opus-5" is 19 columns — so a 60-column terminal lands
// there. A silent clamp names a model that does not exist; the ellipsis is
// what makes the line read as truncated instead of as a value.
func TestNarrowMetaLineMarksTheUnavoidableCut(t *testing.T) {
	floor := paneInner(minListWidth) - metaIndent

	h := protocol.HarnessInfo{
		Name: "sweep", State: "running", Model: "claude-opus-5",
		Backend: "native", LastExitCode: 0, PID: 42,
	}
	what, rest := harnessMeta(h)
	if w := lipgloss.Width(rest[0]); w <= floor {
		t.Fatalf("fixture stopped exercising the branch: %q is %d columns, floor is %d",
			rest[0], w, floor)
	}

	line := metaLine(what, rest, floor)
	if w := lipgloss.Width(line); w > floor {
		t.Errorf("line %q is %d columns, overruns the %d-column floor", line, w, floor)
	}
	if !strings.HasSuffix(line, "…") {
		t.Errorf("unavoidable cut left unmarked: %q, want a trailing ellipsis", line)
	}
}

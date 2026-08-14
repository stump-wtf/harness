package tui

// Governing: SPEC-0001 REQ "Scrollback Substate".
//
// The scrollback *buffer* (scrollback.go) was well covered; the *key handler*
// that binds keys to it was not — only `q` (exit) had a test. Everything the
// operator actually presses to move around frozen output — arrows, PageUp/Down,
// g/G, n/N, and the whole `/` search sub-machine — went through
// onScrollbackKey untested.
//
// That gap hid a real bug, fixed alongside these tests: a coalesced multi-rune
// keystroke was dropped entirely here. See TestScrollbackMultiRuneExpansion.

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// sbFixture builds an attached model frozen in scrollback over a known buffer.
// The lines are deliberately numbered so a scroll assertion can name the row it
// expected, and "alpha" recurs at a known stride for the search tests.
func sbFixture(t *testing.T, height int) (*Model, *scrollback) {
	t.Helper()
	lines := []string{
		"alpha 0", "beta 1", "alpha 2", "gamma 3",
		"alpha 4", "delta 5", "alpha 6", "epsilon 7",
		"alpha 8", "zeta 9",
	}
	m, _ := attachedFake(80, 24)
	m.att.enterScrollback(lines, height)
	return m, m.att.scroll
}

// TestScrollbackNavigationKeys pins each navigation binding to the buffer
// movement it is supposed to cause. Before this, an arrow key wired to the
// wrong scrollback method (or a PageUp passing the wrong stride) would not fail
// any test.
func TestScrollbackNavigationKeys(t *testing.T) {
	tests := []struct {
		name    string
		key     tea.KeyPressMsg
		setup   func(*scrollback)
		wantTop func(sb *scrollback) int
	}{
		{
			name:    "down moves one line",
			key:     specialKey(tea.KeyDown),
			setup:   func(sb *scrollback) { sb.top = 2 },
			wantTop: func(sb *scrollback) int { return 3 },
		},
		{
			name:    "up moves one line",
			key:     specialKey(tea.KeyUp),
			setup:   func(sb *scrollback) { sb.top = 2 },
			wantTop: func(sb *scrollback) int { return 1 },
		},
		{
			name:  "page down moves a full viewport",
			key:   specialKey(tea.KeyPgDown),
			setup: func(sb *scrollback) { sb.top = 0 },
			// A page is exactly sb.height — an off-by-one here silently skips
			// or repeats a line on every page, which is the kind of thing you
			// only notice after losing output.
			wantTop: func(sb *scrollback) int { return sb.height },
		},
		{
			name:    "page up moves a full viewport back",
			key:     specialKey(tea.KeyPgUp),
			setup:   func(sb *scrollback) { sb.top = sb.height * 2 },
			wantTop: func(sb *scrollback) int { return sb.height },
		},
		{
			name:    "g goes to the top",
			key:     runeKey("g"),
			setup:   func(sb *scrollback) { sb.top = 5 },
			wantTop: func(sb *scrollback) int { return 0 },
		},
		{
			name:    "G goes to the bottom",
			key:     runeKey("G"),
			setup:   func(sb *scrollback) { sb.top = 0 },
			wantTop: func(sb *scrollback) int { return sb.maxTop() },
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, sb := sbFixture(t, 3)
			tt.setup(sb)
			want := tt.wantTop(sb)
			m.onScrollbackKey(tt.key)
			if sb.top != want {
				t.Errorf("top = %d, want %d", sb.top, want)
			}
		})
	}
}

// TestScrollbackNavigationClamps pins that the handler cannot walk off either
// end. scrollBy clamps internally, but the handler is what feeds it strides,
// and a page-size regression is most visible at the boundaries.
func TestScrollbackNavigationClamps(t *testing.T) {
	m, sb := sbFixture(t, 3)

	sb.top = 0
	for i := 0; i < 10; i++ {
		m.onScrollbackKey(specialKey(tea.KeyPgUp))
	}
	if sb.top != 0 {
		t.Errorf("paging up from the top left top=%d, want 0", sb.top)
	}

	for i := 0; i < 10; i++ {
		m.onScrollbackKey(specialKey(tea.KeyPgDown))
	}
	if sb.top != sb.maxTop() {
		t.Errorf("paging down past the end left top=%d, want maxTop=%d", sb.top, sb.maxTop())
	}
}

// TestScrollbackExitReturnsToInteractive pins `q` leaving the frozen substate.
// Staying in scrollback means live output keeps accumulating unseen while the
// operator believes they are back on the agent.
func TestScrollbackExitReturnsToInteractive(t *testing.T) {
	m, _ := sbFixture(t, 3)
	if m.att.substate != substateScrollback {
		t.Fatal("fixture did not enter scrollback")
	}
	m.onScrollbackKey(runeKey("q"))
	if m.att.substate != substateInteractive {
		t.Error("q did not return to the interactive substate")
	}
}

// TestScrollbackSearchSubmachine walks the `/` sub-state end to end: open,
// live-preview while typing, commit on Enter. Each transition was previously
// untested at the handler level.
func TestScrollbackSearchSubmachine(t *testing.T) {
	m, sb := sbFixture(t, 3)

	m.onScrollbackKey(runeKey("/"))
	if !m.att.searchOn {
		t.Fatal("/ did not open the search input")
	}

	// Typing live-previews: the buffer is searched as characters arrive, so the
	// operator sees matches before committing.
	for _, r := range "alpha" {
		m.onScrollbackKey(runeKey(string(r)))
	}
	if sb.term != "alpha" {
		t.Errorf("live preview term = %q, want %q", sb.term, "alpha")
	}
	if len(sb.matches) == 0 {
		t.Error("live preview found no matches for a term that occurs 5 times")
	}

	m.onScrollbackKey(specialKey(tea.KeyEnter))
	if m.att.searchOn {
		t.Error("Enter did not close the search input")
	}
	if sb.term != "alpha" {
		t.Errorf("committed term = %q, want it preserved", sb.term)
	}
}

// TestScrollbackSearchEscapeClosesWithoutLosingMatches pins the Esc exit. Esc
// leaves the input; it deliberately does not clear the matches already found,
// so n/N still work on what you searched for.
func TestScrollbackSearchEscapeClosesWithoutLosingMatches(t *testing.T) {
	m, sb := sbFixture(t, 3)
	m.onScrollbackKey(runeKey("/"))
	for _, r := range "alpha" {
		m.onScrollbackKey(runeKey(string(r)))
	}
	found := len(sb.matches)
	m.onScrollbackKey(specialKey(tea.KeyEscape))
	if m.att.searchOn {
		t.Error("Esc did not close the search input")
	}
	if len(sb.matches) != found {
		t.Errorf("Esc changed the match set: %d -> %d", found, len(sb.matches))
	}
}

// TestScrollbackMatchNavigation pins n/N stepping through matches, including
// the wrap. These are bound by literal string compare rather than a key.Binding,
// which is easy to break in a refactor and produced no test failure before.
func TestScrollbackMatchNavigation(t *testing.T) {
	m, sb := sbFixture(t, 3)
	sb.search("alpha")
	if len(sb.matches) < 3 {
		t.Fatalf("fixture should have several matches, got %d", len(sb.matches))
	}
	start := sb.matchAt

	m.onScrollbackKey(runeKey("n"))
	if sb.matchAt == start {
		t.Error("n did not advance the current match")
	}
	m.onScrollbackKey(runeKey("N"))
	if sb.matchAt != start {
		t.Errorf("N did not step back to the previous match (%d, want %d)", sb.matchAt, start)
	}
}

// TestScrollbackMultiRuneExpansion is the regression test for the bug these
// tests uncovered.
//
// Bubble Tea coalesces a burst of printable runes into ONE message carrying
// "nnn" in Text. The navigation switch matches on single keys, so a coalesced
// burst matched nothing and was dropped whole: holding `n` to walk matches did
// nothing in scrollback while working fine on the dashboard, which had already
// been fixed for exactly this in issue #145.
//
// Asserted by equivalence rather than against a hard-coded index: N coalesced
// runes must land wherever N individual presses land. The two arms use
// independent fixtures because search() re-anchors on the current top, so
// reusing one buffer would compare against a moved starting point.
func TestScrollbackMultiRuneExpansion(t *testing.T) {
	mSingles, sbSingles := sbFixture(t, 3)
	sbSingles.search("alpha")
	start := sbSingles.matchAt
	for i := 0; i < 3; i++ {
		mSingles.onScrollbackKey(runeKey("n"))
	}

	mBatch, sbBatch := sbFixture(t, 3)
	sbBatch.search("alpha")
	if sbBatch.matchAt != start {
		t.Fatalf("fixtures diverged before the comparison: %d vs %d", sbBatch.matchAt, start)
	}
	mBatch.onScrollbackKey(runeKey("nnn"))

	if sbBatch.matchAt != sbSingles.matchAt {
		t.Errorf("coalesced \"nnn\" left matchAt=%d but three separate 'n' presses left %d; "+
			"a coalesced burst must not be dropped (issue #145, scrollback half)",
			sbBatch.matchAt, sbSingles.matchAt)
	}
}

// TestScrollbackMultiRuneMovementExpands covers the same expansion for a
// movement key, so the fix is not narrowly about n/N.
func TestScrollbackMultiRuneMovementExpands(t *testing.T) {
	m, sb := sbFixture(t, 3)
	sb.top = 0
	m.onScrollbackKey(runeKey("jjj"))
	if sb.top == 0 {
		t.Error("coalesced \"jjj\" did not scroll at all")
	}
	if sb.top != 3 {
		t.Errorf("coalesced \"jjj\" moved to top=%d, want 3 (one line per rune)", sb.top)
	}
}

// TestScrollbackSearchInputDoesNotExpand pins the deliberate exception: while
// the search input is focused, a coalesced burst is text the user typed or
// pasted and must reach textinput WHOLE. Expanding it there would turn one
// paste into N keystrokes and re-scan the buffer per rune.
func TestScrollbackSearchInputDoesNotExpand(t *testing.T) {
	m, sb := sbFixture(t, 3)
	m.onScrollbackKey(runeKey("/"))
	if !m.att.searchOn {
		t.Fatal("/ did not open search")
	}
	m.onScrollbackKey(runeKey("alpha"))
	if got := m.att.search.Value(); got != "alpha" {
		t.Errorf("pasted search text = %q, want %q delivered whole", got, "alpha")
	}
	if sb.term != "alpha" {
		t.Errorf("term = %q, want the pasted value searched once", sb.term)
	}
}

// TestScrollbackMultiRuneStopsAtModeChange pins the break in the expansion
// loop: a burst that opens search partway through must stop driving navigation
// on a surface that is no longer showing.
//
// It does NOT claim the remaining runes reach the input — they are currently
// dropped, same as on the dashboard. See the note in onScrollbackKey.
func TestScrollbackMultiRuneStopsAtModeChange(t *testing.T) {
	m, _ := sbFixture(t, 3)
	// "j/j": scroll, then open search, then the trailing rune must NOT scroll.
	m.att.scroll.top = 0
	m.onScrollbackKey(runeKey("j/j"))
	if !m.att.searchOn {
		t.Fatal("the / in the burst did not open search")
	}
	if m.att.scroll.top != 1 {
		t.Errorf("top = %d, want 1 — only the rune before / should have scrolled", m.att.scroll.top)
	}
}

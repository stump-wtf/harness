package tui

// Governing: SPEC-0001 REQ "Scrollback Substate" — Ctrl-b [ / PgUp freezes the
// view with ↑/↓/PgUp/PgDn/g/G navigation and `/` search over the daemon-owned
// scrollback (ADR-0007); q/Esc returns to live. Search must let the user
// navigate matches "without disturbing the live harness", so search operates on
// a frozen copy of the scrollback lines held client-side.

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// scrollback holds a frozen copy of the harness's scrollback lines plus the
// viewport/search cursor. It is created when the user enters the substate and
// never mutates the live terminal — navigating it cannot touch the harness.
type scrollback struct {
	lines   []string
	plain   []string // lines with escapes stripped and lowercased, for search
	top     int      // index of the first visible line
	height  int      // visible rows
	term    string
	matches []int // line indices matching term, ascending
	matchAt int   // index into matches of the current match
}

// newScrollback freezes lines into a scrollback view of the given height. A
// stripped, lowercased shadow copy is built once here so search matches the
// text the user can see — never escape bytes, and never split by SGR runs —
// and so the per-keystroke live-preview path allocates nothing.
func newScrollback(lines []string, height int) *scrollback {
	plain := make([]string, len(lines))
	for i, ln := range lines {
		plain[i] = strings.ToLower(ansi.Strip(ln))
	}
	sb := &scrollback{lines: lines, plain: plain, height: height}
	sb.top = sb.maxTop() // enter at the bottom (most recent), like tmux copy-mode
	return sb
}

// maxTop is the largest valid top index (so the last page is visible).
func (s *scrollback) maxTop() int {
	m := len(s.lines) - s.height
	if m < 0 {
		return 0
	}
	return m
}

// scrollBy moves the viewport by delta lines, clamped.
func (s *scrollback) scrollBy(delta int) {
	s.top = clamp(s.top+delta, 0, s.maxTop())
}

// toTop / toBottom implement g / G.
func (s *scrollback) toTop()    { s.top = 0 }
func (s *scrollback) toBottom() { s.top = s.maxTop() }

// search sets the term, recomputes matches, and jumps the viewport to the first
// match at or after the current top (SPEC-0001 scenario "Searching history").
func (s *scrollback) search(term string) {
	s.term = term
	s.matches = s.matches[:0]
	if term == "" {
		return
	}
	low := strings.ToLower(term)
	for i, ln := range s.plain {
		if strings.Contains(ln, low) {
			s.matches = append(s.matches, i)
		}
	}
	// Position on the first match at or after the current top — search moves
	// forward from where the user is looking, not back to the oldest copy —
	// falling back to the first match overall.
	s.matchAt = 0
	for i, line := range s.matches {
		if line >= s.top {
			s.matchAt = i
			break
		}
	}
	if len(s.matches) > 0 {
		s.revealMatch()
	}
}

// nextMatch / prevMatch cycle through matches, wrapping.
func (s *scrollback) nextMatch() {
	if len(s.matches) == 0 {
		return
	}
	s.matchAt = (s.matchAt + 1) % len(s.matches)
	s.revealMatch()
}

func (s *scrollback) prevMatch() {
	if len(s.matches) == 0 {
		return
	}
	s.matchAt = (s.matchAt - 1 + len(s.matches)) % len(s.matches)
	s.revealMatch()
}

// currentMatchLine returns the line index the search cursor is on, or -1.
func (s *scrollback) currentMatchLine() int {
	if len(s.matches) == 0 {
		return -1
	}
	return s.matches[s.matchAt]
}

// revealMatch scrolls so the current match line is visible (centered-ish).
func (s *scrollback) revealMatch() {
	line := s.matches[s.matchAt]
	target := line - s.height/2
	s.top = clamp(target, 0, s.maxTop())
}

// clamp bounds v to [lo, hi].
func clamp(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

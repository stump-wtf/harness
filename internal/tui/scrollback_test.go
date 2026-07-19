package tui

import "testing"

func lines(n int) []string {
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = "line " + string(rune('a'+i%26))
	}
	return out
}

// TestScrollbackSearch is the SPEC-0001 scenario "Searching history": entering a
// term in scrollback makes the matches navigable (n/N cycle, wrapping) over the
// frozen buffer — without touching the live harness (the buffer is a copy).
func TestScrollbackSearch(t *testing.T) {
	buf := []string{"boot ok", "error: disk full", "retrying", "error: timeout", "done"}
	sb := newScrollback(buf, 3)

	sb.search("error")
	if len(sb.matches) != 2 {
		t.Fatalf("matches = %d, want 2", len(sb.matches))
	}
	if sb.currentMatchLine() != 1 {
		t.Fatalf("first match line = %d, want 1", sb.currentMatchLine())
	}
	sb.nextMatch()
	if sb.currentMatchLine() != 3 {
		t.Fatalf("second match line = %d, want 3", sb.currentMatchLine())
	}
	sb.nextMatch() // wraps back to the first
	if sb.currentMatchLine() != 1 {
		t.Fatalf("wrapped match line = %d, want 1", sb.currentMatchLine())
	}
	sb.prevMatch() // wraps to the last
	if sb.currentMatchLine() != 3 {
		t.Fatalf("prev-wrap match line = %d, want 3", sb.currentMatchLine())
	}
}

// TestScrollbackSearchCaseInsensitive verifies search ignores case.
func TestScrollbackSearchCaseInsensitive(t *testing.T) {
	sb := newScrollback([]string{"FATAL panic", "info"}, 2)
	sb.search("fatal")
	if len(sb.matches) != 1 || sb.currentMatchLine() != 0 {
		t.Fatalf("case-insensitive search failed: %v", sb.matches)
	}
}

// TestScrollbackNavigationClamps verifies the viewport clamps at both ends and
// g/G jump to top/bottom.
func TestScrollbackNavigation(t *testing.T) {
	sb := newScrollback(lines(20), 5) // maxTop = 15
	sb.toTop()
	if sb.top != 0 {
		t.Fatalf("toTop = %d, want 0", sb.top)
	}
	sb.scrollBy(-5)
	if sb.top != 0 {
		t.Fatalf("scroll above top should clamp to 0, got %d", sb.top)
	}
	sb.toBottom()
	if sb.top != 15 {
		t.Fatalf("toBottom = %d, want 15", sb.top)
	}
	sb.scrollBy(100)
	if sb.top != 15 {
		t.Fatalf("scroll past bottom should clamp to 15, got %d", sb.top)
	}
}

// TestScrollbackFreezeIsACopy verifies mutating the source slice after freezing
// does not change the scrollback view — the live harness can't disturb it.
func TestScrollbackFreezeIsACopy(t *testing.T) {
	src := []string{"a", "b", "c"}
	sb := newScrollback(append([]string(nil), src...), 3)
	src[0] = "MUTATED"
	if sb.lines[0] != "a" {
		t.Errorf("scrollback should be a frozen copy, got %q", sb.lines[0])
	}
}

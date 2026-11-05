// Summary render tests: which sections survive, in what order, and what the
// cap does at the boundary.
//
// Governing: SPEC-0007 REQ "Search And Retrieval Tools", REQ "Skill Repos".
//
// @joestump-agent 11/02/2026 - Added for harness#79.
package skillserve

import (
	"strings"
	"testing"
	"unicode/utf8"
)

const renderedBody = "---\nname: x\n---\n" +
	"## Evidence\n\n- reduit#1.\n\n" +
	"## Do not\n\n- Do not guess.\n\n" +
	"## When to use\n\nA failure appears.\n\n" +
	"## Failure modes\n\n- None known.\n\n" +
	"## Invariants\n\n- Always pin.\n\n" +
	"## Steps\n\n1. Pin it.\n"

func TestSummaryKeepsFourSectionsInCanonicalOrder(t *testing.T) {
	out := Summary(skillBody(renderedBody), 0)
	for _, want := range SummarySections {
		if !strings.Contains(out, "## "+want+"\n") {
			t.Fatalf("summary lost %q: %q", want, out)
		}
	}
	for _, dropped := range []string{"## Evidence", "## Failure modes"} {
		if strings.Contains(out, dropped) {
			t.Fatalf("summary must drop %q: %q", dropped, out)
		}
	}
	if strings.Index(out, "## When to use") > strings.Index(out, "## Steps") {
		t.Fatalf("sections must keep the canonical order, not the file's: %q", out)
	}
}

func TestSummaryFallsBackToTheBody(t *testing.T) {
	out := Summary("nothing but prose here\n", 0)
	if strings.TrimSpace(out) != "nothing but prose here" {
		t.Fatalf("a body without the four sections renders as itself, got %q", out)
	}
}

func TestCapCharsBoundaries(t *testing.T) {
	s := "abcdefghij"
	if got := capChars(s, 0); got != s {
		t.Fatalf("no cap means no cut, got %q", got)
	}
	if got := capChars(s, len(s)); got != s {
		t.Fatalf("an exact fit is not a cut, got %q", got)
	}
	if got := capChars(s, 12); got != s {
		t.Fatalf("a cap past the length is not a cut, got %q", got)
	}
	for _, max := range []int{1, 5, len(truncationMarker), len(truncationMarker) + 1, 40} {
		if got := capChars(s, max); len(got) > max {
			t.Fatalf("cap %d returned %d chars: %q", max, len(got), got)
		}
	}
}

func TestCapCharsNeverCutsAUTF8Rune(t *testing.T) {
	s := "skill ✓ pinned"
	got := capChars(s, 10)
	if !utf8.ValidString(got) {
		t.Fatalf("cut produced invalid UTF-8: %q", got)
	}
	if len(got) > 10 {
		t.Fatalf("cap 10 returned %d chars", len(got))
	}
}

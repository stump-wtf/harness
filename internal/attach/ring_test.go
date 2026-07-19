package attach

// Governing: ADR-0007 (bounded scrollback ring; default ~10k lines,
// configurable) and SPEC-0002 REQ "Attach Session" ("a bounded tail of
// scrollback").

import (
	"strings"
	"testing"
)

func TestRingTailAndEviction(t *testing.T) {
	tests := []struct {
		name     string
		maxLines int
		writes   []string
		wantTail string
		wantN    int // completed lines retained
	}{
		{
			name:     "under cap keeps everything",
			maxLines: 5,
			writes:   []string{"a\n", "b\n", "c\n"},
			wantTail: "a\nb\nc\n",
			wantN:    3,
		},
		{
			name:     "evicts oldest past cap",
			maxLines: 2,
			writes:   []string{"a\n", "b\n", "c\n", "d\n"},
			wantTail: "c\nd\n",
			wantN:    2,
		},
		{
			name:     "partial line preserved",
			maxLines: 3,
			writes:   []string{"line1\n", "partial-"},
			wantTail: "line1\npartial-",
			wantN:    1,
		},
		{
			name:     "bytes split across writes coalesce into one line",
			maxLines: 3,
			writes:   []string{"hel", "lo\n"},
			wantTail: "hello\n",
			wantN:    1,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRing(tc.maxLines)
			for _, w := range tc.writes {
				r.Write([]byte(w))
			}
			if got := string(r.Tail()); got != tc.wantTail {
				t.Errorf("Tail() = %q, want %q", got, tc.wantTail)
			}
			if got := r.Lines(); got != tc.wantN {
				t.Errorf("Lines() = %d, want %d", got, tc.wantN)
			}
		})
	}
}

func TestRingDefaultCap(t *testing.T) {
	r := newRing(0)
	if r.maxLines != DefaultRingLines {
		t.Fatalf("default maxLines = %d, want %d", r.maxLines, DefaultRingLines)
	}
	// Write more than nothing and confirm tail round-trips.
	r.Write([]byte(strings.Repeat("x\n", 10)))
	if r.Lines() != 10 {
		t.Errorf("Lines() = %d, want 10", r.Lines())
	}
}

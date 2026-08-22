package chatroom

// Line Index Equivalence
//
// EventBuffer keeps the flattened render of its visible events incrementally:
// in-order arrivals append to the index, evictions trim its front, and anything
// else gives up and marks it dirty for one lazy rebuild. That bookkeeping is
// what makes a frame independent of the buffer's depth, and it is also the only
// place the buffer can silently lie — an index that drifts from the events
// renders content the buffer does not hold, with nothing to signal it.
//
// So the incremental path is checked against a from-scratch rebuild after every
// mutation, over randomized interleavings of the four things that touch it.
//
// @joestump 08/22/2026 - Added during review of the batching change.

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"

	"gitea.stump.rocks/stump.wtf/harness/internal/tui/theme"
)

// rebuildLines is what Lines() produces on a dirty buffer, computed independently.
func rebuildLines(b *EventBuffer, s *Styles) []string {
	var out []string
	for i := range b.events {
		if b.admits(b.events[i]) {
			out = append(out, b.events[i].RenderLines(s)...)
		}
	}
	return out
}

func indexEvent(kind tail.Harness, tsSec int, tool string) tail.Event {
	ts := time.Date(2026, 8, 21, 11, 0, 0, 0, time.UTC).Add(time.Duration(tsSec) * time.Second)
	return tail.Event{
		Session: tail.SessionMeta{Harness: kind, Cwd: "/srv/app"},
		Classified: classify.Event{
			Tool:      tool,
			Action:    classify.ActionExec,
			Timestamp: ts.Format(time.RFC3339),
			Summary:   "go test ./...",
		},
		ReceivedAt: ts,
	}
}

// The incremental index must equal a full rebuild after every mutation, at
// buffer caps small enough that most inserts evict.
func TestIncrementalIndexMatchesRebuild(t *testing.T) {
	kinds := []tail.Harness{tail.HarnessClaudeCode, tail.HarnessCrush, tail.HarnessCodex}
	styles := NewStyles(theme.Default())

	for seed := int64(0); seed < 200; seed++ {
		rng := rand.New(rand.NewSource(seed))
		size := 3 + rng.Intn(8)
		b := NewEventBuffer(size)
		clock := 0

		for step := 0; step < 60; step++ {
			switch n := rng.Intn(10); {
			case n < 6: // in-order arrival: the append fast path
				clock++
				b.Insert(MakeRenderable(indexEvent(kinds[rng.Intn(len(kinds))], clock, fmt.Sprintf("T%d", step))), styles)
			case n < 8: // out-of-order arrival: sorts into the middle, invalidates
				b.Insert(MakeRenderable(indexEvent(kinds[rng.Intn(len(kinds))], clock-rng.Intn(5), fmt.Sprintf("O%d", step))), styles)
			case n < 9: // a different filter is a different set of lines
				b.SetFilter(FilterSet(rng.Intn(32)))
			default: // read it, which settles any pending rebuild
				_ = b.Lines(styles)
			}

			got, want := b.Lines(styles), rebuildLines(b, styles)
			if len(got) != len(want) {
				t.Fatalf("seed %d step %d cap %d: index has %d lines, rebuild has %d",
					seed, step, size, len(got), len(want))
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("seed %d step %d cap %d: line %d\n got: %q\nwant: %q",
						seed, step, size, i, got[i], want[i])
				}
			}
		}
	}
}

// View must return exactly the rows it was given, including at the degenerate
// sizes — a body joined to a status bar is one row too many when the whole pane
// is one row, which is the same overflow the row truncation guards against.
func TestViewIsExactlyTheHeightItWasGiven(t *testing.T) {
	for _, h := range []int{1, 2, 3, 10, 40} {
		m := New(theme.Default(), nil)
		m.SetSize(80, h)
		for i := 0; i < 5; i++ {
			m.Add(indexEvent(tail.HarnessClaudeCode, i, "Bash"))
		}
		m.Settle()

		if got := strings.Count(m.View(), "\n") + 1; got != h {
			t.Errorf("height %d: View rendered %d rows", h, got)
		}
	}
}

package chatroom

import (
	"fmt"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"

	"gitea.stump.rocks/stump.wtf/harness/internal/tui/theme"
)

// benchEvent produces a monotonically increasing timestamp. An earlier version
// wrapped the minute field every 3600 events, so at the 10k cap most inserts
// landed in the middle of the buffer and the benchmark was measuring the
// insertion scan rather than the render cycle it names.
func benchEvent(i int) tail.Event {
	return tail.Event{
		Session: tail.SessionMeta{Harness: tail.HarnessClaudeCode},
		Classified: classify.Event{
			Tool:      "Bash",
			Action:    classify.ActionExec,
			Timestamp: benchStamp(i),
			Summary:   "go test ./internal/... -run TestSomething -count=1 -race",
		},
	}
}

func benchStamp(i int) string {
	return time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC).
		Add(time.Duration(i) * time.Second).Format(time.RFC3339)
}

// BenchmarkEventCycle measures what Bubble Tea actually does per message: the
// model's update, then View — tea.Program.render calls View after EVERY message
// it processes, not once per frame.
//
// Benchmarking only the buffering half is what let the original O(buffer) View
// through review: Insert plus the scroll anchor measured at 35us/event at the
// 10k cap while the full cycle was 470us, because View re-flattened every
// buffered event into every line on every event and kept the ~40 on screen.
//
// The number that matters here is the SHAPE, not the absolute: it must be flat
// across depths. A cycle that grows with the buffer means a live stream
// eventually outruns the update loop, and the TUI stops answering keys.
func BenchmarkEventCycle(b *testing.B) {
	for _, depth := range []int{100, 1000, 5000, 10000} {
		b.Run(fmt.Sprint(depth), func(b *testing.B) {
			m := New(theme.Default(), nil)
			m.SetSize(160, 40)
			step := func(i int) {
				m.Add(benchEvent(i))
				m.Settle()
				_ = m.View()
			}
			for i := 0; i < depth; i++ {
				step(i)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				step(depth + i)
			}
		})
	}
}

// BenchmarkAddWhileClosed is the cost of buffering an event nobody is looking
// at — the common case, since the watcher runs for the whole session and the
// chatroom is open for a fraction of it. Rendering is deferred to Lines, which
// a closed view never calls, so this must not scale with the buffer either.
func BenchmarkAddWhileClosed(b *testing.B) {
	for _, depth := range []int{100, 10000} {
		b.Run(fmt.Sprint(depth), func(b *testing.B) {
			m := New(theme.Default(), nil)
			m.SetSize(160, 40)
			for i := 0; i < depth; i++ {
				m.Add(benchEvent(i))
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				m.Add(benchEvent(depth + i))
			}
		})
	}
}

// Events do not arrive in timestamp order: the watcher scans one adapter at a
// time, so a session polled later can carry events older than one polled first.
// Those land mid-buffer and invalidate the line index.
//
// The interleave depth here is one poll interval's worth of events, which is as
// far out of order as the watcher can put them — it emits a session's events
// contiguously and moves on. A deeper interleave is not a slower version of this
// benchmark but a different cost entirely: Insert keeps the buffer sorted in a
// slice, so an event landing N from the end memmoves N events, and at the 10k
// cap a mid-buffer insert moves megabytes. Nothing the watcher does reaches
// there; a buffer that had to would want a different structure.
//
// The cost that matters is per FRAME, not per event — the index rebuilds lazily
// in Lines, so a batch of out-of-order arrivals between two frames costs one
// rebuild however many events it held. A falling ns/event as the batch grows is
// that amortisation, and it is the whole reason the watcher delivers batches.
func BenchmarkOutOfOrderBatch(b *testing.B) {
	const interleave = 50
	for _, batch := range []int{1, 16, 256} {
		b.Run(fmt.Sprint(batch), func(b *testing.B) {
			m := New(theme.Default(), nil)
			m.SetSize(160, 40)
			depth := 10000
			for i := 0; i < depth; i++ {
				m.Add(benchEvent(i))
			}
			_ = m.View()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				for j := 0; j < batch; j++ {
					// Each event sorts a little before the tail, the way two
					// sessions polled in sequence interleave.
					m.Add(benchEvent(depth + i*batch + j - interleave))
				}
				m.Settle()
				_ = m.View()
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*batch), "ns/event")
		})
	}
}

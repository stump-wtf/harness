package chatroom

import (
	"fmt"
	"testing"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"

	"gitea.stump.rocks/stump.wtf/harness/internal/tui/theme"
)

func benchEvent(i int) tail.Event {
	return tail.Event{
		Session: tail.SessionMeta{Harness: tail.HarnessClaudeCode},
		Classified: classify.Event{
			Tool:      "Bash",
			Action:    classify.ActionExec,
			Timestamp: fmt.Sprintf("2026-08-21T11:%02d:%02dZ", i/60%60, i%60),
			Summary:   "go test ./internal/... -run TestSomething -count=1 -race",
		},
	}
}

// Each incoming event re-renders the whole buffer to recompute the scroll
// anchor, so the cost of one event grows with everything already buffered.
func BenchmarkInsertAtDepth(b *testing.B) {
	for _, depth := range []int{100, 1000, 5000, 10000} {
		b.Run(fmt.Sprint(depth), func(b *testing.B) {
			m := New(theme.Default(), nil)
			m.SetSize(160, 40)
			// Mirror the MsgEvent path in Update.
			insert := func(i int) {
				re := MakeRenderable(benchEvent(i))
				re.lines = re.RenderLines(m.styles)
				m.buffer.Insert(re)
				m.scrollToBottom()
			}
			for i := 0; i < depth; i++ {
				insert(i)
			}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				insert(depth + i)
			}
		})
	}
}

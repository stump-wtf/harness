package chatroom

import (
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"

	"gitea.stump.rocks/stump.wtf/harness/internal/tui/theme"
)

func crushEvent(session, ts string) tail.Event {
	return tail.Event{
		Session:    tail.SessionMeta{Harness: tail.HarnessCrush, ID: session, Key: session},
		Classified: classify.Event{Seq: 1, Timestamp: ts, Tool: "bash", Action: classify.ActionExec, Summary: "gh pr list"},
		ReceivedAt: time.Now(),
	}
}

// TestAttributorNamesTheHarness: an attributable session shows under its
// harness's name, anything else under the tool's, and a label computed on
// arrival is recomputed when attribution changes — including the rendered
// lines the buffer had already cached.
func TestAttributorNamesTheHarness(t *testing.T) {
	m := New(theme.Default(), nil)
	owner := map[string]string{"s1": "pr-sweep"}
	m.SetAttributor(func(meta tail.SessionMeta) string { return owner[meta.ID] })
	m.Add(crushEvent("s1", "2026-09-11T13:30:05Z"))
	m.Add(crushEvent("s2", "2026-09-11T13:30:06Z"))

	vis := m.Buffer().Visible()
	if vis[0].Identity.Username != "@pr-sweep" || vis[1].Identity.Username != "@crush" {
		t.Fatalf("usernames = %q, %q; want @pr-sweep and the tool identity @crush", vis[0].Identity.Username, vis[1].Identity.Username)
	}
	_ = m.Buffer().Lines(m.styles) // populate the render caches

	owner["s2"] = "stumpcloud-sweep-pdx"
	m.Reattribute()
	if got := m.Buffer().Visible()[1].Identity.Username; got != "@stumpcloud-sweep-pdx" {
		t.Errorf("after Reattribute username = %q, want @stumpcloud-sweep-pdx", got)
	}
	if lines := strings.Join(m.Buffer().Lines(m.styles), "\n"); !strings.Contains(lines, "@stumpcloud-sweep-pdx") {
		t.Errorf("rendered lines kept the stale label:\n%s", lines)
	}

	// The filter is per tool: an attributed crush session is still a crush
	// session.
	m.SetFilter(FilterSet(0).Toggle(2))
	if n := len(m.Buffer().Visible()); n != 2 {
		t.Errorf("crush-only filter shows %d events, want 2", n)
	}
}

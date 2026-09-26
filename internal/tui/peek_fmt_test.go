package tui

// Coverage for issue #13: the dashboard's live preview renders a claude-code
// harness's stream-json readably — heartbeats collapse into one live tally,
// tool calls lead with their human-written description — while every other
// adapter keeps the byte-faithful mirror. The transformation itself is
// adapter-side (internal/adapter/streamjson.go); these tests prove the TUI
// wiring: which harness gets a formatter, that its output reaches the pane,
// and that state never bleeds across a hop.

import (
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/protocol"
)

// peekModelWithAdapter is peekModel with the first harness's adapter named.
func peekModelWithAdapter(t *testing.T, adapterName string) (*Model, *fakeAttach) {
	t.Helper()
	m, fa := peekModel()
	m.harnesses[0].Adapter = adapterName
	return m, fa
}

func TestPeekFormatsClaudeCodeStreamJSON(t *testing.T) {
	m, _ := peekModelWithAdapter(t, "claude-code")
	drain(m.syncPeekSession())
	if m.peekFmt == nil {
		t.Fatal("a claude-code selection opened a preview without a peek formatter")
	}

	m.Update(attachDataMsg{sessionID: m.peekSess, data: []byte(
		`{"type":"tool_progress","heartbeat":true,"elapsed_time_seconds":30}` + "\n" +
			`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git push","description":"Push branch to GitHub"}}]}}` + "\n")})

	if got := m.viewPeek(100, m.bodyHeight()); !strings.Contains(got, "Push branch to GitHub") {
		t.Errorf("the tool call's description did not reach the pane:\n%s", got)
	}
	if got := m.viewPeek(100, m.bodyHeight()); strings.Contains(got, `"type":"tool_progress"`) {
		t.Errorf("raw heartbeat JSON reached the pane:\n%s", got)
	}
	if !m.peekLive() {
		t.Fatal("the formatted output did not latch the painted state")
	}
}

func TestPeekOtherAdaptersStayByteFaithful(t *testing.T) {
	for _, name := range []string{"", "crush", "generic"} {
		m, _ := peekModelWithAdapter(t, name)
		drain(m.syncPeekSession())
		if m.peekFmt != nil {
			t.Fatalf("%q harness got a peek formatter; only claude-code opts in", name)
		}
		raw := `{"type":"assistant","message":{"content":[{"type":"text","text":"raw json for a non-claude backend"}]}}` + "\n"
		m.Update(attachDataMsg{sessionID: m.peekSess, data: []byte(raw)})
		if got := m.viewPeek(100, m.bodyHeight()); !strings.Contains(got, "raw json for a non-claude backend") {
			t.Errorf("%q preview no longer mirrors the guest's bytes:\n%s", name, got)
		}
	}
}

func TestPeekFormatterFollowsTheSelection(t *testing.T) {
	m, _ := peekModelWithAdapter(t, "claude-code")

	drain(m.syncPeekSession())
	m.Update(attachDataMsg{sessionID: m.peekSess, data: []byte(
		`{"type":"tool_progress","heartbeat":true,"elapsed_time_seconds":30}` + "\n")})
	if m.peekFmt == nil {
		t.Fatal("claude-code selection has no formatter")
	}

	// Hop to a generic harness: the new session must not inherit the old
	// renderer, or its state (a partial line, a live tally) bleeds across.
	m.moveSel(1)
	drain(m.syncPeekSession())
	if m.peekFmt != nil {
		t.Fatal("a generic selection inherited the previous harness's formatter")
	}
}

func TestPeekFormatterDroppedOnClose(t *testing.T) {
	m, _ := peekModelWithAdapter(t, "claude-code")
	drain(m.syncPeekSession())
	drain(m.closePeekSession())
	if m.peekFmt != nil {
		t.Fatal("closing the preview left a formatter behind")
	}
}

// Guard the fixture: these tests rely on the dashboard list carrying the
// adapter name on the wire (protocol.HarnessInfo.Adapter).
func TestHarnessInfoCarriesAdapter(t *testing.T) {
	var info protocol.HarnessInfo
	info.Adapter = "claude-code"
	if info.Adapter != "claude-code" {
		t.Fatal("unreachable")
	}
}

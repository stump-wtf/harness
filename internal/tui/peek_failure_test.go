package tui

import (
	"strings"
	"testing"

	"github.com/stump-wtf/agent-trace/tail"

	"github.com/stump-wtf/harness/internal/protocol"
)

// The peek head is the dashboard's detail surface for the harness you are
// looking at: it has the width the list pane's 22-column action field does
// not, it already carries contextual notes, and it belongs to exactly one
// harness. So the live failure reason goes there, and a failing run says why
// while it runs instead of only in the post-mortem (issue #1036).
func TestPeekHeadNamesTheLiveFailure(t *testing.T) {
	m := baseModel(160, 45)
	m.profiles = nil
	m.harnesses = []protocol.HarnessInfo{
		{Name: "sweep", Adapter: "crush", State: "running", Workdir: "/srv/app"},
	}
	m.sel = 0
	m.peek = logsMsg{name: "sweep", text: "plain tail line\n"}
	m.trackLastAction(failingSessionEvent(tail.HarnessCrush, "/srv/app", "Bash",
		"2026-10-31T11:00:00Z", "auth failed: the token was rejected"))

	view := m.viewPeek(110, 30)
	if !strings.Contains(view, "failed: auth failed: the token was rejected") {
		t.Errorf("head does not say why the newest call failed:\n%s", headLines(view))
	}
}

// Nothing is invented when nothing failed: the head reads exactly as it did.
func TestPeekHeadStaysQuietWithoutAFailure(t *testing.T) {
	m := baseModel(160, 45)
	m.profiles = nil
	m.harnesses = []protocol.HarnessInfo{
		{Name: "sweep", Adapter: "crush", State: "running", Workdir: "/srv/app"},
	}
	m.sel = 0
	m.peek = logsMsg{name: "sweep", text: "plain tail line\n"}
	m.trackLastAction(sessionEvent(tail.HarnessCrush, "/srv/app", "Read", "2026-10-31T11:00:00Z"))

	if view := m.viewPeek(110, 30); strings.Contains(view, "failed:") {
		t.Errorf("a successful newest call put a failure note in the head:\n%s", headLines(view))
	}
}

// A one-shot's preview IS `harness logs`, which already prints the excerpt
// beside (failed); the head repeating it would show the same bytes twice.
func TestPeekHeadDoesNotRepeatTheActivityView(t *testing.T) {
	m := baseModel(160, 45)
	m.profiles = nil
	m.harnesses = []protocol.HarnessInfo{
		{Name: "oneshot", Adapter: "crush", State: "running", Workdir: "/srv/app",
			Prompt: "sweep the backlog"},
	}
	m.sel = 0
	m.peekAct = peekActivity{
		name: "oneshot",
		ok:   true,
		data: protocol.LogsData{
			Name:   "oneshot",
			Source: protocol.LogSourceAgentTrace,
			Entries: []protocol.LogEntry{{
				ID: "s1/tool/1", Kind: protocol.LogEntryTool, Action: "exec", Tool: "bash",
				Summary: "tea issues list", Error: true,
				ErrorExcerpt: "auth failed: the token was rejected",
			}},
		},
	}
	m.trackLastAction(failingSessionEvent(tail.HarnessCrush, "/srv/app", "Bash",
		"2026-10-31T11:00:00Z", "auth failed: the token was rejected"))

	view := m.viewPeek(110, 30)
	if strings.Contains(view, "failed: auth failed") {
		t.Errorf("the head repeated an excerpt the activity lines already print:\n%s", headLines(view))
	}
	if !strings.Contains(view, "auth failed: the token was rejected") {
		t.Errorf("the activity view lost the excerpt it renders inline:\n%s", headLines(view))
	}
}

// headLines keeps a failing assertion readable: the frame is a pane, and the
// note belongs on its first row.
func headLines(view string) string {
	lines := strings.Split(view, "\n")
	if len(lines) > 3 {
		lines = lines[:3]
	}
	return strings.Join(lines, "\n")
}

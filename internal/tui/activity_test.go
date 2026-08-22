package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/tui/theme"
)

func sessionEvent(kind tail.Harness, cwd, tool, ts string) tail.Event {
	return tail.Event{
		Session: tail.SessionMeta{Harness: kind, Cwd: cwd},
		Classified: classify.Event{
			Tool:      tool,
			Action:    classify.ActionExec,
			Timestamp: ts,
			Summary:   "go test ./...",
		},
		ReceivedAt: time.Now(),
	}
}

// The defect this replaces: lastActions was keyed by adapter kind, so two
// harnesses sharing an adapter reported one another's work.
func TestLiveActionIsPerHarnessNotPerAdapter(t *testing.T) {
	m := &Model{}
	m.trackLastAction(sessionEvent(tail.HarnessClaudeCode, "/srv/alpha", "Bash", "2026-08-21T11:00:00Z"))

	alpha := m.liveAction(protocol.HarnessInfo{Name: "alpha", Adapter: "claude-code", Workdir: "/srv/alpha"})
	beta := m.liveAction(protocol.HarnessInfo{Name: "beta", Adapter: "claude-code", Workdir: "/srv/beta"})

	if !strings.Contains(alpha, "Bash") {
		t.Errorf("alpha ran the session in its own workdir, got %q", alpha)
	}
	if strings.TrimSpace(beta) != "" {
		t.Errorf("beta saw no activity in its workdir but reports %q", beta)
	}
}

// A session in a subdirectory of the workdir still belongs to the harness; a
// sibling directory with a shared name prefix does not.
func TestLiveActionMatchesPathComponents(t *testing.T) {
	m := &Model{}
	m.trackLastAction(sessionEvent(tail.HarnessClaudeCode, "/srv/app/internal", "Edit", "2026-08-21T11:00:00Z"))

	under := m.liveAction(protocol.HarnessInfo{Adapter: "claude-code", Workdir: "/srv/app"})
	if !strings.Contains(under, "Edit") {
		t.Errorf("a session under the workdir belongs to the harness, got %q", under)
	}

	sibling := m.liveAction(protocol.HarnessInfo{Adapter: "claude-code", Workdir: "/srv/app-old"})
	if strings.TrimSpace(sibling) != "" {
		t.Errorf("/srv/app/internal is not under /srv/app-old, got %q", sibling)
	}
}

// The watcher sees every agent on the machine. A crush harness must not adopt a
// claude-code session that happens to share its directory.
func TestLiveActionRequiresMatchingAdapter(t *testing.T) {
	m := &Model{}
	m.trackLastAction(sessionEvent(tail.HarnessClaudeCode, "/srv/app", "Bash", "2026-08-21T11:00:00Z"))

	if got := m.liveAction(protocol.HarnessInfo{Adapter: "crush", Workdir: "/srv/app"}); strings.TrimSpace(got) != "" {
		t.Errorf("crush harness adopted a claude-code session: %q", got)
	}
	// A generic harness runs an arbitrary command and may be any agent, so the
	// workdir is the only evidence and the kind is not filtered on.
	if got := m.liveAction(protocol.HarnessInfo{Adapter: "generic", Workdir: "/srv/app"}); !strings.Contains(got, "Bash") {
		t.Errorf("generic harness should match on workdir alone, got %q", got)
	}
}

// A harness with no configured workdir inherits the daemon's cwd, which names
// nothing. Guessing there is what put one action on every row.
func TestLiveActionEmptyWithoutWorkdir(t *testing.T) {
	m := &Model{}
	m.trackLastAction(sessionEvent(tail.HarnessClaudeCode, "/srv/app", "Bash", "2026-08-21T11:00:00Z"))
	if got := m.liveAction(protocol.HarnessInfo{Adapter: "claude-code"}); got != "" {
		t.Errorf("no workdir means no attribution, got %q", got)
	}
}

// Two sessions in one directory: the newest wins regardless of arrival order.
func TestLiveActionKeepsNewest(t *testing.T) {
	m := &Model{}
	m.trackLastAction(sessionEvent(tail.HarnessClaudeCode, "/srv/app", "Newer", "2026-08-21T11:30:00Z"))
	m.trackLastAction(sessionEvent(tail.HarnessClaudeCode, "/srv/app", "Older", "2026-08-21T11:00:00Z"))

	got := m.liveAction(protocol.HarnessInfo{Adapter: "claude-code", Workdir: "/srv/app"})
	if !strings.Contains(got, "Newer") {
		t.Errorf("an out-of-order older event overwrote the newest action: %q", got)
	}
}

// The activity column holds its width so the fields beside it never move. This
// is the flicker: a variable-width field re-elided the cmd path on every event.
func TestActivityColumnWidthIsStable(t *testing.T) {
	m := &Model{}
	for _, tool := range []string{"Bash", "Grep", "AVeryLongToolNameThatOverflowsTheColumn"} {
		m.trackLastAction(sessionEvent(tail.HarnessClaudeCode, "/srv/app", tool, "2026-08-21T11:00:00Z"))
		got := m.liveAction(protocol.HarnessInfo{Adapter: "claude-code", Workdir: "/srv/app"})
		if w := ansi.StringWidth(got); w != actionFieldWidth {
			t.Errorf("tool %q rendered %d columns, want %d (%q)", tool, w, actionFieldWidth, got)
		}
	}
}

// listPaneWidth must measure the line renderMetaRow draws. When the two
// disagree the rendered row is permanently over budget and metaLine elides the
// cmd path by however many columns the current action happens to occupy.
func TestListPaneWidthAccountsForActivityColumn(t *testing.T) {
	h := protocol.HarnessInfo{
		Name: "alpha", State: "running", Adapter: "claude-code",
		Workdir: "/srv/alpha", Backend: "native", PID: 4242,
	}
	m := &Model{theme: theme.Default(), w: 400, h: 40, harnesses: []protocol.HarnessInfo{h}}

	before := m.listPaneWidth()
	m.trackLastAction(sessionEvent(tail.HarnessClaudeCode, "/srv/alpha", "Bash", "2026-08-21T11:00:00Z"))
	after := m.listPaneWidth()

	if after-before != m.activityGutter() {
		t.Fatalf("pane grew by %d when the activity column appeared, want %d", after-before, m.activityGutter())
	}

	// And once it is there, the width stops moving no matter what arrives.
	for _, tool := range []string{"Grep", "Read", "AVeryLongToolNameIndeed"} {
		m.trackLastAction(sessionEvent(tail.HarnessClaudeCode, "/srv/alpha", tool, "2026-08-21T11:00:00Z"))
		if got := m.listPaneWidth(); got != after {
			t.Fatalf("tool %q resized the list pane from %d to %d", tool, after, got)
		}
	}
}

// The rendered meta row must fit the budget viewList hands it, activity column
// included — lipgloss wraps what overflows, and a wrapped row costs a display
// line the frame never budgeted.
func TestMetaRowFitsBudgetWithActivityColumn(t *testing.T) {
	h := protocol.HarnessInfo{
		Name: "alpha", State: "running", Adapter: "claude-code",
		Workdir: "/srv/alpha", Backend: "native", PID: 4242,
		Prompt: strings.Repeat("a very long prompt ", 20),
	}
	m := &Model{theme: theme.Default(), w: 120, h: 40, harnesses: []protocol.HarnessInfo{h}}
	m.trackLastAction(sessionEvent(tail.HarnessClaudeCode, "/srv/alpha", "Bash", "2026-08-21T11:00:00Z"))

	budget := paneInner(m.listPaneWidth()) - metaIndent
	row := m.renderMetaRow(h, budget)
	if got := ansi.StringWidth(row); got > budget+metaIndent {
		t.Fatalf("meta row is %d columns, budget is %d: %q", got, budget+metaIndent, row)
	}
}

// The kind is filtered on at read time, so it has to be part of the key. With
// one slot per directory, an interactive claude-code session in a crush
// harness's workdir evicted the crush entry and the harness — still working —
// rendered blank. That is the same "wrong row" failure this file exists to fix,
// arriving from the other direction.
func TestLiveActionSurvivesForeignSessionInSameDir(t *testing.T) {
	m := &Model{}
	m.trackLastAction(sessionEvent(tail.HarnessCrush, "/srv/app", "CrushBash", "2026-08-21T11:00:00Z"))
	m.trackLastAction(sessionEvent(tail.HarnessClaudeCode, "/srv/app", "ClaudeGrep", "2026-08-21T11:30:00Z"))

	crush := m.liveAction(protocol.HarnessInfo{Adapter: "crush", Workdir: "/srv/app"})
	if !strings.Contains(crush, "CrushBash") {
		t.Errorf("a foreign session in the same directory evicted the crush harness's action: %q", crush)
	}

	// And the claude-code harness still sees its own, so the key split did not
	// simply trade one eviction for the opposite one.
	claude := m.liveAction(protocol.HarnessInfo{Adapter: "claude-code", Workdir: "/srv/app"})
	if !strings.Contains(claude, "ClaudeGrep") {
		t.Errorf("claude-code harness lost its own action: %q", claude)
	}
}

// filepath.Clean leaves the root as "/", which already ends in the separator.
func TestLiveActionUnderRootWorkdir(t *testing.T) {
	m := &Model{}
	m.trackLastAction(sessionEvent(tail.HarnessClaudeCode, "/srv/app", "Bash", "2026-08-21T11:00:00Z"))

	if got := m.liveAction(protocol.HarnessInfo{Adapter: "claude-code", Workdir: "/"}); !strings.Contains(got, "Bash") {
		t.Errorf("every path is under the root workdir, got %q", got)
	}
}

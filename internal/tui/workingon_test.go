package tui

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"

	"github.com/stump-wtf/harness/internal/protocol"
)

// taskEvent is a task-launch call: a tool_use whose classified summary carries
// the description the agent wrote, with the classification tally agent-trace
// appends after it. marks are the ones the watcher parked on this event.
func taskEvent(kind tail.Harness, cwd, sessionKey, tool, desc, ts string, marks ...classify.Mark) tail.Event {
	summary := tool + " -> 0 targets, 0 outside"
	if desc != "" {
		summary = desc + " -> 0 targets, 0 outside"
	}
	return tail.Event{
		Session: tail.SessionMeta{Harness: kind, Cwd: cwd, Key: sessionKey},
		Classified: classify.Event{
			Tool:      tool,
			Action:    classify.ActionOther,
			Timestamp: ts,
			Summary:   summary,
		},
		Marks:      marks,
		ReceivedAt: time.Now(),
	}
}

func subagentMark() classify.Mark {
	return classify.Mark{Type: "subagent", Note: "Task"}
}

func runningHarness(name, adapter, workdir string) protocol.HarnessInfo {
	return protocol.HarnessInfo{Name: name, Adapter: adapter, State: "running", Workdir: workdir}
}

func stripAnsi(s string) string { return ansi.Strip(s) }

// The question the banner answers: a triggered or scheduled harness working a
// queue reads as running, and nothing said on what.
func TestWorkingOnTracksTheLatestTopLevelTask(t *testing.T) {
	m := &Model{}
	m.trackWorkingOn(taskEvent(tail.HarnessClaudeCode, "/srv/app", "s1", "Task",
		"implement the working-on banner", "2026-11-05T11:00:00Z"))

	want := "implement the working-on banner"
	if got := m.workingOn(runningHarness("sweep", "claude-code", "/srv/app")); got != want {
		t.Errorf("workingOn = %q, want %q", got, want)
	}

	// The banner outlives the call that set it: every tool the task then runs
	// is a step inside the same task, so none of them may blank it.
	m.trackWorkingOn(sessionEvent(tail.HarnessClaudeCode, "/srv/app", "Edit", "2026-11-05T11:05:00Z"))
	if got := m.workingOn(runningHarness("sweep", "claude-code", "/srv/app")); got != want {
		t.Errorf("a later tool call changed the banner to %q, want %q", got, want)
	}

	// The next top-level task replaces it.
	m.trackWorkingOn(taskEvent(tail.HarnessClaudeCode, "/srv/app", "s1", "Task",
		"open the pull request", "2026-11-05T11:10:00Z"))
	if got := m.workingOn(runningHarness("sweep", "claude-code", "/srv/app")); got != "open the pull request" {
		t.Errorf("a newer task left %q, want the newer description", got)
	}
}

// The banner is what the harness is working on, so a task a subagent launched
// must not overwrite it. The launch mark is what says which level a task sits
// at: the parent's launch opens one subagent, so the parent's own task closes
// one and reads as top level, and a launch inside it reads as delegated.
func TestWorkingOnIgnoresSubagentTasks(t *testing.T) {
	m := &Model{}
	// The parent launches a subagent; its mark rides the first event after the
	// launch, which here is the subagent's own work.
	m.trackWorkingOn(taskEvent(tail.HarnessClaudeCode, "/srv/app", "s1", "Read", "",
		"2026-11-05T11:00:00Z", subagentMark()))
	// The subagent launches a task of its own.
	m.trackWorkingOn(taskEvent(tail.HarnessClaudeCode, "/srv/app", "s1", "Agent",
		"review the diff", "2026-11-05T11:01:00Z", subagentMark()))
	if got := m.workingOn(runningHarness("sweep", "claude-code", "/srv/app")); got != "" {
		t.Errorf("a subagent's task set the banner to %q, want nothing yet", got)
	}
	// The parent's task closes, and its description is the harness's own.
	m.trackWorkingOn(taskEvent(tail.HarnessClaudeCode, "/srv/app", "s1", "Task",
		"implement the working-on banner", "2026-11-05T11:02:00Z"))
	if got, want := m.workingOn(runningHarness("sweep", "claude-code", "/srv/app")),
		"implement the working-on banner"; got != want {
		t.Errorf("workingOn = %q, want %q", got, want)
	}
	if len(m.taskNesting) != 0 {
		t.Errorf("the open-task count for a finished task was left behind: %v", m.taskNesting)
	}
}

// A launch mark whose seq lands on the task's own event is the same transcript
// read from the other side: the task is still top level.
func TestWorkingOnReadsALaunchMarkOnItsOwnEventAsTopLevel(t *testing.T) {
	m := &Model{}
	m.trackWorkingOn(taskEvent(tail.HarnessClaudeCode, "/srv/app", "s1", "Task",
		"triage the backlog", "2026-11-05T11:00:00Z", subagentMark()))
	if got, want := m.workingOn(runningHarness("sweep", "claude-code", "/srv/app")), "triage the backlog"; got != want {
		t.Errorf("workingOn = %q, want %q", got, want)
	}
}

// An adapter that reports subagents as separate sessions, which the watcher
// never lists, emits no launch mark, so its tasks arrive at no depth at all
// and are the harness's own work.
func TestWorkingOnWithoutLaunchMarksIsTopLevel(t *testing.T) {
	m := &Model{}
	m.trackWorkingOn(taskEvent(tail.HarnessCrush, "/srv/app", "s1", "agent",
		"sweep the queue", "2026-11-05T11:00:00Z"))
	if got, want := m.workingOn(runningHarness("sweep", "crush", "/srv/app")), "sweep the queue"; got != want {
		t.Errorf("workingOn = %q, want %q", got, want)
	}
}

// A task call that carried no description says nothing: agent-trace falls back
// to the tool's own name, and "working on: Task" is noise.
func TestWorkingOnNeedsADescription(t *testing.T) {
	m := &Model{}
	m.trackWorkingOn(taskEvent(tail.HarnessClaudeCode, "/srv/app", "s1", "Task", "",
		"2026-11-05T11:00:00Z"))
	if got := m.workingOn(runningHarness("sweep", "claude-code", "/srv/app")); got != "" {
		t.Errorf("an undescribed task call set the banner to %q", got)
	}
}

// Attribution follows the live action field's rule: the workdir, checked by path
// component, and the adapter's trajectory kind.
func TestWorkingOnIsAttributedByWorkdir(t *testing.T) {
	m := &Model{}
	m.trackWorkingOn(taskEvent(tail.HarnessClaudeCode, "/srv/app/internal", "s1", "Task",
		"fix the parser", "2026-11-05T11:00:00Z"))

	if got := m.workingOn(runningHarness("alpha", "claude-code", "/srv/app")); got != "fix the parser" {
		t.Errorf("a session under the workdir was not credited to the harness, got %q", got)
	}
	if got := m.workingOn(runningHarness("beta", "claude-code", "/srv/app-old")); got != "" {
		t.Errorf("a shared name prefix credited the wrong harness, got %q", got)
	}
	if got := m.workingOn(runningHarness("gamma", "crush", "/srv/app")); got != "" {
		t.Errorf("a different adapter's session was credited to a crush harness, got %q", got)
	}
	if got := m.workingOn(runningHarness("delta", "claude-code", "")); got != "" {
		t.Errorf("a harness with no workdir guessed at activity, got %q", got)
	}
}

// A harness that is not running is not working on anything. The field carries no
// timestamp of its own, so a leftover description would read as current work.
func TestWorkingOnIsSilentWhenTheHarnessIsNotRunning(t *testing.T) {
	m := &Model{}
	m.trackWorkingOn(taskEvent(tail.HarnessClaudeCode, "/srv/app", "s1", "Task",
		"implement the banner", "2026-11-05T11:00:00Z"))

	for _, state := range []string{"stopped", "stopping", "failed"} {
		h := runningHarness("sweep", "claude-code", "/srv/app")
		h.State = state
		if got := m.workingOn(h); got != "" {
			t.Errorf("state %q reported %q, want nothing", state, got)
		}
	}
}

func TestWorkingOnIsQuietWithoutATask(t *testing.T) {
	m := &Model{}
	m.trackWorkingOn(sessionEvent(tail.HarnessClaudeCode, "/srv/app", "Bash", "2026-11-05T11:00:00Z"))
	if got := m.workingOn(runningHarness("sweep", "claude-code", "/srv/app")); got != "" {
		t.Errorf("an ordinary tool call set the banner to %q", got)
	}
}

// The list row is the surface that has to answer the question without attaching,
// so the banner rides it once any harness is inside a task.
func TestListRowShowsTheWorkingOnBanner(t *testing.T) {
	m := baseModel(180, 45)
	m.profiles = nil
	m.harnesses = []protocol.HarnessInfo{runningHarness("sweep", "claude-code", "/srv/app")}
	m.sel = 0
	m.trackWorkingOn(taskEvent(tail.HarnessClaudeCode, "/srv/app", "s1", "Task",
		"issue 826", "2026-11-05T11:00:00Z"))

	row := stripAnsi(m.renderRow(m.harnesses[0], true))
	if !strings.Contains(row, "working on: issue 826") {
		t.Errorf("row does not carry the banner: %q", row)
	}
}

// A row without a task still reserves the field, so rows do not slide sideways
// as harnesses pick work up and finish it.
func TestListRowsReserveTheBannerTogether(t *testing.T) {
	m := baseModel(180, 45)
	m.profiles = nil
	m.harnesses = []protocol.HarnessInfo{
		runningHarness("busy", "claude-code", "/srv/app"),
		runningHarness("idle", "claude-code", "/srv/other"),
	}
	m.trackWorkingOn(taskEvent(tail.HarnessClaudeCode, "/srv/app", "s1", "Task",
		"issue 826", "2026-11-05T11:00:00Z"))

	busy := stripAnsi(m.renderRow(m.harnesses[0], false))
	idle := stripAnsi(m.renderRow(m.harnesses[1], false))
	if len([]rune(busy)) != len([]rune(idle)) {
		t.Errorf("the field moved the row text:\nbusy %q\nidle %q", busy, idle)
	}
	if strings.Contains(idle, "working on:") {
		t.Errorf("a harness with no task claims one: %q", idle)
	}
}

// A list pane too narrow to hold the row's own fields beside a legible clip
// prints neither: an ellipsis behind a label says a task is running without
// saying which, in the columns a blank would have spent.
func TestListRowDropsTheBannerWhenThePaneIsNarrow(t *testing.T) {
	m := baseModel(90, 30)
	m.profiles = nil
	m.harnesses = []protocol.HarnessInfo{runningHarness("sweep", "claude-code", "/srv/app")}
	m.trackWorkingOn(taskEvent(tail.HarnessClaudeCode, "/srv/app", "s1", "Task",
		"implement the working-on banner", "2026-11-05T11:00:00Z"))

	if m.workingOnGutter() != 0 {
		t.Errorf("a %d-column window reserved %d columns for the banner", m.w, m.workingOnGutter())
	}
}

// The peek head answers the question for the selected harness whatever the
// preview pane is showing, including when it is showing nothing.
func TestPeekHeadShowsTheWorkingOnBanner(t *testing.T) {
	m := baseModel(160, 45)
	m.profiles = nil
	m.harnesses = []protocol.HarnessInfo{runningHarness("sweep", "claude-code", "/srv/app")}
	m.sel = 0
	m.trackWorkingOn(taskEvent(tail.HarnessClaudeCode, "/srv/app", "s1", "Task",
		"implement the working-on banner", "2026-11-05T11:00:00Z"))

	view := stripAnsi(m.viewPeek(110, 30))
	if !strings.Contains(view, "working on: implement the working-on banner") {
		t.Errorf("peek head does not name the task:\n%s", headLines(view))
	}
}

// Attached mode is where an operator sits while a harness works, and its status
// bar is the only per-harness field it has.
func TestStatusBarShowsTheWorkingOnBanner(t *testing.T) {
	m := baseModel(120, 30)
	m.profiles = nil
	m.harnesses = []protocol.HarnessInfo{runningHarness("sweep", "claude-code", "/srv/app")}
	m.att = &attachState{name: "sweep"}
	m.trackWorkingOn(taskEvent(tail.HarnessClaudeCode, "/srv/app", "s1", "Task",
		"implement the banner", "2026-11-05T11:00:00Z"))

	bar := stripAnsi(m.viewStatusBar())
	if !strings.Contains(bar, "working on: implement the banner") {
		t.Errorf("status bar does not name the task: %q", bar)
	}
}

// The bar's key map is how you detach, so the banner is clipped before the
// bindings are ever squeezed out of the window.
func TestStatusBarKeepsTheKeyMapWhenTheBannerIsLong(t *testing.T) {
	m := baseModel(100, 30)
	m.profiles = nil
	m.harnesses = []protocol.HarnessInfo{runningHarness("sweep", "claude-code", "/srv/app")}
	m.att = &attachState{name: "sweep"}
	m.trackWorkingOn(taskEvent(tail.HarnessClaudeCode, "/srv/app", "s1", "Task",
		strings.Repeat("r", 200), "2026-11-05T11:00:00Z"))

	bar := stripAnsi(m.viewStatusBar())
	if len([]rune(bar)) > m.w {
		t.Errorf("the bar overran the window at %d columns: %q", m.w, bar)
	}
	if !strings.Contains(bar, "working on:") {
		t.Errorf("the banner was dropped instead of clipped: %q", bar)
	}
}

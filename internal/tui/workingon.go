package tui

// Working-on banner
//
// The dashboard answers "running / armed / stopped" from the daemon and "which
// tool is it in this second" from the session watcher (activity.go). Neither
// answers "what is it working on", which is the question a triggered or
// scheduled harness raises: a queue worker can run for an hour on one issue,
// and today the only way to find out which one is to attach and read the
// stream.
//
// The answer is already in the transcript. A top-level `Agent`/`Task` tool call
// carries the description the agent wrote for the task it is launching, and
// agent-trace puts that `input.description` at the head of the classified
// summary (classify/exec.go), so this is display work: no adapter change, and
// nothing new on the daemon protocol, because the banner is derived where the
// live action field is derived, from the watcher feed the TUI already consumes.
//
// Top level is the whole design. The banner says what the HARNESS is working
// on; a subagent's own task calls are one level down and must not overwrite it.
// Nesting is read off the marks, because marks are all the stream carries about
// a launch: every subagent launch leaves a `subagent` mark on its session's
// timeline, and that task's own event closes it. A launch mark's seq is the seq
// of the next event the session emits, and a nested launch's mark can only ever
// ride a later one, so counting launches against completions gives a task
// event's own depth: 1 is the harness itself, 2 and up are someone it delegated
// to. An adapter that reports subagents as separate sessions (which the watcher
// never lists) emits no launch mark at all; its tasks arrive at depth 0, and
// depth 0 is top level.
//
// Governing: ADR-0015 (the watcher feed this rides), issue #826.

import (
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stump-wtf/agent-trace/redact"
	"github.com/stump-wtf/agent-trace/tail"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/logview"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/runtrace"
)

// workingOnLabel is the prefix every surface prints before the description, so
// the field never reads as the agent's own words.
const workingOnLabel = "working on: "

// workingOnFieldWidth bounds the banner in the list row's first line. The
// description is free text an agent wrote for itself, and a long one would
// otherwise size the list pane (the pane is measured from its rows), so the
// field is clipped rather than accommodated, the same call the 22-column
// action column makes (activity.go).
const workingOnFieldWidth = 40

// minWorkingOnWidth is the narrowest clip worth drawing. Below it the field is
// an ellipsis with a label in front of it: it would say a task is running
// without saying anything about it, in the same columns a blank would.
const minWorkingOnWidth = 24

// maxTrackedNesting bounds how many sessions may hold an open-task count. The
// count is only interesting while it is non-zero, but a task whose result never
// arrives (a killed run) leaves its session's entry behind, so a dashboard left
// open for weeks would accumulate one small int per abandoned session. Clearing
// is safe: the worst it does is read one launch as top level.
const maxTrackedNesting = 256

// workingOnEntry is the newest top-level task description observed for one
// activity key.
type workingOnEntry struct {
	desc string
	at   time.Time
}

// trackWorkingOn records the task a watcher event started, when it started one
// at the harness's own level.
//
// Like trackLastAction it does not resolve the event to a harness: the earliest
// events arrive before there is a harness list to match against, so attribution
// happens at render time (activity.go), on the same (workdir, kind) key.
func (m *Model) trackWorkingOn(ev tail.Event) {
	cwd := cleanDir(ev.Session.Cwd)
	if cwd == "" {
		return // nothing to correlate against
	}

	depth := m.taskNesting[cleanSessionKey(ev)]
	for _, mk := range ev.Marks {
		if mk.Type == markSubagent {
			depth++
		}
	}
	if !isTaskTool(ev.Classified.Tool) {
		m.setTaskNesting(cleanSessionKey(ev), depth)
		return
	}
	// A task event closes the subagent its launch opened, so its own level is
	// the depth counted before closing.
	nested := depth > 1
	if depth > 0 {
		depth--
	}
	m.setTaskNesting(cleanSessionKey(ev), depth)
	if nested {
		return
	}
	desc := taskDescription(ev)
	if desc == "" {
		return
	}
	at := ev.ReceivedAt
	if ts, err := time.Parse(time.RFC3339, ev.Classified.Timestamp); err == nil {
		at = ts
	}
	key := activityKey{cwd: cwd, kind: ev.Session.Harness}
	if prev, ok := m.workingOnMap[key]; ok && prev.at.After(at) {
		return // two sessions in one directory: keep the newest task
	}
	if m.workingOnMap == nil {
		m.workingOnMap = make(map[activityKey]workingOnEntry)
	}
	m.workingOnMap[key] = workingOnEntry{desc: desc, at: at}
}

// setTaskNesting records a session's open-subagent count, dropping zero entries
// so the map holds only sessions that are inside a task.
func (m *Model) setTaskNesting(sessionKey string, depth int) {
	if sessionKey == "" {
		return
	}
	if depth <= 0 {
		delete(m.taskNesting, sessionKey)
		return
	}
	if m.taskNesting == nil {
		m.taskNesting = make(map[string]int)
	}
	if len(m.taskNesting) >= maxTrackedNesting {
		delete(m.taskNesting, sessionKey)
		return
	}
	m.taskNesting[sessionKey] = depth
}

// workingOn returns the banner for h, the description of the newest top-level
// task observed in its workdir, or "" when no task is running that the
// harness could be credited with.
func (m *Model) workingOn(h protocol.HarnessInfo) string {
	if !canBeWorking(h) || len(m.workingOnMap) == 0 {
		return ""
	}
	best, ok := newestMatch(m, h, m.workingOnMap, func(e workingOnEntry) time.Time { return e.at })
	if !ok {
		return ""
	}
	return best.desc
}

// canBeWorking reports whether h is in a state where "working on" is true
// rather than a leftover. A stopped, armed or off-hours harness is not running
// anything, and the banner carries no timestamp of its own to date a stale
// line, so it is simply absent there.
func canBeWorking(h protocol.HarnessInfo) bool {
	switch core.State(h.State) {
	case core.StateRunning, core.StateStarting, core.StateRestarting, core.StateDegraded:
		return true
	}
	return false
}

// isTaskTool reports whether a tool call launches an agent task. Claude Code
// names it `Task`, and `Agent` is the same call under the name other adapters
// (and newer Claude Code transcripts) use; the comparison ignores case because
// crush's own tool is lowercase.
func isTaskTool(tool string) bool {
	switch strings.ToLower(tool) {
	case "task", "agent":
		return true
	}
	return false
}

// markSubagent is the mark type every adapter uses for a subagent launch
// (classify.Mark).
const markSubagent = "subagent"

// taskDescription extracts the description a task call carries from its
// classified summary: the head, with the classification tally dropped, one
// line, masked the way every other transcript excerpt shown live is masked,
// and clipped. "" means the call carried nothing worth showing, including the
// case where the summary head fell back to the tool's own name.
func taskDescription(ev tail.Event) string {
	head := runtrace.ToolSummary(ev.Classified.Summary)
	if head = strings.TrimSpace(head); head == "" || strings.EqualFold(head, ev.Classified.Tool) {
		return ""
	}
	return logview.Clip(logview.Inert(redact.Redact(head)), workingOnFieldWidth)
}

// cleanSessionKey is the watcher's identity for one transcript.
func cleanSessionKey(ev tail.Event) string {
	if ev.Session.Key != "" {
		return ev.Session.Key
	}
	return ev.Session.Path
}

// workingOnGutter is the field width the list rows reserve for the banner, and
// zero when they should not reserve anything.
//
// All-or-nothing across the list, for the reason activityGutter gives: reserving
// it only on the rows that currently have a task would slide every other row's
// text sideways as harnesses picked tasks up and finished them. The field is
// padded to it (fitWorkingOn), so the pane, which sizes itself to these rows,
// keeps its width while a harness changes tasks, and pays nothing at all until
// some harness is actually inside one.
func (m *Model) workingOnGutter() int {
	b := m.workingOnBudget()
	if b == 0 {
		return 0
	}
	for _, h := range m.visible() {
		if m.workingOn(h) != "" {
			return b
		}
	}
	return 0
}

// workingOnBudget is how many columns a row may spend on the banner. It is a
// function of the window alone, never of the description on screen, which is
// what makes the field unable to resize the pane every time a harness starts a
// task. It is zero on a list pane too narrow to hold the row's own fields beside
// a legible clip.
func (m *Model) workingOnBudget() int {
	b := paneInner(m.paneCeiling()) - minListWidth
	if b > workingOnFieldWidth {
		b = workingOnFieldWidth
	}
	if b < minWorkingOnWidth {
		return 0
	}
	return b
}

// fitWorkingOn renders the banner as a field of exactly w columns: the label,
// the clipped description, and padding to w so what follows holds its position.
func fitWorkingOn(desc string, w int) string {
	if desc == "" || w < minWorkingOnWidth {
		return strings.Repeat(" ", maxInt(w, 0))
	}
	field := workingOnLabel + ansi.Truncate(desc, w-len(workingOnLabel), "…")
	if d := ansi.StringWidth(field); d < w {
		field += strings.Repeat(" ", w-d)
	}
	return field
}

// attachedHelpFloor is the width the attached status bar keeps for its key map
// whatever the left segment spends, so the banner can never squeeze the bindings
// out of the bar the user needs in order to detach.
const attachedHelpFloor = 32

// attachedWorkingOn is the banner as the attached status bar draws it: clipped
// to the columns the window can spare after the identity segment and the key-map
// floor, and "" when there is nothing running, nothing to say, or no room worth
// spending.
func (m *Model) attachedWorkingOn(baseWidth int) string {
	if m.att == nil {
		return ""
	}
	h := m.harnessByName(m.att.name)
	if h == nil {
		return ""
	}
	desc := m.workingOn(*h)
	if desc == "" {
		return ""
	}
	const sep = 3 // the " · " the field is joined with
	budget := m.w - baseWidth - sep - attachedHelpFloor - len(workingOnLabel)
	if budget < minWorkingOnWidth-len(workingOnLabel) {
		return ""
	}
	return " · " + workingOnLabel + ansi.Truncate(desc, budget, "…")
}

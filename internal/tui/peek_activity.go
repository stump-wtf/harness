package tui

// One-Shot Activity Preview
//
// A one-shot — a prompt harness, scheduled or run by hand — is not a session
// anyone sits in. Its PTY holds whatever the agent printed in headless mode:
// a wall of wrapped prose, or claude-code's stream-json, interleaved with the
// daemon's own log lines. Mirroring that into the preview showed the operator
// the least useful view of the run, while `harness logs` on the same harness
// printed what it actually did — every read, edit and command, one per line.
//
// So for a one-shot the preview renders the structured `logs` reply through
// the very renderer `harness logs` uses (internal/logview): the run header,
// the notices, then the activity with repeats collapsed. Interactive
// harnesses keep the live PTY mirror, where the screen IS the session.
//
// It falls back rather than failing. A daemon that cannot build the activity
// view — too old, or a harness whose adapter keeps no transcript — answers
// with the raw tail, and the preview then renders exactly what it always has.
//
// Governing: SPEC-0001 REQ "Dashboard" (live peek), SPEC-0002 REQ "Control
// Operations" ("logs"), ADR-0011 (a prompt harness is a one-shot).
//
// @joestump-agent 09/27/2026 - Added: a one-shot's preview and its `harness
// logs` looked nothing alike.

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/logview"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/tui/theme"
)

// peekActivityPoll is how often the preview re-fetches a one-shot's activity:
// the `harness logs --follow` interval. Building the reply parses transcripts,
// so it is not polled at the raw tail's once a second.
const peekActivityPoll = 2 * time.Second

// peekActivityMsg carries one structured `logs` reply for the preview.
type peekActivityMsg struct {
	name string
	data protocol.LogsData
	err  error
}

// peekActivity is the preview's copy of a one-shot's activity and its
// rendering.
type peekActivity struct {
	// name and data are the newest reply; ok is set when data is the activity
	// view rather than the raw tail a daemon falls back to.
	name string
	data protocol.LogsData
	ok   bool
	// lines is data rendered under theme, dropped whenever either changes.
	lines []string
	theme *theme.Theme

	// fetchName and fetchAt are the latest request; inflight holds while it
	// is unanswered, so a slow daemon gets one request at a time, not one per
	// tick.
	fetchName string
	fetchAt   time.Time
	inflight  bool
}

// nonInteractive reports whether h is a one-shot: a prompt harness, whose run
// is read afterwards rather than sat in (ADR-0011).
func nonInteractive(h protocol.HarnessInfo) bool {
	return h.Prompt != "" || h.PromptFile != "" || h.PromptTemplate != "" || h.PromptTemplateFile != ""
}

// peekActivityCmd fetches the selected one-shot's activity, at most once per
// peekActivityPoll and never twice at once. A selection change fetches at
// once: the throttle is per harness.
func (m *Model) peekActivityCmd(sel protocol.HarnessInfo) tea.Cmd {
	if m.ctrl == nil || !nonInteractive(sel) {
		return nil
	}
	a := &m.peekAct
	if a.fetchName == sel.Name && (a.inflight || time.Since(a.fetchAt) < peekActivityPoll) {
		return nil
	}
	a.fetchName, a.fetchAt, a.inflight = sel.Name, time.Now(), true
	ctrl, name := m.ctrl, sel.Name
	return func() tea.Msg {
		ld, err := ctrl.LogEvents(name, client.LogOptions{Lines: peekLines})
		return peekActivityMsg{name: name, data: ld, err: err}
	}
}

// onPeekActivity files a reply. An error keeps the last good reply on screen:
// one failed poll is not a reason to blank the pane.
func (m *Model) onPeekActivity(msg peekActivityMsg) {
	a := &m.peekAct
	if msg.name == a.fetchName {
		a.inflight = false
	}
	if msg.err != nil {
		return
	}
	if sel, ok := m.selectedHarness(); !ok || sel.Name != msg.name {
		return
	}
	// NoAgentActivity (#825) marks a run window that carried no attributable
	// session: the activity view is then notices and lifecycle lines only,
	// and the formatted live stream is the better thing to show. A daemon
	// older than ProtoMinor 20 never sets it, so ok keeps the historical
	// preference for the activity view.
	a.name, a.data, a.ok = msg.name, msg.data,
		msg.data.Source == protocol.LogSourceAgentTrace && !msg.data.NoAgentActivity
	a.lines = nil
}

// peekActivityLines is the selected one-shot's activity as display lines, or
// ok=false when the preview should render the PTY instead: the selection is
// interactive, or no activity view has arrived for it.
func (m *Model) peekActivityLines(sel protocol.HarnessInfo) ([]string, bool) {
	a := &m.peekAct
	if !nonInteractive(sel) || !a.ok || a.name != sel.Name {
		return nil, false
	}
	if a.lines == nil || a.theme != m.theme {
		a.lines, a.theme = logview.Lines(a.data, logview.NewStyle(m.theme)), m.theme
	}
	return a.lines, true
}

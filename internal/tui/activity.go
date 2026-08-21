package tui

// Live Harness Activity
//
// The dashboard's list rows carry a live "what is this agent doing" field fed
// by agent-trace's tail.Watcher (ADR-0015 REQ "Dashboard Activity Field"). The
// watcher discovers every agent session on the machine and reports the working
// directory each one ran in; it has no concept of a harness. This file is the
// correlation between the two, plus the fixed-width column the field renders
// into.
//
// Two rules govern it. Attribution is by working directory, because the cwd is
// the only thing a transcript records that a harness also knows about — the
// adapter kind alone names a whole class of harnesses, not one of them. And the
// column has a FIXED width, because the list pane sizes itself to its content:
// a field whose width tracks the action would resize the pane, and the peek
// beside it, on every event.
//
// Governing: ADR-0015 (chatroom TUI), SPEC-0015 REQ "Dashboard Activity Field"
//
// @joestump-agent 08/21/2026 - Extracted from update.go and view.go. The field
// was keyed by adapter kind, so every harness sharing an adapter showed one
// shared action — sourced from any session on the machine, harness-started or
// not — and its variable width re-elided the cmd path on every event.

import (
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/stump-wtf/agent-trace/tail"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/tui/chatroom"
)

// actionFieldWidth is the column budget the live activity field occupies in a
// list row's metadata line, whether or not it currently holds anything.
//
// It is a constant rather than the width of the current action for the same
// reason the pane reserves it up front: listPaneWidth measures the rows to size
// the pane, so a field that grew and shrank with "[SEARCH] Grep" versus
// "[EXEC] Bash" would re-lay out the whole cockpit every time an agent changed
// tools. 22 columns fits "[COMPACTION] 14:03:22" and the great majority of
// "[BADGE] Tool HH:MM:SS"; anything longer is truncated, not accommodated.
const actionFieldWidth = 22

// sessionActivity is the newest action observed in one agent session's working
// directory, with the harness kind that produced it.
type sessionActivity struct {
	kind   tail.Harness
	action string
	at     time.Time
}

// trackLastAction records an event from the dashboard watcher against the
// working directory it happened in.
//
// It deliberately does NOT resolve the event to a harness here. The watcher
// starts on daemon connect, in the same breath as the first state fetch, so the
// earliest events routinely arrive before there is a harness list to match them
// against — resolving eagerly drops exactly the events that would have made the
// field populated on arrival. Resolution happens at render time instead, which
// also means a config reload that moves a harness's workdir re-attributes on
// the next frame rather than on the next event.
func (m *Model) trackLastAction(ev tail.Event) {
	action := chatroom.LastAction(ev)
	if action == "" {
		return
	}
	cwd := cleanDir(ev.Session.Cwd)
	if cwd == "" {
		return // nothing to correlate against
	}
	at := ev.ReceivedAt
	if ts, err := time.Parse(time.RFC3339, ev.Classified.Timestamp); err == nil {
		at = ts
	}
	if prev, ok := m.lastActions[cwd]; ok && prev.at.After(at) {
		// The watcher replays a session's history in file order, but two
		// sessions in one directory interleave arbitrarily; keep the newest.
		return
	}
	if m.lastActions == nil {
		m.lastActions = make(map[string]sessionActivity)
	}
	m.lastActions[cwd] = sessionActivity{
		kind:   ev.Session.Harness,
		action: action + " " + chatroom.FormatTime(ev.Classified.Timestamp),
		at:     at,
	}
}

// liveAction returns h's activity field, padded to actionFieldWidth, or "" when
// nothing has been observed in its workdir.
//
// A harness with no configured workdir gets nothing: it inherits the daemon's
// working directory, which identifies no particular harness, and guessing there
// is what put one agent's action on every row.
//
// The correlation is the strongest evidence available, not proof. A transcript
// records the directory an agent ran in and no identifier of the process that
// spawned it, so an interactive session started by hand in a harness's workdir
// is indistinguishable from the harness's own. Session-to-PID correlation would
// settle it and no adapter reports one.
func (m *Model) liveAction(h protocol.HarnessInfo) string {
	work := cleanDir(h.Workdir)
	if work == "" || len(m.lastActions) == 0 {
		return ""
	}
	var best sessionActivity
	var found bool
	for cwd, act := range m.lastActions {
		if !underDir(cwd, work) || !adapterHandles(h.Adapter, act.kind) {
			continue
		}
		if !found || act.at.After(best.at) {
			best, found = act, true
		}
	}
	if !found {
		return ""
	}
	return fitAction(best.action)
}

// fitAction clips or pads s to exactly actionFieldWidth columns, so the fields
// that follow it on the metadata line hold their position from frame to frame.
func fitAction(s string) string {
	w := ansi.StringWidth(s)
	if w > actionFieldWidth {
		return ansi.Truncate(s, actionFieldWidth, "…")
	}
	return s + strings.Repeat(" ", actionFieldWidth-w)
}

// adapterHandles reports whether a harness declaring the given adapter could
// have produced a session of the given kind.
//
// A `generic` harness runs an arbitrary command — it may well be an agent, and
// which one is not knowable from the config — so for it the workdir is the only
// evidence there is and the kind is not filtered on. Every other adapter names
// exactly the agent it spawns.
func adapterHandles(adapter string, kind tail.Harness) bool {
	adapter = orDefault(adapter, "crush")
	if adapter == "generic" {
		return true
	}
	return adapter == string(kind)
}

// underDir reports whether path is dir or sits beneath it, comparing whole path
// components — a raw string prefix would put /srv/app-old under /srv/app.
func underDir(path, dir string) bool {
	if path == "" || dir == "" {
		return false
	}
	if path == dir {
		return true
	}
	return strings.HasPrefix(path, dir+string(filepath.Separator))
}

// cleanDir normalizes a directory for comparison. The empty string stays empty
// rather than becoming filepath.Clean's ".", which would match a relative cwd.
func cleanDir(p string) string {
	if p == "" {
		return ""
	}
	return filepath.Clean(p)
}

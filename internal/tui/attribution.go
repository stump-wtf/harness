package tui

// Chatroom Attribution
//
// Names the harness behind each session the chatroom shows. The TUI's watcher
// is machine-wide — it discovers every session every agent tool on this
// machine wrote — so a session is credited to a harness only when the run
// correlation rule finds exactly one harness that could have written it: same
// adapter, same workdir, and a run whose window covers the session's start.
// Anything else keeps its tool identity (@crush, @claude-code), which is true
// of every session and claims nothing.
//
// The daemon reports only each harness's latest run (LastStarted/LastExitAt),
// so a session from an earlier run keeps its tool label. That under-reports;
// it never misattributes.
//
// Governing: ADR-0015 (chatroom), SPEC-0009 REQ "Harness Identity Display",
// SPEC-0006 REQ "Run Correlation".
//
// @joestump-agent 09/11/2026 - Added for harness#302.

import (
	"fmt"
	"strings"
	"time"

	"github.com/stump-wtf/agent-trace/tail"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/runtrace"
)

// traceScopes converts the daemon's harness list into correlation scopes.
func traceScopes(infos []protocol.HarnessInfo) []runtrace.Scope {
	out := make([]runtrace.Scope, 0, len(infos))
	for _, h := range infos {
		if h.Workdir == "" {
			continue
		}
		s := runtrace.Scope{Name: h.Name, Adapter: h.Adapter, Workdir: h.Workdir}
		if w, ok := latestRun(h); ok {
			s.Runs = []runtrace.Window{w}
		}
		out = append(out, s)
	}
	return out
}

// latestRun is the window of h's latest run as the daemon reported it.
func latestRun(h protocol.HarnessInfo) (runtrace.Window, bool) {
	start, err := time.Parse(time.RFC3339Nano, h.LastStarted)
	if err != nil {
		return runtrace.Window{}, false
	}
	w := runtrace.Window{Start: start}
	if h.PID != 0 {
		return w, true
	}
	if end, err := time.Parse(time.RFC3339Nano, h.LastExitAt); err == nil && !end.Before(start) {
		w.End = end
		return w, true
	}
	// Not running, and no exit after the start: the daemon went down with the
	// run. Closing the window at its start vouches only for a session opened
	// in that same moment, rather than for everything since.
	w.End = start
	return w, true
}

// scopesKey fingerprints what attribution depends on, so the buffer is
// relabelled only when an answer could change — not on every state poll.
func scopesKey(scopes []runtrace.Scope) string {
	var b strings.Builder
	for _, s := range scopes {
		fmt.Fprintf(&b, "%s|%s|%s", s.Name, s.Adapter, s.Workdir)
		for _, r := range s.Runs {
			fmt.Fprintf(&b, "|%d-%d", r.Start.UnixNano(), r.End.UnixNano())
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// syncAttribution refreshes the scopes from m.harnesses and relabels the
// chatroom when they changed.
func (m *Model) syncAttribution() {
	m.traceScopes = traceScopes(m.harnesses)
	key := scopesKey(m.traceScopes)
	if key == m.traceKey {
		return
	}
	m.traceKey = key
	if m.chatroom != nil {
		m.chatroom.Reattribute()
	}
}

// attributeSession is the chatroom's attributor.
func (m *Model) attributeSession(meta tail.SessionMeta) string {
	name, _ := runtrace.Claimant(meta, m.traceScopes, time.Now())
	return name
}

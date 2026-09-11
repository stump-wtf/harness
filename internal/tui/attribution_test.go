package tui

import (
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/runtrace"
	"gitea.stump.rocks/stump.wtf/harness/internal/tui/chatroom"
	"gitea.stump.rocks/stump.wtf/harness/internal/tui/theme"
)

// tarsHarnesses is the 2026-09-11 tars shape: staggered sweeps sharing
// ~/sweeps, and two long-running crush agents sharing ~/src.
func tarsHarnesses() []protocol.HarnessInfo {
	return []protocol.HarnessInfo{
		{Name: "stumpcloud-sweep-pdx", Adapter: "crush", Workdir: "/h/sweeps", LastStarted: "2026-09-11T07:40:00.106-04:00", LastExitAt: "2026-09-11T07:56:21.558-04:00"},
		{Name: "pr-sweep", Adapter: "crush", Workdir: "/h/sweeps", LastStarted: "2026-09-11T09:30:00.017-04:00", LastExitAt: "2026-09-11T09:50:24.282-04:00"},
		{Name: "crush-signal", Adapter: "crush", Workdir: "/h/src", LastStarted: "2026-09-09T12:06:39.894-04:00", PID: 100},
		{Name: "crush-switchboard", Adapter: "crush", Workdir: "/h/src", LastStarted: "2026-09-09T12:06:39.895-04:00", PID: 101},
		{Name: "no-workdir", Adapter: "crush", LastStarted: "2026-09-11T07:40:00Z", PID: 102},
	}
}

func TestTraceScopesAttributeTarsSessions(t *testing.T) {
	scopes := traceScopes(tarsHarnesses())
	now := time.Date(2026, 9, 11, 18, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		meta tail.SessionMeta
		want string
	}{
		// pr-sweep's latest run began after this session did, and `list` says
		// nothing about its earlier runs — one of them may have covered 07:40.
		// Only the daemon's log-backed view (`harness logs`) can tell.
		{"pdx session, with a later-started sibling", tail.SessionMeta{Harness: tail.HarnessCrush, Cwd: "/h/sweeps", StartedAt: "2026-09-11T11:40:02Z"}, ""},
		{"pr-sweep session", tail.SessionMeta{Harness: tail.HarnessCrush, Cwd: "/h/sweeps", StartedAt: "2026-09-11T13:30:03Z"}, "pr-sweep"},
		{"between runs", tail.SessionMeta{Harness: tail.HarnessCrush, Cwd: "/h/sweeps", StartedAt: "2026-09-11T12:30:00Z"}, ""},
		{"shared by two running agents", tail.SessionMeta{Harness: tail.HarnessCrush, Cwd: "/h/src", StartedAt: "2026-09-11T15:41:00Z"}, ""},
		{"claude session in a sweep's window", tail.SessionMeta{Harness: tail.HarnessClaudeCode, Cwd: "/h/sweeps", StartedAt: "2026-09-11T11:41:00Z"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got, _ := runtrace.Claimant(tc.meta, scopes, now); got != tc.want {
				t.Errorf("Claimant = %q, want %q", got, tc.want)
			}
		})
	}
	for _, s := range scopes {
		if s.Name == "no-workdir" {
			t.Error("a harness with no workdir became a scope; it identifies nothing")
		}
	}
}

// TestTraceScopesExitWithoutStartIsUnknownHistory: tars' state file has sweeps
// with an exit and no start. Such a harness ran at some unknown time before its
// exit, so it may have written a session its sibling's run covers.
func TestTraceScopesExitWithoutStartIsUnknownHistory(t *testing.T) {
	scopes := traceScopes([]protocol.HarnessInfo{
		{Name: "pdx", Adapter: "crush", Workdir: "/h/sweeps", LastStarted: "2026-09-11T07:40:00-04:00", LastExitAt: "2026-09-11T07:56:21-04:00"},
		{Name: "brief", Adapter: "crush", Workdir: "/h/sweeps", LastExitAt: "2026-09-11T08:14:26-04:00"},
	})
	meta := tail.SessionMeta{Harness: tail.HarnessCrush, Cwd: "/h/sweeps", StartedAt: "2026-09-11T11:45:00Z"}
	now := time.Date(2026, 9, 11, 18, 0, 0, 0, time.UTC)
	if got, _ := runtrace.Claimant(meta, scopes, now); got != "" {
		t.Errorf("Claimant = %q, want none: brief exited at 08:14 with no recorded start", got)
	}
	if got, _ := runtrace.Claimant(meta, scopes[:1], now); got != "pdx" {
		t.Errorf("Claimant without brief = %q, want pdx", got)
	}
}

// TestLatestRunClosesAtStartWithoutExit: not running and no exit after the
// start means the daemon went down with the run; the window must not stay open
// and vouch for every session since.
func TestLatestRunClosesAtStartWithoutExit(t *testing.T) {
	w, ok := latestRun(protocol.HarnessInfo{LastStarted: "2026-09-11T07:40:00Z", LastExitAt: "2026-09-10T07:56:21Z"})
	if !ok || w.Open() || !w.End.Equal(w.Start) {
		t.Errorf("window = %+v, want closed at its start", w)
	}
	if _, ok := latestRun(protocol.HarnessInfo{}); ok {
		t.Error("a never-started harness produced a run window")
	}
}

// TestSyncAttributionRelabelsTheChatroom: events that arrive before the first
// harness list show under the tool; the list's arrival relabels them.
func TestSyncAttributionRelabelsTheChatroom(t *testing.T) {
	m := &Model{}
	m.chatroom = chatroom.New(theme.Default(), nil)
	m.chatroom.SetAttributor(m.attributeSession)
	m.chatroom.Add(tail.Event{
		Session:    tail.SessionMeta{Harness: tail.HarnessCrush, ID: "09a363b5", Key: "09a363b5", Cwd: "/h/sweeps", StartedAt: "2026-09-11T13:30:03Z"},
		Classified: classify.Event{Seq: 1, Timestamp: "2026-09-11T13:31:00Z", Tool: "view", Action: classify.ActionRead},
	})
	if got := m.chatroom.Buffer().Visible()[0].Identity.Username; got != "@crush" {
		t.Fatalf("before any harness list: %q, want @crush", got)
	}

	m.harnesses = tarsHarnesses()
	m.syncAttribution()
	if got := m.chatroom.Buffer().Visible()[0].Identity.Username; got != "@pr-sweep" {
		t.Errorf("after sync: %q, want @pr-sweep", got)
	}
	key := m.traceKey
	m.syncAttribution()
	if m.traceKey != key {
		t.Error("an unchanged harness list changed the fingerprint")
	}
}

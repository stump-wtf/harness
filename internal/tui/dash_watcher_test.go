// Dashboard live-activity watcher routing
//
// The dashboard and the chatroom each run their own tail.Watcher over the same
// transcripts. Bubble Tea read loops are self-re-arming — each delivered event
// must return the cmd that reads the next one — so the two loops have to stay
// distinguishable at the message level. While both emitted chatroom.MsgEvent,
// the first event delivered with the chatroom open re-armed only the chatroom's
// read and the dashboard's activity field went dead for the rest of the session.
//
// @joestump 08/21/2026 - Added alongside the dashEventMsg split in PR #248.

package tui

import (
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"

	"gitea.stump.rocks/stump.wtf/harness/internal/tui/chatroom"
)

func dashEvent(harness tail.Harness, tool string) tail.Event {
	return tail.Event{
		Session:    tail.SessionMeta{Harness: harness},
		Classified: classify.Event{Tool: tool, Action: classify.ActionEdit, Timestamp: "2026-08-21T11:00:00Z"},
	}
}

// A dashboard event must update the activity field and re-arm the dashboard's
// own read, whatever mode the TUI is in.
func TestDashEventTracksAndRearms(t *testing.T) {
	m := baseModel(120, 40)
	m.dashWatcher = tail.NewWatcherWithConfig(tail.DefaultWatchConfig(), tail.DefaultAdapters())
	t.Cleanup(m.stopDashWatcher)

	for _, mode := range []mode{modeDashboard, modeChatroom} {
		m.mode = mode
		m.lastActions = nil

		_, cmd := m.Update(dashEventMsg{Event: dashEvent(tail.HarnessCrush, "edit_file")})

		if got := m.lastActions[string(tail.HarnessCrush)]; got == "" {
			t.Errorf("mode %v: dash event did not reach the activity field", mode)
		}
		if cmd == nil {
			t.Errorf("mode %v: dash read loop was not re-armed; the activity field is dead from here on", mode)
		}
	}
}

// A chatroom event must never be mistaken for a dashboard event: with the
// chatroom closed it is inert, and it must not feed the activity field.
func TestChatroomEventDoesNotDriveDashboard(t *testing.T) {
	m := baseModel(120, 40)
	m.mode = modeDashboard
	m.lastActions = nil

	_, _ = m.Update(chatroom.MsgEvent{Event: dashEvent(tail.HarnessCrush, "edit_file")})

	if len(m.lastActions) != 0 {
		t.Fatalf("chatroom event leaked into the dashboard activity field: %v", m.lastActions)
	}
}

// startDashWatcher runs from onConnected, i.e. inside Update. tail.Watcher.Start
// is a blocking poll loop, so starting it inline froze the whole TUI the moment
// it reached the daemon.
func TestStartDashWatcherDoesNotBlock(t *testing.T) {
	m := baseModel(120, 40)
	t.Cleanup(m.stopDashWatcher)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = m.startDashWatcher()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("startDashWatcher did not return: watcher.Start is being called inline, which freezes the TUI on daemon connect")
	}
}

// stopDashWatcher must be idempotent — Close calls it after the program stops,
// and it may already have been torn down.
func TestStopDashWatcherIsIdempotent(t *testing.T) {
	m := baseModel(120, 40)
	m.dashWatcher = tail.NewWatcherWithConfig(tail.DefaultWatchConfig(), tail.DefaultAdapters())

	m.stopDashWatcher()
	if m.dashWatcher != nil || m.dashCancel != nil {
		t.Fatal("stopDashWatcher left state behind")
	}
	m.stopDashWatcher()
}

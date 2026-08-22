// Agent session watcher routing
//
// One watcher feeds two consumers. These tests pin the properties that make it
// survive its own first scan, which replays every transcript on the machine:
// events are delivered in batches, stale ones never arrive, and the read loop
// re-arms whatever mode the TUI is in.
//
// @joestump 08/21/2026 - Added alongside the dashEventMsg split in PR #248.
// @joestump-agent 08/21/2026 - Rewritten for the single watcher. The message
// split the earlier tests pinned existed to keep two watchers' read loops
// distinguishable; with one watcher there is one loop and nothing to confuse.

package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/tui/chatroom"
)

func watchEvent(harness tail.Harness, tool, ts string) tail.Event {
	return tail.Event{
		Session:    tail.SessionMeta{Harness: harness, Cwd: "/srv/app"},
		Classified: classify.Event{Tool: tool, Action: classify.ActionEdit, Timestamp: ts},
	}
}

func nowStamp(offset time.Duration) string {
	return time.Now().Add(offset).Format(time.RFC3339)
}

// A batch must reach the activity field and the chatroom buffer alike, and
// re-arm the read, whatever mode the TUI is in. A read that is not re-armed
// ends the stream silently for the rest of the session.
func TestAgentEventsFanOutAndRearm(t *testing.T) {
	for _, mode := range []mode{modeDashboard, modeChatroom} {
		m := baseModel(120, 40)
		m.watcher = tail.NewWatcherWithConfig(tail.DefaultWatchConfig(), tail.DefaultAdapters())
		t.Cleanup(m.stopWatcher)
		m.mode = mode
		m.chatroom = chatroom.New(m.theme, nil)
		m.chatroom.SetSize(120, 40)

		_, cmd := m.Update(agentEventMsg{Events: []tail.Event{
			watchEvent(tail.HarnessCrush, "edit_file", nowStamp(0)),
		}})

		if got := m.liveAction(protocol.HarnessInfo{Adapter: "crush", Workdir: "/srv/app"}); got == "" {
			t.Errorf("mode %v: event did not reach the activity field", mode)
		}
		if m.chatroom.Buffer().Len() != 1 {
			t.Errorf("mode %v: event did not reach the chatroom buffer", mode)
		}
		if cmd == nil {
			t.Errorf("mode %v: read loop was not re-armed; the stream is dead from here on", mode)
		}
	}
}

// The chatroom buffers whether or not it is on screen. Building it on entry
// meant opening the view showed an empty stream while a fresh scan of every
// transcript on the machine ran.
//
// The model is deliberately NOT pre-built here. It was, and that fixture was
// doing the work the assertion was meant to check: the chatroom was created
// only in enterChatroom, so on a real run every event before the first open
// went nowhere and the first open showed an empty stream — exactly the
// behaviour this test names.
func TestChatroomBuffersWhileClosed(t *testing.T) {
	m := baseModel(120, 40)
	m.mode = modeDashboard

	m.Update(agentEventMsg{Events: []tail.Event{
		watchEvent(tail.HarnessCrush, "edit_file", nowStamp(0)),
		watchEvent(tail.HarnessClaudeCode, "Bash", nowStamp(0)),
	}})

	if got := m.chatroom.Buffer().Len(); got != 2 {
		t.Fatalf("chatroom buffered %d events while closed, want 2", got)
	}
}

// Entering the chatroom must not rebuild it: the buffer is the session's, not
// the view's, and a rebuild throws away everything already collected.
func TestEnterChatroomKeepsTheBuffer(t *testing.T) {
	m := baseModel(120, 40)
	m.chatroom = chatroom.New(m.theme, nil)
	m.chatroom.SetSize(120, 40)
	m.Update(agentEventMsg{Events: []tail.Event{watchEvent(tail.HarnessCrush, "edit_file", nowStamp(0))}})

	m.enterChatroom()
	if got := m.chatroom.Buffer().Len(); got != 1 {
		t.Fatalf("entering the chatroom left %d buffered events, want 1", got)
	}

	m.exitChatroom()
	m.enterChatroom()
	if got := m.chatroom.Buffer().Len(); got != 1 {
		t.Fatalf("a second visit left %d buffered events, want 1", got)
	}
}

// The watcher's first scan emits every event in every transcript it can find —
// 76k of them against a two-year-old Claude Code store, with no way to ask for
// less. Anything older than the window is dropped before it can reach a buffer.
func TestHistoryWindowDropsBackfill(t *testing.T) {
	floor := time.Now().Add(-historyWindow)

	stale := watchEvent(tail.HarnessCrush, "edit_file", nowStamp(-24*time.Hour))
	if withinWindow(stale, floor) {
		t.Error("a day-old event passed the history window")
	}
	fresh := watchEvent(tail.HarnessCrush, "edit_file", nowStamp(-time.Minute))
	if !withinWindow(fresh, floor) {
		t.Error("a one-minute-old event was dropped by the history window")
	}
	// An adapter that does not date its events is a reason to show them, not to
	// hide them; the batching is what makes the volume survivable either way.
	undated := watchEvent(tail.HarnessCrush, "edit_file", "")
	if !withinWindow(undated, floor) {
		t.Error("an undated event was dropped; its adapter simply omits timestamps")
	}
}

// readEvents must batch. Bubble Tea renders after every message, so delivering
// a burst one event at a time costs one full frame render per event — which is
// how a backfill locks the UI out of answering keys for the length of the scan.
func TestReadEventsBatchesABurst(t *testing.T) {
	// Fill a channel the way a first scan does.
	const burst = 64
	ch := make(chan tail.Event, burst)
	for i := 0; i < burst; i++ {
		ch <- watchEvent(tail.HarnessCrush, "edit_file", nowStamp(0))
	}

	batch, ok := readBatch(ch, time.Now().Add(-historyWindow)).(agentEventMsg)
	if !ok {
		t.Fatal("readBatch did not return an agentEventMsg")
	}
	if len(batch.Events) != burst {
		t.Fatalf("batch carried %d of %d queued events; a burst costs one frame per event at this rate",
			len(batch.Events), burst)
	}
}

// The window is applied inside the drain, so a backfill of stale events costs
// one message for the whole burst rather than one per event.
func TestReadBatchDropsStaleInsideTheDrain(t *testing.T) {
	ch := make(chan tail.Event, 3)
	ch <- watchEvent(tail.HarnessCrush, "old", nowStamp(-24*time.Hour))
	ch <- watchEvent(tail.HarnessCrush, "new", nowStamp(0))
	ch <- watchEvent(tail.HarnessCrush, "old", nowStamp(-24*time.Hour))

	batch, ok := readBatch(ch, time.Now().Add(-historyWindow)).(agentEventMsg)
	if !ok {
		t.Fatal("readBatch did not return an agentEventMsg")
	}
	if len(batch.Events) != 1 || batch.Events[0].Classified.Tool != "new" {
		t.Fatalf("batch = %+v, want only the fresh event", batch.Events)
	}
}

// An all-stale drain must keep blocking rather than deliver an empty batch.
// Every delivered message costs a full frame render, and during the first scan
// almost every batch is entirely stale: on a real store, returning early meant
// 644 wake-ups to deliver 66 live events. Waiting made it 2.
func TestReadBatchWaitsThroughAnAllStaleDrain(t *testing.T) {
	ch := make(chan tail.Event, 4)
	for i := 0; i < 3; i++ {
		ch <- watchEvent(tail.HarnessCrush, "old", nowStamp(-24*time.Hour))
	}
	ch <- watchEvent(tail.HarnessCrush, "live", nowStamp(0))
	close(ch)

	batch, ok := readBatch(ch, time.Now().Add(-historyWindow)).(agentEventMsg)
	if !ok {
		t.Fatal("readBatch did not return an agentEventMsg")
	}
	if len(batch.Events) != 1 || batch.Events[0].Classified.Tool != "live" {
		t.Fatalf("batch = %+v, want the one live event and no empty delivery", batch.Events)
	}
}

// A closed channel with nothing live left returns nil, which Bubble Tea
// discards: the watcher is gone and there is nothing to re-arm.
func TestReadBatchOnClosedChannel(t *testing.T) {
	ch := make(chan tail.Event)
	close(ch)
	if msg := readBatch(ch, time.Time{}); msg != nil {
		t.Fatalf("readBatch on a closed channel = %v, want nil", msg)
	}
}

// A batch of stale events must still re-arm the read. Dropping every event in a
// batch is the normal case during the first scan, and a nil cmd there ends the
// stream before the live events arrive.
func TestStaleBatchStillRearms(t *testing.T) {
	m := baseModel(120, 40)
	m.watcher = tail.NewWatcherWithConfig(tail.DefaultWatchConfig(), tail.DefaultAdapters())
	t.Cleanup(m.stopWatcher)

	var cmd tea.Cmd
	_, cmd = m.Update(agentEventMsg{Events: nil})
	if cmd == nil {
		t.Fatal("an empty batch did not re-arm the read; the stream dies during the backfill")
	}
}

// startWatcher runs from onConnected, i.e. inside Update. tail.Watcher.Start is
// a blocking poll loop, so starting it inline froze the whole TUI the moment it
// reached the daemon — no repaint, no keys, no quit.
func TestStartWatcherDoesNotBlock(t *testing.T) {
	m := baseModel(120, 40)
	t.Cleanup(m.stopWatcher)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = m.startWatcher()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("startWatcher did not return: watcher.Start is being called inline, which freezes the TUI on daemon connect")
	}
}

// stopWatcher must be idempotent — Close calls it after the program stops, and
// it may already have been torn down.
func TestStopWatcherIsIdempotent(t *testing.T) {
	m := baseModel(120, 40)
	m.watcher = tail.NewWatcherWithConfig(tail.DefaultWatchConfig(), tail.DefaultAdapters())

	m.stopWatcher()
	if m.watcher != nil || m.watcherCancel != nil {
		t.Fatal("stopWatcher left state behind")
	}
	m.stopWatcher()
}

// A batch arriving before the view has ever been opened must build the buffer,
// not be dropped on the floor. This is the first-open case: the dashboard can
// run for an hour before anyone presses the key, and what it collected in that
// hour is the entire value of opening it.
func TestFirstBatchBeforeAnyEntryIsKept(t *testing.T) {
	m := baseModel(120, 40)
	m.mode = modeDashboard
	if m.chatroom != nil {
		t.Fatal("fixture pre-built the chatroom; this test is about the case where nothing has")
	}

	m.Update(agentEventMsg{Events: []tail.Event{
		watchEvent(tail.HarnessCrush, "edit_file", nowStamp(0)),
		watchEvent(tail.HarnessClaudeCode, "Bash", nowStamp(0)),
	}})

	m.enterChatroom()
	if got := m.chatroom.Buffer().Len(); got != 2 {
		t.Fatalf("opening the chatroom for the first time showed %d events, want 2", got)
	}
}

// An empty batch must not build a buffer or cost an anchor recomputation, but
// must still re-arm the read.
func TestEmptyBatchIsInert(t *testing.T) {
	m := baseModel(120, 40)
	m.watcher = tail.NewWatcherWithConfig(tail.DefaultWatchConfig(), tail.DefaultAdapters())
	t.Cleanup(m.stopWatcher)

	_, cmd := m.Update(agentEventMsg{})
	if m.chatroom != nil {
		t.Error("an empty batch built a chatroom buffer")
	}
	if cmd == nil {
		t.Error("an empty batch did not re-arm the read; the stream is dead from here on")
	}
}

// The terminal answers tea.RequestBackgroundColor after the model is built, and
// the buffer now fills from daemon connect — so events can already be buffered,
// in the dark default palette, when the answer lands. The rendered lines are
// cached, so the theme has to reach the buffer and not only the style set.
func TestBackgroundColorRestylesBufferedChatroom(t *testing.T) {
	m := baseModel(120, 40)
	m.Update(agentEventMsg{Events: []tail.Event{watchEvent(tail.HarnessCrush, "edit_file", nowStamp(0))}})
	if m.chatroom == nil {
		t.Fatal("the batch did not build a chatroom buffer")
	}
	m.chatroom.SetSize(120, 40)
	// The event row, not the whole frame: the status bar restyles from the
	// style set alone, so comparing frames passes even when every buffered
	// line is still cached in the old palette.
	dark := strings.SplitN(m.chatroom.View(), "\n", 2)[0]

	m.Update(tea.BackgroundColorMsg{Color: lipgloss.Color("#ffffff")})

	if got := strings.SplitN(m.chatroom.View(), "\n", 2)[0]; got == dark {
		t.Errorf("the buffered event kept its dark-default styling after the terminal reported a light background: %q", got)
	}
	if got := m.chatroom.Buffer().Len(); got != 1 {
		t.Fatalf("re-styling cost %d buffered events, want 1 kept", 1-got)
	}
}

package tui

// Agent Session Watcher
//
// One tail.Watcher for the whole TUI, feeding both consumers of agent activity:
// the dashboard's live action field and the chatroom stream (ADR-0015). It
// starts on daemon connect and runs until the model closes.
//
// Everything here exists because of one property of the watcher: its first scan
// emits the ENTIRE history of every agent session it can discover, and it has
// no way to be asked for less. On a developer laptop with a couple of years of
// transcripts that is tens of thousands of events delivered in under a minute —
// measured at 76,309 over 43s against a 1.2GB Claude Code store — and Bubble
// Tea calls View() after every message it processes. One event per message
// therefore means tens of thousands of full frame renders, during which
// keystrokes queue behind the flood and the UI is not merely slow but
// unresponsive.
//
// Two defences, in the order they apply. Events older than historyWindow are
// dropped on arrival, so the backfill mostly evaporates before it reaches a
// buffer. What survives is delivered in batches, so a burst costs a frame
// rather than a frame each.
//
// Governing: ADR-0015 (chatroom TUI), SPEC-0015 REQ "Live Event Stream"
//
// @joestump-agent 08/21/2026 - Replaces the two independent watchers PR #248
// created (one for the dashboard field, one built per chatroom entry), each
// re-scanning every transcript on the machine.

import (
	"context"
	"log/slog"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/stump-wtf/agent-trace/tail"

	"gitea.stump.rocks/stump.wtf/harness/internal/tui/chatroom"
)

const (
	// historyWindow is how far back an event may be timestamped and still reach
	// the TUI. The chatroom answers "what are my agents doing", which is a
	// question about the recent past; the watcher's first scan answers "what
	// has every agent on this machine ever done", which is not the same
	// question and arrives in five figures.
	//
	// Events whose timestamp will not parse are kept. An adapter that omits
	// timestamps is a reason to see its events, not to silently hide them, and
	// the batching below survives the volume either way.
	historyWindow = 15 * time.Minute

	// discoveryWindow bounds how far back the watcher looks for session files
	// at all, as opposed to historyWindow above, which bounds which of their
	// events get shown. Discovery used to be unbounded: every session file
	// that had ever existed was re-read on every 2s tick, which on a real
	// corpus meant hundreds of frozen transcripts rediscovered forever and
	// sustained CPU burn with nothing to show for it.
	//
	// Pinned here rather than inherited from tail.DefaultWatchConfig() so the
	// value harness runs with is visible next to the floor it has to respect,
	// and so an upstream change to that default cannot silently change what
	// the dashboard discovers.
	//
	// The invariant that matters is discoveryWindow >= historyWindow: an event
	// older than the floor is dropped before it is rendered, so a discovery
	// window at least as wide as the floor cannot hide anything the UI would
	// have displayed. At 48h against 15m there is three orders of magnitude of
	// headroom; TestDiscoveryWindowCoversHistoryWindow keeps it that way.
	//
	// @joestump-agent 08/23/2026 - Added with the agent-trace bump that
	// introduced WatchConfig.MaxAge.
	discoveryWindow = tail.DefaultMaxAge

	// eventBatchMax caps one delivered batch. The drain is bounded so a
	// watcher producing faster than the UI consumes cannot hold the Update loop
	// indefinitely inside a single message — it just yields and is re-armed.
	eventBatchMax = 512
)

// agentEventMsg carries a batch of events from the single watcher.
//
// A batch, not an event: Bubble Tea renders after every message, so delivering
// the backfill one event at a time bought one full frame render per historical
// tool call.
type agentEventMsg struct{ Events []tail.Event }

// startWatcher starts the agent session watcher and arms the first read. Called
// once the daemon connection is established.
func (m *Model) startWatcher() tea.Cmd {
	if m.watcher != nil {
		return nil
	}
	cfg := tail.DefaultWatchConfig()
	cfg.MaxAge = discoveryWindow
	m.watcher = tail.NewWatcherWithConfig(cfg, tail.DefaultAdapters())
	m.watcherFloor = time.Now().Add(-historyWindow)
	ctx, cancel := context.WithCancel(context.Background())
	m.watcherCancel = cancel
	// Start is a blocking poll loop; inline it and Update never returns.
	go m.watcher.Start(ctx)
	return m.readEvents()
}

// stopWatcher tears the watcher down.
func (m *Model) stopWatcher() {
	if m.watcherCancel != nil {
		m.watcherCancel()
		m.watcherCancel = nil
	}
	if m.watcher != nil {
		m.watcher.Stop()
		m.watcher = nil
	}
}

// readEvents returns the tea.Cmd that reads the next batch of events.
//
// It blocks for the first event, then takes whatever else is already queued
// without waiting. That is what makes it a batch under load and a plain read
// when idle: a live stream at human pace delivers one event per message, and a
// backfill delivers eventBatchMax.
//
// The watcher is captured rather than read from m at call time, so a teardown
// that clears m.watcher cannot leave an in-flight read holding a nil.
func (m *Model) readEvents() tea.Cmd {
	if m.watcher == nil {
		return nil
	}
	ch, floor := m.watcher.Events(), m.watcherFloor
	return func() tea.Msg { return readBatch(ch, floor) }
}

// readBatch blocks for one event, then takes whatever else is already queued
// without waiting, dropping anything older than floor as it goes.
//
// Split from readEvents so it can be exercised against a channel a test owns:
// tail.Watcher hands out a receive-only channel and fills it from a real scan,
// which is not a burst anyone can arrange.
//
// A closed channel returns nil, which Bubble Tea discards — the watcher is gone
// and there is nothing left to re-arm.
func readBatch(ch <-chan tail.Event, floor time.Time) tea.Msg {
	batch := make([]tail.Event, 0, 16)
	for {
		ev, ok := <-ch
		if !ok {
			if len(batch) > 0 {
				return agentEventMsg{Events: batch}
			}
			return nil
		}
		if withinWindow(ev, floor) {
			batch = append(batch, ev)
		}
		drained := false
		for !drained && len(batch) < eventBatchMax {
			select {
			case ev, ok := <-ch:
				if !ok {
					return agentEventMsg{Events: batch}
				}
				if withinWindow(ev, floor) {
					batch = append(batch, ev)
				}
			default:
				drained = true
			}
		}
		if len(batch) > 0 {
			return agentEventMsg{Events: batch}
		}
		// Everything queued was older than the window. Keep blocking rather
		// than delivering an empty batch: a message costs a full frame render,
		// and during the first scan almost every batch is entirely stale. On a
		// real store this was 644 wake-ups to deliver 66 events.
	}
}

// withinWindow reports whether ev is recent enough to show. An event with no
// parseable timestamp passes: the alternative is hiding a live event because
// its adapter did not date it.
func withinWindow(ev tail.Event, floor time.Time) bool {
	if floor.IsZero() {
		return true
	}
	t, err := time.Parse(time.RFC3339, ev.Classified.Timestamp)
	if err != nil {
		return true
	}
	return !t.Before(floor)
}

// onAgentEvents feeds one batch to both consumers and re-arms the read.
func (m *Model) onAgentEvents(msg agentEventMsg) (tea.Model, tea.Cmd) {
	if len(msg.Events) == 0 {
		return m, m.readEvents()
	}
	// Built here, not on entry to the view. The buffer is the session's and the
	// view is a window onto it, so it has to exist from the first event the
	// watcher delivers — otherwise the first open shows an empty stream no
	// matter how long the dashboard has been collecting.
	m.ensureChatroom()
	for _, ev := range msg.Events {
		m.trackLastAction(ev)
		m.chatroom.Add(ev)
	}
	// Once per batch, not once per event: the anchor is a property of the
	// buffer's final state, not of each arrival.
	m.chatroom.Settle()
	return m, m.readEvents()
}

// ensureChatroom builds the chatroom model if it does not exist yet. Cheap and
// idempotent: New allocates a buffer and a style set and starts nothing.
func (m *Model) ensureChatroom() {
	if m.chatroom == nil {
		m.chatroom = chatroom.New(m.theme, slog.Default())
	}
}

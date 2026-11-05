package chatroom

// Chatroom History
//
// The watcher that feeds the chatroom only shows what happened in the last
// fifteen minutes (internal/tui/watcher.go, historyWindow), and it can only see
// the session stores of the machine the TUI runs on, in their default places.
// A sweep that finished an hour ago, a crush harness keeping its sessions under
// its own data directory, and every harness on a remote daemon were all
// invisible — the chatroom opened empty on a fleet that had done a day's work.
//
// So on open the chatroom also asks the daemon for each harness's latest run,
// the same structured `logs` reply `harness logs` prints, and merges it into
// the stream by time. The daemon attributes those sessions itself, with each
// harness's real store and environment, so they arrive already knowing whose
// they are: a session named by history keeps that harness as its speaker, even
// for the live events that follow it.
//
// History and the watcher overlap for anything recent, so every row carries a
// key built the way runtrace builds an entry's ID — session, then seq for a
// tool call — and a row whose key is already buffered is dropped.
//
// Governing: ADR-0015 (chatroom TUI), SPEC-0009 REQ "Multi-Harness Event
// Aggregation", SPEC-0006 REQ "Run Correlation".
//
// @joestump-agent 09/27/2026 - Added: the chatroom opened empty unless an
// agent had acted in the last fifteen minutes.

import (
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"

	"github.com/stump-wtf/harness/internal/protocol"
)

// AddHistory merges one harness's structured `logs` reply into the stream,
// attributed to that harness, skipping every row already buffered. A reply
// that is not the activity view (a daemon too old to build one, or an adapter
// with no transcript) carries no events and adds nothing.
//
// Lifecycle and session entries are the run's, not the agent's, and the
// chatroom is the agents talking; only tool calls and marks are merged.
func (m *Model) AddHistory(harness, adapter string, ld protocol.LogsData) {
	if ld.Source != protocol.LogSourceAgentTrace {
		return
	}
	if adapter == "" {
		adapter = "crush" // the config default (ADR-0011)
	}
	var evs []tail.Event
	learned := false
	for _, e := range ld.Entries {
		if e.Ambiguous || e.Session == "" {
			continue // never attributed to this harness (SPEC-0006)
		}
		ev, ok := historyEvent(e, tail.Harness(adapter))
		if !ok {
			continue
		}
		if m.owners == nil {
			m.owners = map[string]string{}
		}
		if m.owners[e.Session] != harness {
			m.owners[e.Session] = harness
			learned = true
		}
		evs = append(evs, ev)
	}
	if learned {
		// Owners learned here relabel what the watcher already buffered from
		// the same sessions. Only when one is new: a relabel re-renders the
		// whole buffer, and the same reply arrives on every poll.
		m.Reattribute()
	}
	if len(evs) == 0 {
		return
	}
	// In time order, so the buffer's insert walks back as little as it can.
	sort.SliceStable(evs, func(i, j int) bool {
		return evs[i].Classified.Timestamp < evs[j].Classified.Timestamp
	})
	added := false
	for _, ev := range evs {
		added = m.Add(ev) || added
	}
	if added {
		m.Settle()
	}
}

// historyEvent rebuilds a tail.Event from one activity entry: a tool call as
// the classified call, a mark as a mark riding an otherwise empty event.
func historyEvent(e protocol.LogEntry, h tail.Harness) (tail.Event, bool) {
	t, err := time.Parse(time.RFC3339Nano, e.Time)
	if err != nil {
		return tail.Event{}, false
	}
	// One spelling for every history timestamp, so the buffer's string
	// ordering holds among them whatever zone the daemon wrote.
	ts := t.UTC().Format(time.RFC3339Nano)
	ev := tail.Event{
		Session:    tail.SessionMeta{ID: e.Session, Harness: h},
		Classified: classify.Event{Timestamp: ts},
		ReceivedAt: t,
	}
	switch e.Kind {
	case protocol.LogEntryTool:
		seq, err := strconv.Atoi(strings.TrimPrefix(e.ID, e.Session+"/tool/"))
		if err != nil {
			return tail.Event{}, false
		}
		ev.Classified.Seq = seq
		ev.Classified.Tool = e.Tool
		if ev.Classified.Tool == "" {
			ev.Classified.Tool = e.Action
		}
		ev.Classified.Action = e.Action
		ev.Classified.Summary = e.Summary
		ev.Classified.IsError = e.Error
		ev.Classified.ErrorExcerpt = e.ErrorExcerpt
		if e.Target != "" {
			touch := classify.TouchHit
			switch e.Action {
			case "read":
				touch = classify.TouchRead
			case "edit":
				touch = classify.TouchEdit
			}
			ev.Classified.Targets = []classify.Target{{Path: e.Target, Touch: touch}}
		}
	case protocol.LogEntryMark:
		ev.Marks = []classify.Mark{{Timestamp: ts, Type: e.Action, Note: e.Summary}}
	default:
		return tail.Event{}, false
	}
	return ev, true
}

// rowKeys names each row an event renders, the way runtrace names the entry
// it becomes: a tool call by its session and seq, a mark by its session, type
// and instant (runtrace numbers marks by position in the whole transcript,
// which a watcher reading only what was appended cannot know). The first key
// is the tool call's, "" when the event has none.
func rowKeys(ev tail.Event) (tool string, marks []string) {
	id := ev.Session.ID
	if id == "" {
		return "", make([]string, len(ev.Marks)) // nothing to match on
	}
	if ev.Classified.Tool != "" {
		tool = id + "/tool/" + strconv.Itoa(ev.Classified.Seq)
	}
	for _, mk := range ev.Marks {
		at := mk.Timestamp
		if t, err := time.Parse(time.RFC3339Nano, at); err == nil {
			at = strconv.FormatInt(t.UnixNano(), 10)
		}
		marks = append(marks, id+"/mark/"+mk.Type+"/"+at)
	}
	return tool, marks
}

// dedupe strips from ev every row already buffered and records the rest,
// reporting false when nothing is left.
func (m *Model) dedupe(ev *tail.Event) bool {
	if m.seen == nil {
		m.seen = map[string]struct{}{}
	}
	tool, marks := rowKeys(*ev)
	kept := ev.Marks[:0:0]
	for i, k := range marks {
		if k != "" {
			if _, dup := m.seen[k]; dup {
				continue
			}
			m.seen[k] = struct{}{}
		}
		kept = append(kept, ev.Marks[i])
	}
	ev.Marks = kept
	if tool != "" {
		if _, dup := m.seen[tool]; dup {
			ev.Classified.Tool = ""
		} else {
			m.seen[tool] = struct{}{}
		}
	}
	if len(m.seen) > 4*m.buffer.maxSize {
		m.reseed()
	}
	return ev.Classified.Tool != "" || len(ev.Marks) > 0
}

// reseed rebuilds the seen set from what the buffer still holds, so the set
// stays bounded by the buffer rather than by the session's length. A key for a
// row already evicted has nothing left to duplicate.
func (m *Model) reseed() {
	m.seen = make(map[string]struct{}, len(m.buffer.events))
	for i := range m.buffer.events {
		tool, marks := rowKeys(m.buffer.events[i].Event)
		if tool != "" {
			m.seen[tool] = struct{}{}
		}
		for _, k := range marks {
			if k != "" {
				m.seen[k] = struct{}{}
			}
		}
	}
}

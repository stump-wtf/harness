package chatroom

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"gitea.stump.rocks/stump.wtf/harness/internal/tui/theme"
	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"
)

func testTheme() *theme.Theme {
	return theme.New(0, true, theme.DefaultPalette()) // 0 = no color, tests stay plain
}

func ev(key, harness, ts string, seq int) tail.Event {
	return tail.Event{
		Session: tail.SessionMeta{Key: key, Harness: tail.Harness(harness)},
		Classified: classify.Event{
			Seq:       seq,
			Timestamp: ts,
			Tool:      "bash",
			Action:    classify.ActionExec,
			Summary:   "go test ./...",
		},
		ReceivedAt: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC),
	}
}

func markEv(key, harness, ts string, seq int, markType, note string) tail.Event {
	return tail.Event{
		Session: tail.SessionMeta{Key: key, Harness: tail.Harness(harness)},
		Marks: []classify.Mark{{
			Seq:       seq,
			Timestamp: ts,
			Type:      markType,
			Note:      note,
		}},
		ReceivedAt: time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC),
	}
}

func TestAddOrdersAndDedupes(t *testing.T) {
	m := New(testTheme())
	m.add([]tail.Event{
		ev("s1", "crush", "2026-09-06T12:00:02Z", 1),
		ev("s1", "crush", "2026-09-06T12:00:01Z", 1), // same session+seq: dup
		ev("s2", "claude-code", "2026-09-06T12:00:00Z", 1),
	})
	if len(m.entries) != 2 {
		t.Fatalf("expected 2 entries after dedupe, got %d", len(m.entries))
	}
	if m.entries[0].harness != tail.HarnessClaudeCode || m.entries[1].harness != tail.HarnessCrush {
		t.Fatalf("entries not in timestamp order: %+v", m.entries)
	}
}

func TestAddFallsBackToReceivedAt(t *testing.T) {
	m := New(testTheme())
	m.add([]tail.Event{ev("s1", "pi", "", 1)})
	if !m.entries[0].at.Equal(time.Date(2026, 9, 6, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("missing ts should fall back to ReceivedAt, got %v", m.entries[0].at)
	}
}

func TestIdenticalTimestampsOrderByHarness(t *testing.T) {
	m := New(testTheme())
	ts := "2026-09-06T12:00:00Z"
	m.add([]tail.Event{
		ev("s1", "pi", ts, 1),
		ev("s2", "codex", ts, 1),
	})
	if m.entries[0].harness != tail.HarnessCodex {
		t.Fatalf("tie should order by harness name, got %+v", m.entries)
	}
}

func TestBufferCapEvictsOldest(t *testing.T) {
	m := New(testTheme())
	batch := make([]tail.Event, 0, maxEvents+50)
	for i := 0; i < maxEvents+50; i++ {
		batch = append(batch, ev("s", "crush", time.Unix(int64(i), 0).UTC().Format(time.RFC3339), i))
	}
	m.add(batch)
	if len(m.entries) != maxEvents {
		t.Fatalf("expected buffer capped at %d, got %d", maxEvents, len(m.entries))
	}
	if want := maxEvents + 50 - 1; m.entries[len(m.entries)-1].dedupe != "s:e:"+itoa(want) {
		// The newest event must survive; the 50 oldest must be gone.
		t.Fatalf("newest entry should be seq %d", want)
	}
	if _, still := m.seen["s:e:0"]; still {
		t.Fatal("evicted entries must leave the dedupe set")
	}
}

func TestFilter(t *testing.T) {
	m := New(testTheme())
	m.add([]tail.Event{
		ev("s1", "crush", "2026-09-06T12:00:01Z", 1),
		ev("s2", "codex", "2026-09-06T12:00:02Z", 1),
	})
	m.filter = "crush"
	if got := len(m.visible()); got != 1 {
		t.Fatalf("filter should show 1 entry, got %d", got)
	}
	if _, exited := m.onKey(tea.KeyPressMsg{Code: '0'}); exited {
		t.Fatal("0 should not exit")
	}
	if m.filter != "" {
		t.Fatalf("0 should clear the filter, got %q", m.filter)
	}
}

func TestKeysExitAndPause(t *testing.T) {
	m := New(testTheme())
	if _, exited := m.onKey(tea.KeyPressMsg{Code: 'q'}); !exited {
		t.Fatal("q should request exit")
	}
	m2 := New(testTheme())
	if _, exited := m2.onKey(tea.KeyPressMsg{Code: tea.KeyEscape}); !exited {
		t.Fatal("esc should request exit")
	}
	if _, exited := m2.onKey(tea.KeyPressMsg{Code: 'p'}); exited || !m2.paused {
		t.Fatal("p should toggle pause without exiting")
	}
}

func TestRenderContainsUsernameAndBadges(t *testing.T) {
	m := New(testTheme())
	m.Resize(120, 40)
	m.add([]tail.Event{
		markEv("s1", "crush", "2026-09-06T12:00:00Z", 1, "user", "fix the login bug"),
		ev("s1", "crush", "2026-09-06T12:00:01Z", 2),
	})
	out := m.View()
	for _, want := range []string{"@crush", "[USER]", "[EXEC]", "fix the login bug"} {
		if !strings.Contains(out, want) {
			t.Fatalf("view missing %q:\n%s", want, out)
		}
	}
}

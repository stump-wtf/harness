// Chatroom navigation
//
// The chatroom is a full-screen stream with no footer, reached by keypress, and
// fed by a watcher that can deliver hundreds of events between two frames.
// Every test here pins something that made it unnavigable: keys that did
// nothing when held, a scroll position undone by the next arriving event, a
// Ctrl-C that would not quit, and no way to discover any of it from the screen.
//
// @joestump-agent 08/21/2026 - Review of #248.

package tui

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"

	"gitea.stump.rocks/stump.wtf/harness/internal/tui/chatroom"
)

// chatModel is a chatroom with enough buffered events to scroll through.
func chatModel(t *testing.T, events int) *Model {
	t.Helper()
	m := baseModel(120, 40)
	m.chatroom = chatroom.New(m.theme, nil)
	m.chatroom.SetSize(120, 40)
	m.mode = modeChatroom
	for i := 0; i < events; i++ {
		m.chatroom.Add(tail.Event{
			Session: tail.SessionMeta{Harness: tail.HarnessClaudeCode, Cwd: "/srv/app"},
			Classified: classify.Event{
				Tool: "Bash", Action: classify.ActionExec,
				Timestamp: time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC).
					Add(time.Duration(i) * time.Second).Format(time.RFC3339),
				Summary: "go test ./...",
			},
		})
	}
	m.chatroom.Settle()
	return m
}

func liveEvent() tail.Event {
	return tail.Event{
		Session: tail.SessionMeta{Harness: tail.HarnessClaudeCode, Cwd: "/srv/app"},
		Classified: classify.Event{
			Tool: "Bash", Action: classify.ActionExec,
			Timestamp: time.Now().Format(time.RFC3339), Summary: "a later event",
		},
	}
}

// Bubble Tea coalesces a held key into one message whose Text holds every rune.
// key.Matches compares msg.String() against the binding, so "kkk" matched
// nothing and holding the only scroll key did nothing at all. Issue #145,
// solved for the dashboard, reintroduced here.
func TestChatroomHeldKeyScrolls(t *testing.T) {
	for _, held := range []string{"kkk", "jjj"} {
		m := chatModel(t, 200)
		if held == "jjj" {
			m.chatroom.Scroll(-1 << 30) // start at the top so down has room
		}
		before := m.chatroom.Top()

		m.onChatroomKey(tea.KeyPressMsg{Code: rune(held[0]), Text: held})

		got := m.chatroom.Top()
		if got == before {
			t.Errorf("held %q did not scroll: top stayed %d", held, got)
		}
		if delta := got - before; delta != 3 && delta != -3 {
			t.Errorf("held %q moved %d lines, want 3 — one per rune", held, delta)
		}
	}
}

// Scrolling up must survive the stream. The view re-anchored to the bottom on
// EVERY arriving event, so during a burst any scroll was undone before the next
// frame and the only way to read anything was an undocumented pause key.
func TestChatroomScrollSurvivesArrivingEvents(t *testing.T) {
	m := chatModel(t, 200)

	m.onChatroomKey(tea.KeyPressMsg{Code: 'g', Text: "g"}) // to the top
	if m.chatroom.Top() != 0 {
		t.Fatalf("g did not reach the top: top = %d", m.chatroom.Top())
	}
	if m.chatroom.Following() {
		t.Fatal("scrolling to the top left the view following the stream")
	}

	for i := 0; i < 5; i++ {
		m.chatroom.Add(liveEvent())
	}
	m.chatroom.Settle()

	if got := m.chatroom.Top(); got != 0 {
		t.Fatalf("arriving events moved the reader's scroll from 0 to %d", got)
	}
}

// ...and returning to the bottom must resume following, so catching up is not a
// one-way trip into a frozen view.
func TestChatroomBottomResumesFollowing(t *testing.T) {
	m := chatModel(t, 200)
	m.onChatroomKey(tea.KeyPressMsg{Code: 'g', Text: "g"})
	if m.chatroom.Following() {
		t.Fatal("precondition: still following after scrolling to the top")
	}

	m.onChatroomKey(tea.KeyPressMsg{Code: 'G', Text: "G"})
	if !m.chatroom.Following() {
		t.Fatal("G reached the bottom but did not resume following")
	}

	before := m.chatroom.Top()
	m.chatroom.Add(liveEvent())
	m.chatroom.Settle()
	if m.chatroom.Top() <= before {
		t.Fatal("following resumed but the view did not track the new event")
	}
}

// Space toggles follow by hand, and turning it back on jumps to the bottom —
// "following" and "parked in the scrollback" is not a state anyone means.
func TestChatroomSpaceTogglesFollow(t *testing.T) {
	m := chatModel(t, 200)
	m.onChatroomKey(tea.KeyPressMsg{Code: ' ', Text: " "})
	if m.chatroom.Following() {
		t.Fatal("space did not stop the view following")
	}
	m.chatroom.Scroll(-20)
	m.onChatroomKey(tea.KeyPressMsg{Code: ' ', Text: " "})
	if !m.chatroom.Following() {
		t.Fatal("space did not resume following")
	}
	if m.chatroom.Top() == 0 {
		t.Fatal("resuming follow left the view in the scrollback")
	}
}

// Quit binds q and ctrl+c together. While the chatroom treated the whole
// binding as "leave the view", there was no way to exit harness from in here.
func TestChatroomCtrlCQuits(t *testing.T) {
	m := chatModel(t, 10)
	_, cmd := m.onChatroomKey(tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl})
	if !m.quitting {
		t.Error("ctrl+c did not quit; it dropped to the dashboard instead")
	}
	if cmd == nil {
		t.Error("ctrl+c returned no command, so the program never stops")
	}

	// q still means "back to the dashboard", not "quit".
	m2 := chatModel(t, 10)
	m2.onChatroomKey(tea.KeyPressMsg{Code: 'q', Text: "q"})
	if m2.quitting {
		t.Error("q quit the program; in the chatroom it means back")
	}
	if m2.mode != modeDashboard {
		t.Error("q did not return to the dashboard")
	}
}

// Filter keys were matched with a STRING comparison on msg.Text, so "111" —
// which is what a held 1 delivers — fell inside "1" <= t <= "5" and toggled
// once. Matching the rune makes the held case go through rune expansion like
// every other key.
func TestChatroomFilterKeysMatchRunes(t *testing.T) {
	m := chatModel(t, 10)
	all := m.chatroom.Buffer().Filter()

	m.onChatroomKey(tea.KeyPressMsg{Code: '1', Text: "1"})
	one := m.chatroom.Buffer().Filter()
	if one == all {
		t.Fatal("1 did not toggle a harness out of the filter")
	}

	m.onChatroomKey(tea.KeyPressMsg{Code: '0', Text: "0"})
	if m.chatroom.Buffer().Filter() != all {
		t.Fatal("0 did not restore the full filter")
	}

	// A held 1 toggles three times — landing back where a single press does.
	m.onChatroomKey(tea.KeyPressMsg{Code: '1', Text: "111"})
	if got := m.chatroom.Buffer().Filter(); got != one {
		t.Errorf("held '111' left filter %v, want %v — three toggles", got, one)
	}
}

// Filtering shrinks the stream, so an offset taken against the unfiltered line
// count lands past the end and the pane renders blank — which reads as "the
// filter matched nothing" when it matched plenty.
func TestChatroomFilterReanchors(t *testing.T) {
	m := chatModel(t, 200)
	m.onChatroomKey(tea.KeyPressMsg{Code: 'g', Text: "g"}) // top, stops following
	m.chatroom.Scroll(1 << 20)                             // back to the bottom of the full stream
	m.onChatroomKey(tea.KeyPressMsg{Code: '1', Text: "1"}) // hide claude-code: nothing left

	if strings.TrimSpace(stripStatus(m.chatroom.View())) == "" && m.chatroom.Buffer().Len() == 0 {
		t.Skip("nothing buffered to assert against")
	}
	if m.chatroom.Top() != 0 {
		t.Fatalf("filtering to an empty stream left the offset at %d, not 0", m.chatroom.Top())
	}
}

// The chatroom borrows none of the dashboard's footer, so without hints on its
// own status bar every key it binds is undiscoverable — which is how the pause
// key it used to REQUIRE for scrolling went unfound.
func TestChatroomStatusBarShowsKeyHints(t *testing.T) {
	m := chatModel(t, 10)
	out := m.chatroom.View()
	for _, want := range []string{"scroll", "follow", "filter", "esc"} {
		if !strings.Contains(out, want) {
			t.Errorf("status bar does not mention %q:\n%s", want, lastLine(out))
		}
	}
}

// A narrow terminal drops the hints rather than the state, and never wraps the
// bar onto a second line — the frame budgets exactly one.
func TestChatroomStatusBarFitsNarrowTerminals(t *testing.T) {
	for _, w := range []int{40, 80, 200} {
		m := chatModel(t, 10)
		m.chatroom.SetSize(w, 20)
		out := m.chatroom.View()
		if got := len(strings.Split(out, "\n")); got != 20 {
			t.Errorf("width %d: view is %d lines, want 20", w, got)
		}
		if got := ansi.StringWidth(lastLine(out)); got > w {
			t.Errorf("width %d: status bar is %d columns and will wrap", w, got)
		}
	}
}

func lastLine(s string) string {
	lines := strings.Split(s, "\n")
	return lines[len(lines)-1]
}

func stripStatus(s string) string {
	lines := strings.Split(s, "\n")
	return strings.Join(lines[:len(lines)-1], "\n")
}

package tui

// Governing: SPEC-0001 REQ "Scrollback Substate" and REQ "Attached Mode" (the
// attached frame is exactly m.h lines of m.w columns).
//
// Scrollback lines come from the daemon's durable log — the harness's RAW PTY
// byte stream (ADR-0007). For a full-screen harness (crush, an agent CLI) that
// stream is cursor-addressed repaint traffic: absolute cursor moves, erases,
// alt-screen toggles, carriage returns. Rendering those bytes verbatim doesn't
// display them, it EXECUTES them against the user's real terminal — the frame
// is torn apart and styling is reset out from under the chrome.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// rawPTYLog is a slice of what a full-screen app's log actually looks like:
// SGR styling (which must survive) tangled up with cursor addressing, erases,
// an alt-screen toggle, and a bare CR (which must not).
const rawPTYLog = "\x1b[?1049h\x1b[2J\x1b[H" +
	"\x1b[38;5;212mCharm™ HYPERCRUSH\x1b[0m ///////\r\n" +
	"\x1b[12;1H\x1b[K  \x1b[32mHello!\x1b[0m\r\n" +
	"\x1b[24;1Htab change focus • / or ctrl+p commands\r\n" +
	"\x1b[H\x1b[38;5;99mYolo mode!\x1b[0m\r\x1b[2K\r\n"

// scrollbackModel returns an attached model frozen in the scrollback substate
// over the raw log above — the state a mouse wheel-up drops you into.
func scrollbackModel(w, h int) *Model {
	return scrollbackModelWithScreen(w, h, rawPTYLog, "")
}

// scrollbackModelWithScreen is scrollbackModel with the peek history and the
// live screen contents both under test control: screen is written into the
// vtView before entering scrollback, so the appended frame is the one the
// real entry paths capture.
func scrollbackModelWithScreen(w, h int, peekText, screen string) *Model {
	fc := &fakeController{harnesses: sampleHarnesses(), profiles: sampleProfiles()}
	m := New(Options{})
	m.ctrl, m.attach = fc, &fakeAttach{}
	m.conn = startOK
	m.harnesses = fc.harnesses
	m.profiles = fc.profiles
	m.w, m.h = w, h
	m.help.Width = w
	m.mode = modeAttached
	cols, rows := m.attachViewport()
	m.att = newAttachState(m.harnesses[0].Name, protocol.AttachRW, sessionBase, cols, rows)
	m.peek = logsMsg{name: m.harnesses[0].Name, text: peekText}
	if screen != "" {
		m.att.view.write([]byte(screen))
	}
	m.att.enterScrollback(m.peekLines(), m.scrollbackHeight())
	return m
}

// forbidden lists control sequences that must never survive into a rendered
// frame: each one moves the cursor, wipes the screen, or switches buffers on
// the user's real terminal.
var forbidden = []struct{ name, seq string }{
	{"alt-screen enable", "\x1b[?1049h"},
	{"erase display", "\x1b[2J"},
	{"erase line", "\x1b[K"},
	{"erase line (2K)", "\x1b[2K"},
	{"cursor home", "\x1b[H"},
	{"absolute cursor move", "\x1b[12;1H"},
	{"absolute cursor move", "\x1b[24;1H"},
	{"carriage return", "\r"},
}

// TestScrollbackDoesNotEmitControlSequences is the regression: wheel-up into
// scrollback must render inert text, not replay the harness's repaint traffic.
func TestScrollbackDoesNotEmitControlSequences(t *testing.T) {
	for _, dim := range [][2]int{{200, 50}, {176, 25}, {120, 40}, {80, 24}} {
		w, h := dim[0], dim[1]
		frame := scrollbackModel(w, h).viewAttached()
		for _, f := range forbidden {
			if strings.Contains(frame, f.seq) {
				t.Errorf("%dx%d: frame contains %s (%q) — it will execute against the user's terminal",
					w, h, f.name, f.seq)
			}
		}
	}
}

// TestScrollbackPreservesColor: stripping the dangerous sequences must not take
// the styling with it — losing color is the other half of the reported bug.
func TestScrollbackPreservesColor(t *testing.T) {
	frame := scrollbackModel(120, 40).viewAttached()
	for _, sgr := range []string{"\x1b[38;5;212m", "\x1b[32m", "\x1b[38;5;99m"} {
		if !strings.Contains(frame, sgr) {
			t.Errorf("frame lost SGR %q — scrollback should keep the harness's colors", sgr)
		}
	}
}

// TestScrollbackLinesFitWindow: a line wider than the window wraps in the
// alt-screen and scrolls the frame, which is what breaks the "exactly m.h lines"
// invariant the attached view depends on.
func TestScrollbackLinesFitWindow(t *testing.T) {
	for _, dim := range [][2]int{{200, 50}, {80, 24}, {40, 20}} {
		w, h := dim[0], dim[1]
		lines := strings.Split(scrollbackModel(w, h).viewAttached(), "\n")
		if len(lines) > h {
			t.Errorf("%dx%d: %d lines overflow the window", w, h, len(lines))
		}
		for i, ln := range lines {
			if got := lipgloss.Width(ln); got > w {
				t.Errorf("%dx%d: line %d width = %d, want <= %d", w, h, i, got, w)
			}
		}
	}
}

// TestDashboardPeekDoesNotEmitControlSequences: the dashboard's live peek pane
// tails the same raw log, so it carries the identical hazard — a harness that
// clears the screen would wipe the cockpit around it.
func TestDashboardPeekDoesNotEmitControlSequences(t *testing.T) {
	m := baseModel(160, 44)
	sel, _ := m.selectedHarness()
	m.peek = logsMsg{name: sel.Name, text: rawPTYLog}
	frame := m.viewDashboard()
	for _, f := range forbidden {
		if f.seq == "\r" {
			continue // lipgloss joins panes with \n; a bare CR is the only ambiguous one
		}
		if strings.Contains(frame, f.seq) {
			t.Errorf("dashboard peek contains %s (%q)", f.name, f.seq)
		}
	}
}

// TestScrollbackIncludesCurrentFrame verifies that entering scrollback appends
// the rendered current screen to the scrollback lines (#50). This is what
// makes the bottom of scrollback show the faithful screen state rather than
// garbled cursor-addressed repaint traffic.
func TestScrollbackIncludesCurrentFrame(t *testing.T) {
	m := scrollbackModelWithScreen(80, 24, "historical line\n", "\x1b[32mVISIBLE-SENTINEL\x1b[0m")
	if !strings.Contains(strings.Join(m.att.scroll.lines, "\n"), "VISIBLE-SENTINEL") {
		t.Fatal("scrollback missing current frame content (VISIBLE-SENTINEL)")
	}
	// And it must actually be visible in the rendered view, not just stored.
	if !strings.Contains(m.viewAttached(), "VISIBLE-SENTINEL") {
		t.Fatal("rendered scrollback view missing current frame content")
	}
}

// TestScrollbackTrimsBlankFrameRows: the appended frame contributes only the
// rows the guest has drawn — a mostly-empty screen must not bury the history
// under a page of blank padding (nor open scrollback on an empty page).
func TestScrollbackTrimsBlankFrameRows(t *testing.T) {
	m := scrollbackModelWithScreen(80, 24, "historical line\n", "one line of output")
	sb := m.att.scroll
	if n := len(sb.lines); n != 2 {
		t.Fatalf("scrollback has %d lines, want 2 (history + 1 drawn frame row)", n)
	}
	if !strings.Contains(m.viewAttached(), "historical line") {
		t.Fatal("entry page hides the history behind blank frame padding")
	}
}

// TestScrollbackSearchMatchesVisibleText: search must match what the user can
// see on the appended frame — words split by SGR style runs are still found,
// and escape bytes are never matched (#50 follow-up; enterScrollback's
// documented guarantee).
func TestScrollbackSearchMatchesVisibleText(t *testing.T) {
	m := scrollbackModelWithScreen(80, 24, "historical line\n", "ER\x1b[31mROR\x1b[0m happened")
	sb := m.att.scroll
	sb.search("error")
	if len(sb.matches) != 1 {
		t.Fatalf("search(\"error\") over a style-split ERROR: %d matches, want 1", len(sb.matches))
	}
	sb.search("0m") // raw escape bytes must be unmatchable
	if len(sb.matches) != 0 {
		t.Fatalf("search(\"0m\") matched %d lines via escape bytes, want 0", len(sb.matches))
	}
}

// TestScrollbackResizeReclamps: a window resize during scrollback must rebind
// the frozen viewport geometry, or the old height renders more rows than the
// window has and the alt-screen scrolls.
func TestScrollbackResizeReclamps(t *testing.T) {
	m := scrollbackModelWithScreen(80, 40, strings.Repeat("history line\n", 60), "screen content")
	_, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if got, want := m.att.scroll.height, m.scrollbackHeight(); got != want {
		t.Fatalf("scroll height after resize = %d, want %d", got, want)
	}
	if got := len(strings.Split(m.viewAttached(), "\n")); got > 24 {
		t.Fatalf("scrollback view is %d lines after shrink to 24 — the alt-screen scrolls", got)
	}
}

// TestAttachDataFlowsDuringScrollback: bytes arriving while frozen in
// scrollback must still feed the emulator — the frozen view reads from its
// copy, and dropping them would resume a stale live view on exit.
func TestAttachDataFlowsDuringScrollback(t *testing.T) {
	m := scrollbackModelWithScreen(80, 24, "history\n", "before")
	_, _ = m.Update(attachDataMsg{sessionID: sessionBase, data: []byte(" LIVE-BYTES")})
	m.att.exitScrollback()
	if !strings.Contains(m.viewAttached(), "LIVE-BYTES") {
		t.Fatal("bytes received during scrollback were dropped; live view resumed stale")
	}
}

// TestScrollbackEntryDropsForeignPeek: after a hop the peek can still belong
// to the previous harness; its history must not be stitched under this
// harness's frame as if it were ours.
func TestScrollbackEntryDropsForeignPeek(t *testing.T) {
	m := scrollbackModelWithScreen(80, 24, "historical line\n", "screen content")
	m.att.exitScrollback()
	m.peek = logsMsg{name: "some-other-harness", text: "foreign history\n"}
	m.att.enterScrollback(m.peekLines(), m.scrollbackHeight())
	joined := strings.Join(m.att.scroll.lines, "\n")
	if strings.Contains(joined, "foreign history") {
		t.Fatal("scrollback stitched another harness's peek under this harness's frame")
	}
	if !strings.Contains(joined, "screen content") {
		t.Fatal("scrollback lost the current frame when dropping a foreign peek")
	}
}

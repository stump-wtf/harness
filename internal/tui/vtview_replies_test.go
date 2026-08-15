package tui

// Regression coverage for issue #184.
//
// The client's emulator synthesizes replies to guest terminal queries into an
// unbuffered io.Pipe. Nothing drained it, so the first query a guest emitted
// blocked write() — which runs inside Update, on Bubble Tea's single event loop
// — and wedged the whole program: no keystrokes, no Ctrl-C, no capability
// handshake, and a client that survived SIGTERM and went on clamping the
// harness PTY for every other client (#183).
//
// A shell guest never queries the terminal, which is why this hid from both the
// existing suite and from manual testing against `bash` harnesses. Every case
// below is a sequence real agent TUIs (crush, claude) emit at startup.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
)

// guestQueries are sequences that make x/vt synthesize a reply.
var guestQueries = []struct {
	name string
	seq  string
}{
	{"DECRQM mode report (2026, synchronized output)", "\x1b[?2026$p"},
	{"DECRQM mode report (2027)", "\x1b[?2027$p"},
	{"cursor position report", "\x1b[6n"},
	{"primary device attributes", "\x1b[c"},
}

// withinTimeout runs fn and fails if it hasn't returned in time. A blocked
// write parks forever, so the deadline is what turns a hang into a test
// failure rather than a timed-out package.
func withinTimeout(t *testing.T, d time.Duration, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("%s blocked for %s — the Bubble Tea event loop would be wedged here", what, d)
	}
}

// TestVTViewWriteDoesNotBlockOnGuestQuery is the core regression: a query must
// not stall the write that carries it.
func TestVTViewWriteDoesNotBlockOnGuestQuery(t *testing.T) {
	for _, q := range guestQueries {
		t.Run(q.name, func(t *testing.T) {
			v := newVTView(174, 46)
			withinTimeout(t, 5*time.Second, "vtView.write", func() {
				v.write([]byte(q.seq))
			})
		})
	}
}

// TestVTViewSurvivesQueryFlood covers the steady state rather than the first
// byte: a live agent redraws continuously and queries repeatedly, so the pump
// has to keep up over many writes, not just unblock once.
func TestVTViewSurvivesQueryFlood(t *testing.T) {
	v := newVTView(120, 40)
	withinTimeout(t, 10*time.Second, "repeated vtView.write", func() {
		for i := 0; i < 200; i++ {
			for _, q := range guestQueries {
				v.write([]byte(q.seq))
			}
			v.write([]byte("some ordinary output\r\n"))
		}
	})
}

// TestVTViewQueryInterleavedWithOutputStillRenders proves the drain doesn't eat
// screen content: a query embedded mid-stream must leave the surrounding text
// on the screen, and must not appear in the rendered output itself.
func TestVTViewQueryInterleavedWithOutputStillRenders(t *testing.T) {
	v := newVTView(80, 10)
	withinTimeout(t, 5*time.Second, "vtView.write", func() {
		v.write([]byte("BEFORE\x1b[?2026$pAFTER"))
	})
	got := v.render()
	if !strings.Contains(got, "BEFORE") || !strings.Contains(got, "AFTER") {
		t.Errorf("screen lost content around a query: %q", firstLine(got))
	}
	if strings.Contains(got, "$p") {
		t.Errorf("query sequence leaked into the rendered screen: %q", firstLine(got))
	}
}

// TestPeekCacheRenderDoesNotBlockOnGuestQuery covers the other emulator in this
// file. The dashboard peek pane replays a harness's scrollback tail through a
// throwaway view, so it takes guest bytes — queries included — on a path that
// also runs inside Update.
func TestPeekCacheRenderDoesNotBlockOnGuestQuery(t *testing.T) {
	for _, q := range guestQueries {
		t.Run(q.name, func(t *testing.T) {
			var pc peekCache
			withinTimeout(t, 5*time.Second, "peekCache.render", func() {
				pc.render("tail output\r\n"+q.seq+"more output\r\n", 80, 10)
			})
		})
	}
}

// TestVTViewResetClearsAndKeepsDraining covers the re-use path that replaces
// closing: a view handed to a different harness must come back blank at the new
// size, and must still drain queries afterwards.
func TestVTViewResetClearsAndKeepsDraining(t *testing.T) {
	v := newVTView(80, 24)
	v.write([]byte("OLD HARNESS OUTPUT"))
	withinTimeout(t, 5*time.Second, "vtView.reset", func() { v.reset(120, 30) })
	if got := v.render(); strings.Contains(got, "OLD HARNESS OUTPUT") {
		t.Error("reset left the previous harness's output on screen")
	}
	if v.cols != 120 || v.rows != 30 {
		t.Errorf("reset produced %dx%d, want 120x30", v.cols, v.rows)
	}
	withinTimeout(t, 5*time.Second, "write after reset", func() {
		v.write([]byte("\x1b[?2026$pNEW OUTPUT"))
	})
	if got := v.render(); !strings.Contains(got, "NEW OUTPUT") {
		t.Errorf("post-reset output missing: %q", firstLine(got))
	}
}

// TestPeekCacheReusesOneView pins the property that keeps the pump count fixed:
// repeated renders with changing tails must not build a new view each time. The
// peek pane misses roughly once a second against a live harness, so a view per
// miss would be a goroutine per second.
func TestPeekCacheReusesOneView(t *testing.T) {
	var pc peekCache
	pc.render("first tail\r\n", 80, 10)
	first := pc.view
	if first == nil {
		t.Fatal("peekCache did not retain a view")
	}
	for i := 0; i < 25; i++ {
		pc.render(fmt.Sprintf("tail %d\r\n\x1b[6n", i), 80, 10)
	}
	if pc.view != first {
		t.Error("peekCache built a new view on a miss; each one starts another reply pump")
	}
}

// firstLine trims a rendered screen to something readable in a failure message.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// compile-time assertion that the reader the pump relies on is part of the
// interface the view actually holds.
var _ interface {
	Read([]byte) (int, error)
} = vt.Terminal(nil)

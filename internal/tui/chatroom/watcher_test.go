// Chatroom watcher lifecycle
//
// The chatroom's tail.Watcher runs a blocking poll loop. Init is called from
// inside Bubble Tea's Update, so starting the watcher inline deadlocks the
// whole TUI — no repaint, no keystrokes, no quit. These tests pin the two
// properties that keep that from regressing: Init returns promptly, and Stop
// tears the watcher down without leaving the cancel func dangling.
//
// @joestump 08/21/2026 - Added alongside the fix for the synchronous
// watcher.Start in PR #248.

package chatroom

import (
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/tui/theme"
)

// Init must return promptly: tail.Watcher.Start is a poll loop that never
// returns, so it has to be launched on its own goroutine.
func TestInitDoesNotBlock(t *testing.T) {
	m := New(theme.Default(), nil)
	t.Cleanup(m.Stop)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = m.Init()
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Init did not return: watcher.Start is being called inline, which deadlocks the TUI update loop")
	}
}

// Stop must cancel the watcher context and drop the references, so a chatroom
// opened and closed repeatedly does not leak a poll loop per visit.
func TestStopCancelsWatcher(t *testing.T) {
	m := New(theme.Default(), nil)
	_ = m.Init()

	if m.watcher == nil || m.cancel == nil {
		t.Fatal("Init left the watcher or its cancel func unset")
	}

	m.Stop()

	if m.watcher != nil {
		t.Error("Stop left m.watcher set")
	}
	if m.cancel != nil {
		t.Error("Stop left m.cancel set; the watcher context is never cancelled")
	}
	if m.running {
		t.Error("Stop left m.running true")
	}

	// Stop is called from exitChatroom and again from Model.Close; the second
	// call must not panic.
	m.Stop()
}

// truncateShort clips by rune, not by byte: transcript summaries carry paths,
// quoted output and box drawing, and a byte slice lands mid-rune.
func TestTruncateShortIsRuneSafe(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		n    int
		want string
	}{
		{"ascii under limit", "hello", 10, "hello"},
		{"ascii over limit", "hello world", 8, "hello w…"},
		{"trims first", "  spaced  ", 10, "spaced"},
		{"multibyte under limit", "héllo wörld", 20, "héllo wörld"},
		{"multibyte over limit", "héllo wörld ünïcode", 8, "héllo w…"},
		{"cjk over limit", "编辑文件内容示例", 4, "编辑文…"},
		{"zero width", "anything", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateShort(tc.in, tc.n)
			if got != tc.want {
				t.Fatalf("truncateShort(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
			}
			for _, r := range got {
				if r == '�' {
					t.Fatalf("truncateShort(%q, %d) = %q — split a multi-byte rune", tc.in, tc.n, got)
				}
			}
		})
	}
}

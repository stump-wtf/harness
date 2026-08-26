package supervisor

// Governing: #279 — the durable per-harness log stores sanitized output
// history (scrolled-off screen lines + a final-screen flush), never the raw
// PTY byte stream. These tests pin the three properties that make `harness
// logs` readable again: in-place repaints (a TUI agent's per-second
// spinner/timer frames) produce nothing, scrolled output lines land verbatim,
// and the end-of-run screen flush recovers a short run that never scrolled.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// spinnerFrames simulates what a full-screen agent TUI emits every second
// while a tool runs: home the cursor, clear the line, repaint a status line
// whose only change is the elapsed-seconds counter. One frame per second of a
// run must yield ZERO log lines.
func spinnerFrames(seconds int) []byte {
	var b bytes.Buffer
	for i := 1; i <= seconds; i++ {
		b.WriteString("\x1b[H\x1b[2J") // home + clear
		fmt.Fprintf(&b, "\x1b[1;1H✻ Working (%ds)…\r\n", i)
		// Last painted row carries no trailing newline — a real TUI leaves the
		// cursor on the row it just wrote, exactly so it does not scroll.
		fmt.Fprintf(&b, "\x1b[2;1H\x1b[31mtool: bash\x1b[0m")
	}
	return b.Bytes()
}

// TestPtyHistoryRepaintsProduceNoLines is the #279 regression: a 60-second
// tool run's worth of repaint frames must not add a single line (previously
// the raw tee recorded one junk line per second).
func TestPtyHistoryRepaintsProduceNoLines(t *testing.T) {
	var out bytes.Buffer
	h := newPtyHistory(&out, 80, 24)
	if _, err := h.Write(spinnerFrames(60)); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 0 {
		t.Fatalf("repaint-only stream wrote %d bytes, want 0:\n%q", out.Len(), out.String())
	}
}

// TestPtyHistoryRepaintsContainNoEscapes doubles down on the same stream: even
// after scrolling content, the log must never contain an ESC byte.
func TestPtyHistoryRepaintsContainNoEscapes(t *testing.T) {
	var out bytes.Buffer
	h := newPtyHistory(&out, 80, 24)
	_, _ = h.Write(spinnerFrames(10))
	// Now let real output scroll everything off.
	for i := 0; i < 40; i++ {
		_, _ = h.Write([]byte(fmt.Sprintf("real line %d\r\n", i)))
	}
	if bytes.ContainsRune(out.Bytes(), 0x1b) {
		t.Fatalf("log contains ESC bytes:\n%q", out.String())
	}
}

// TestPtyHistoryCapturesScrolledLines verifies printed lines land verbatim
// once they scroll off the screen (screen is 24 rows).
func TestPtyHistoryCapturesScrolledLines(t *testing.T) {
	var out bytes.Buffer
	h := newPtyHistory(&out, 80, 24)
	for i := 1; i <= 30; i++ {
		_, _ = h.Write([]byte(fmt.Sprintf("line %02d\r\n", i)))
	}
	got := out.String()
	// Lines 1..7 (30 - 24 + 1) have scrolled off and must be recorded.
	for i := 1; i <= 7; i++ {
		want := fmt.Sprintf("line %02d\n", i)
		if !strings.Contains(got, want) {
			t.Errorf("log missing scrolled-off %q; got:\n%s", want, got)
		}
	}
	// Lines still on screen must not be double-recorded yet.
	if strings.Contains(got, "line 08\n") && strings.Contains(got, "line 08\nline 09") {
		t.Errorf("on-screen lines recorded before scrolling:\n%s", got)
	}
}

// TestPtyHistoryFlushLandsFinalScreen verifies the short-run path: output that
// never scrolled is appended when the stream ends.
func TestPtyHistoryFlushLandsFinalScreen(t *testing.T) {
	var out bytes.Buffer
	h := newPtyHistory(&out, 80, 24)
	_, _ = h.Write([]byte("\x1b[32mhello\x1b[0m world\r\n"))
	_, _ = h.Write([]byte("second line\r\n"))
	h.Flush()
	got := out.String()
	for _, want := range []string{"hello world\n", "second line\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("flushed screen missing %q; got:\n%s", want, got)
		}
	}
	if bytes.ContainsRune([]byte(got), 0x1b) {
		t.Errorf("flushed screen contains ESC bytes:\n%q", got)
	}
}

// TestPtyHistoryFlushIsOnce verifies a second Flush (closeLog runs
// defensively after the reader EOF flush) cannot duplicate the screen.
func TestPtyHistoryFlushIsOnce(t *testing.T) {
	var out bytes.Buffer
	h := newPtyHistory(&out, 80, 24)
	_, _ = h.Write([]byte("only line\r\n"))
	h.Flush()
	h.Flush()
	if n := strings.Count(out.String(), "only line\n"); n != 1 {
		t.Errorf("flush wrote the screen %d times, want 1", n)
	}
}

// TestLogRecordsLifecycleEvents verifies the durable log carries structured
// lifecycle lines (charmbracelet/log) alongside the output history (#279): a
// harness that runs and exits leaves "state changed" and "exited" events in
// its log, so `harness logs` explains a dead harness without any PTY bytes.
func TestLogRecordsLifecycleEvents(t *testing.T) {
	dir := t.TempDir()
	h := shHarness("ephemeral", "echo DONE", 0)
	h.Restart = core.RestartNo
	s := New(h, Options{
		Policy: Policy{CrashWindow: time.Second, CrashThreshold: 3, MaxRestarts: 5, StopGrace: 200 * time.Millisecond},
		Bus:    NewBus(),
		LogCfg: LogConfig{Dir: dir},
	})
	s.Restore(true, 0, 0, time.Time{})
	s.Start()
	waitState(t, s, core.StateStopped) // RestartNever: a clean exit lands stopped
	s.Shutdown()

	data, err := os.ReadFile(filepath.Join(dir, "ephemeral.log"))
	if err != nil {
		t.Fatal(err)
	}
	got := string(data)
	for _, want := range []string{"state changed", "exited", "DONE"} {
		if !strings.Contains(got, want) {
			t.Errorf("log missing %q; got:\n%s", want, got)
		}
	}
	if bytes.ContainsRune(data, 0x1b) {
		t.Errorf("log contains ESC bytes:\n%q", got)
	}
}

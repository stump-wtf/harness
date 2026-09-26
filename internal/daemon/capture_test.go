package daemon

// Governing: issue #735; ADR-0040 (screen observability). These cover both
// halves over a real socket with a real supervised process: `capture` answers
// with the guest's visible screen (which the durable log deliberately never
// receives, ADR-0007), and list/describe project the waiting verdict only for
// a screen that is BOTH silent past the threshold AND prompt-shaped.

import (
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/protocol"
)

// promptTOML is a harness that paints a permission dialog onto its screen and
// then goes quiet — the exact stuck shape a sweep needs to see. The dialog is
// one line plus a trailing newline, so it sits ON the glass and never scrolls;
// `harness logs --raw` for this harness is empty by design.
const promptTOML = `
[harness.stuck]
harness = "generic"
args = ["-c", "printf 'Read outside the working directories\\n'; printf 'Proceed? (y/n)\\n'; sleep 60"]
description = "painted a prompt, then went silent"
`

// quietTOML is a harness whose screen stays blank.
const quietTOML = `
[harness.quiet]
harness = "generic"
args = ["-c", "sleep 60"]
description = "silent, no prompt"
`

// waitFor polls until cond passes or the deadline lapses; the PTY feed and the
// daemon's projections are asynchronous, so every assertion below polls.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestCaptureServesScreenWithoutTTY is the sweep's contract: a non-interactive
// capture returns the guest's visible screen as plain text, with the viewport
// and the screen's silence — the answer `harness logs` cannot give for a
// full-screen prompt (ADR-0007 stores scrolled-off rows only).
func TestCaptureServesScreenWithoutTTY(t *testing.T) {
	td := newTestDaemon(t, promptTOML)
	if _, err := td.dial(t, nil).Start("stuck"); err != nil {
		t.Fatalf("start: %v", err)
	}
	c := td.dial(t, nil)

	var cd protocol.CaptureData
	waitFor(t, "the prompt to reach the emulator", func() bool {
		var err error
		cd, err = c.Capture("stuck", false)
		if err != nil {
			t.Fatalf("capture: %v", err)
		}
		return strings.Contains(cd.Text, "Read outside the working directories")
	})
	if !strings.Contains(cd.Text, "Proceed? (y/n)") {
		t.Errorf("capture text = %q, want the whole dialog", cd.Text)
	}
	if strings.ContainsAny(cd.Text, "\x1b") {
		t.Errorf("plain capture leaked escape bytes: %q", cd.Text)
	}
	if cd.Cols == 0 || cd.Rows == 0 {
		t.Errorf("capture geometry = %dx%d, want the guest viewport", cd.Cols, cd.Rows)
	}
	if cd.IdleMs < 0 {
		t.Errorf("idle = %dms, want non-negative", cd.IdleMs)
	}

	// The ANSI view is the styled repaint, requested explicitly — the same
	// bytes an attach client's first frame carries.
	styled, err := c.Capture("stuck", true)
	if err != nil {
		t.Fatalf("ansi capture: %v", err)
	}
	if !strings.HasPrefix(styled.Ansi, "\x1b[0m\x1b[2J\x1b[H") {
		t.Errorf("ansi capture = %q, want a self-contained repaint", styled.Ansi[:min2(len(styled.Ansi), 20)])
	}
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestCaptureNoScreen pins the no_screen error: a harness the daemon has never
// teed output for has nothing truthful to render, and asking for one must not
// materialize an empty emulator for it.
func TestCaptureNoScreen(t *testing.T) {
	td := newTestDaemon(t, quietTOML)
	c := td.dial(t, nil)
	_, err := c.Capture("quiet", false)
	em, ok := err.(*protocol.ErrorMsg)
	if !ok {
		t.Fatalf("error type = %T, want *protocol.ErrorMsg", err)
	}
	if em.Code != protocol.ErrNoScreen {
		t.Errorf("code = %s, want %s", em.Code, protocol.ErrNoScreen)
	}
}

// TestListReportsWaitingForStuckPrompt is the detection half: a harness frozen
// at a prompt-shaped, silent screen is reported waiting in list (and describe
// shares infoFor, so the projection is identical), while a silent screen
// WITHOUT a prompt and a prompt-shaped screen still receiving bytes both stay
// unflagged.
func TestListReportsWaitingForStuckPrompt(t *testing.T) {
	td := newTestDaemon(t, promptTOML, func(o *Options) { o.WaitingIdle = 150 * time.Millisecond })
	if _, err := td.dial(t, nil).Start("stuck"); err != nil {
		t.Fatalf("start: %v", err)
	}
	c := td.dial(t, nil)

	var info protocol.HarnessInfo
	waitFor(t, "the waiting verdict", func() bool {
		hs, err := c.List()
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		for _, h := range hs {
			if h.Name == "stuck" {
				info = h
			}
		}
		return info.Waiting
	})
	if info.State != "running" {
		t.Errorf("state = %q, want running (the process is alive; the glass is stuck)", info.State)
	}
	if info.WaitingFor != "read outside workdir" {
		t.Errorf("waiting_for = %q, want the matched pattern label", info.WaitingFor)
	}

	// A prompt-shaped screen still receiving bytes is a BUSY harness.
	// lastWrite only moves on real PTY traffic, so this is exercised by the
	// mux-level test (TestScreenStateIdleIsLastByteNotLastChange); here the
	// guard is the quiet harness: silent, but no prompt on the glass — and a
	// fresh prompt inside the threshold — must never read waiting.
	td2 := newTestDaemon(t, quietTOML, func(o *Options) { o.WaitingIdle = 150 * time.Millisecond })
	if _, err := td2.dial(t, nil).Start("quiet"); err != nil {
		t.Fatalf("start quiet: %v", err)
	}
	waitFor(t, "the quiet harness to paint its screen", func() bool {
		_, ok := td2.reg.ScreenFor("quiet", time.Now())
		return ok
	})
	time.Sleep(300 * time.Millisecond)
	hs, err := td2.dial(t, nil).List()
	if err != nil {
		t.Fatalf("list quiet: %v", err)
	}
	for _, h := range hs {
		if h.Name == "quiet" && h.Waiting {
			t.Errorf("a blank screen idle 300ms must not read waiting: %+v", h)
		}
	}
}

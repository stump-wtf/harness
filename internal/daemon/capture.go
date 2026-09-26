package daemon

// Stuck-at-prompt observability: `capture` (a non-interactive screen dump) and
// the waiting projection `harness list` renders.
//
// The durable log deliberately records only scrolled-off rows (ADR-0007, as
// amended for #279), so a full-screen prompt — permission dialog, workspace
// trust, MCP consent — never lands in `harness logs`, and a sweep that greps
// the log confidently reports "nothing is stuck" while the harness plainly is.
// The x/vt emulator the attach plane already feeds (ADR-0003) is the truthful
// source: it holds every byte the guest wrote, projected to the visible screen.
// Both halves of this file read that screen; neither sends input, and neither
// interprets what kind of program drew it (issue #735; ADR-0040).
//
// Governing: SPEC-0002 REQ "Control Operations" (the capture op mirrors the
// CLI verb 1:1, ADR-0002).

import (
	"strings"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/promptd"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// opCapture serves the capture op: the harness's visible screen as plain text
// (plus the ANSI repaint when the request asked), with the viewport and the
// screen's idle age. Unknown harness is a structured ERROR; a harness whose
// daemon has never teed output has no emulator and answers no_screen — there
// is nothing truthful to render, and materializing an empty Mux to say so
// would defeat the point (the Registry's SnapshotFor discipline).
func (c *conn) opCapture(req protocol.ControlReq) {
	if _, ok := c.srv.mgr.Snapshot(req.Name); !ok {
		_ = c.pc.WriteError(req.ID, protocol.ErrUnknownHarness, "unknown harness %q", req.Name)
		return
	}
	st, ok := c.srv.reg.ScreenFor(req.Name, time.Now())
	if !ok || !st.Fed {
		_ = c.pc.WriteError(req.ID, protocol.ErrNoScreen,
			"harness %q has no captured screen yet (nothing has teed output for it)", req.Name)
		return
	}
	data := protocol.CaptureData{
		Name:   req.Name,
		Text:   strings.Join(st.Rows, "\n"),
		Cols:   st.Cols,
		Rows:   st.RowsAt,
		IdleMs: st.Idle.Milliseconds(),
	}
	if req.Ansi {
		if ansi, ok := c.srv.reg.AnsiFor(req.Name); ok {
			data.Ansi = string(ansi)
		}
	}
	c.respond(req, data)
}

// waitingFor computes the stuck-at-prompt projection for one snapshot: a
// running harness whose visible screen has been unchanged for
// promptd.DefaultIdleThreshold with a known interactive-prompt pattern on it.
// Anything else — not running, never teed output, a busy screen, an idle
// screen that shows no prompt — is not waiting, and ok is false. Only the
// RUNNING state qualifies: a restarting or stopping harness is already
// accounted for by its own state, and re-labeling it "waiting" would hide the
// transition the operator needs to see. idle rides along so the caller does
// not pay a second registry round-trip to render the age.
func (c *conn) waitingFor(snap supervisor.Snapshot, now time.Time) (hit promptd.Hit, idle time.Duration, waiting bool) {
	if snap.State != core.StateRunning {
		return promptd.Hit{}, 0, false
	}
	st, ok := c.srv.reg.ScreenFor(snap.Name, now)
	if !ok || !st.Fed {
		return promptd.Hit{}, 0, false
	}
	hit, waiting = promptd.Waiting(st.Rows, st.Idle, c.srv.waitingIdle)
	return hit, st.Idle, waiting
}

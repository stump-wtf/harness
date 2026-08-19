package tui

// Governing: SPEC-0002 REQ "Control Operations" (the typed calls the TUI issues)
// and SPEC-0001 (the reactive dashboard). Each tea.Cmd here runs one control
// call off the UI goroutine and returns a typed message; control calls are
// serialized by the model's mutexController so the non-concurrent Client is
// never touched by two Cmds at once (client.go: "not safe for concurrent
// control Calls").

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	osc52 "github.com/aymanbagabas/go-osc52/v2"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// --- messages ---

// refreshMsg carries a fresh list + profiles snapshot for the dashboard.
type refreshMsg struct {
	harnesses []protocol.HarnessInfo
	profiles  []protocol.ProfileInfo
	daemon    protocol.DaemonInfo
	err       error
}

// logsMsg is a read-only tail for the peek pane (SPEC-0001 REQ "Dashboard":
// live read-only tail without attaching). cols/rows carry the guest's
// authoritative viewport — the geometry the tail was drawn at — so viewPeek can
// replay it faithfully; both are 0 when the daemon has no viewport to report.
type logsMsg struct {
	name string
	text string
	cols int
	rows int
	err  error
}

// opResultMsg is the result of a start/stop/restart action.
type opResultMsg struct {
	action Action
	name   string
	info   protocol.HarnessInfo
	err    error
}

// reloadResultMsg is the result of a config reload (SPEC-0001 REQ "Harness
// Form" / "Zero And Error States": a parse failure returns a reload_failed
// error and the banner, last-good config kept).
type reloadResultMsg struct {
	harnesses []protocol.HarnessInfo
	err       error
}

// profileSwitchMsg is the result of activating a profile plus the set of stopped
// members the non-destructive "start stopped" would touch.
type profileSwitchMsg struct {
	profiles []protocol.ProfileInfo
	toStart  []string
	err      error
}

// tickMsg drives the peek-pane refresh and backoff countdowns.
type tickMsg time.Time

// copyResultMsg reports that an OSC52 clipboard write was emitted (ok is
// false when there was nothing to copy). The text is echoed back so the
// status line can confirm what landed on the clipboard.
type copyResultMsg struct {
	text string
	ok   bool
}

// --- commands ---

// fetchState fetches list + profiles + daemon info in one Cmd.
func fetchState(ctrl Controller) tea.Cmd {
	return func() tea.Msg {
		hs, err := ctrl.List()
		if err != nil {
			return refreshMsg{err: err}
		}
		ps, err := ctrl.Profiles()
		if err != nil {
			return refreshMsg{err: err}
		}
		di, _ := ctrl.DaemonInfo()
		return refreshMsg{harnesses: hs, profiles: ps, daemon: di}
	}
}

// fetchLogs pulls a read-only tail for the peek pane.
func fetchLogs(ctrl Controller, name string, lines int) tea.Cmd {
	if name == "" {
		return nil
	}
	return func() tea.Msg {
		ld, err := ctrl.Logs(name, lines)
		return logsMsg{name: name, text: ld.Text, cols: ld.Cols, rows: ld.Rows, err: err}
	}
}

// doAction runs a start/stop/restart against a harness.
func doAction(ctrl Controller, a Action, name string) tea.Cmd {
	return func() tea.Msg {
		var (
			info protocol.HarnessInfo
			err  error
		)
		switch a {
		case ActionStart:
			info, err = ctrl.Start(name)
		case ActionStop:
			info, err = ctrl.Stop(name)
		case ActionRestart:
			info, err = ctrl.Restart(name)
		}
		return opResultMsg{action: a, name: name, info: info, err: err}
	}
}

// doReload asks the daemon to re-read its config (ADR-0006).
func doReload(ctrl Controller) tea.Cmd {
	return func() tea.Msg {
		hs, err := ctrl.Reload()
		return reloadResultMsg{harnesses: hs, err: err}
	}
}

// doEnable sets a harness's enabled intent and starts it.
func doEnable(ctrl Controller, name string) tea.Cmd {
	return func() tea.Msg {
		info, err := ctrl.Enable(name)
		return opResultMsg{action: ActionToggle, name: name, info: info, err: err}
	}
}

// doDisable clears a harness's enabled intent and stops it.
func doDisable(ctrl Controller, name string) tea.Cmd {
	return func() tea.Msg {
		info, err := ctrl.Disable(name)
		return opResultMsg{action: ActionToggle, name: name, info: info, err: err}
	}
}

// doSwitchProfile activates profile name, then (if startStopped) starts its
// stopped members non-destructively (SPEC-0001 REQ "Profile Switcher").
func doSwitchProfile(ctrl Controller, name string, startStopped bool, harnesses []protocol.HarnessInfo) tea.Cmd {
	return func() tea.Msg {
		ps, err := ctrl.UseProfile(name)
		if err != nil {
			return profileSwitchMsg{err: err}
		}
		var toStart []string
		if startStopped {
			if p := findProfile(ps, name); p != nil {
				toStart = stoppedMembers(*p, harnesses)
				for _, hn := range toStart {
					_, _ = ctrl.Start(hn) // non-destructive: only stopped members
				}
			}
		}
		return profileSwitchMsg{profiles: ps, toStart: toStart}
	}
}

// writeConfigAndReload writes the new harness.toml body then reloads the daemon
// (SPEC-0001 REQ "Harness Form": the harness lands in harness.toml, the daemon
// reloads, and it appears on the dashboard).
func writeConfigAndReload(ctrl Controller, path string, body []byte) tea.Cmd {
	return func() tea.Msg {
		if err := os.WriteFile(path, body, 0o644); err != nil {
			return reloadResultMsg{err: err}
		}
		hs, err := ctrl.Reload()
		return reloadResultMsg{harnesses: hs, err: err}
	}
}

// tick schedules the next periodic refresh/countdown tick.
func tick() tea.Cmd {
	return tea.Tick(time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

// copyToClipboard emits an OSC52 escape sequence that places text on the
// system clipboard, and returns a copyResultMsg so the status line can
// confirm. OSC52 is used rather than a shell-out to pbcopy/xclip because it
// works over SSH and inside tmux (the sequence is forwarded to the outer
// terminal), which is exactly where a supervising cockpit tends to run.
//
// The sequence is written to stderr: Bubble Tea's alt-screen renderer owns
// stdout, but OSC52 is a non-printing operating-system command, so emitting
// it on the controlling terminal's other stream neither disturbs the layout
// nor interleaves with frame writes.
func copyToClipboard(text string) tea.Cmd {
	if text == "" {
		return func() tea.Msg { return copyResultMsg{ok: false} }
	}
	return func() tea.Msg {
		seq := osc52.New(text)
		// tmux and screen intercept the outer escape and require it wrapped in
		// their own DCS passthrough, or the sequence never reaches the real
		// terminal. TERM tells us which wrapper (if any) applies.
		switch {
		case os.Getenv("TMUX") != "":
			seq = seq.Tmux()
		case strings.HasPrefix(os.Getenv("TERM"), "screen"):
			seq = seq.Screen()
		}
		_, _ = fmt.Fprint(os.Stderr, seq.String())
		return copyResultMsg{text: text, ok: true}
	}
}

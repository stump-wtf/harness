package tui

// Governing: SPEC-0001 REQ "Zero And Error States" — no-daemon (offer inline
// start, don't just error), empty-profile zero-state, config-parse-error banner
// (last-good config, ADR-0006), reconnecting overlay on disconnect (ADR-0002).
// This file classifies the daemon-connection and reload conditions the model
// renders those states from.

import (
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"syscall"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// startCondition is how the TUI came up with respect to the daemon.
type startCondition int

const (
	// startOK: connected to a running daemon.
	startOK startCondition = iota
	// startNoDaemon: no socket / connection refused — offer to start the daemon
	// inline rather than erroring out (SPEC-0001 scenario "Daemon not running").
	startNoDaemon
	// startOtherErr: a different, fatal dial error (e.g. permission).
	startOtherErr
)

// classifyDialErr decides how to present a client.Dial failure. A missing socket
// file (ENOENT) or a refused connection means "no daemon" — the friendly inline
// offer; anything else is a real error.
func classifyDialErr(err error) startCondition {
	if err == nil {
		return startOK
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ECONNREFUSED) {
		return startNoDaemon
	}
	// net.OpError wrapping ECONNREFUSED / ENOENT for unix sockets.
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		msg := opErr.Err.Error()
		if strings.Contains(msg, "connection refused") ||
			strings.Contains(msg, "no such file") {
			return startNoDaemon
		}
	}
	msg := err.Error()
	if strings.Contains(msg, "connection refused") || strings.Contains(msg, "no such file") {
		return startNoDaemon
	}
	return startOtherErr
}

// isDisconnect reports whether a read-loop error means the daemon connection
// dropped (so the TUI shows the reconnecting overlay — harnesses are fine, only
// the view died, ADR-0002).
func isDisconnect(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "closed") ||
		strings.Contains(msg, "reset") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "EOF")
}

// reloadBanner turns a reload error into the non-fatal banner text (SPEC-0001
// scenario "Bad config reload": last-good config kept, banner shows the parse
// location). A reload_failed ErrorMsg carries the "file:line: message" the
// daemon produced (ADR-0006); anything else is surfaced verbatim. Returns ""
// when err is nil or not a reload failure.
func reloadBanner(err error) string {
	if err == nil {
		return ""
	}
	var em *protocol.ErrorMsg
	if errors.As(err, &em) && em.Code == protocol.ErrReload {
		return "using last-good config — " + em.Message
	}
	return ""
}

// emptyStateText is the friendly zero-state for a profile/daemon with no visible
// harnesses (SPEC-0001: "press n to create your first harness").
func emptyStateText(profile string) string {
	if profile != "" {
		return "No harnesses in profile " + profile + " — press a to show all, or n to create one."
	}
	return "No harnesses yet — press n to create your first harness."
}

// noDaemonText is the inline no-daemon offer.
func noDaemonText(socket string) string {
	return "No daemon at " + socket + ".\nPress s to start harnessd here, or q to quit."
}

package tui

import (
	"errors"
	"io"
	"net"
	"os"
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// TestClassifyDialErr verifies the no-daemon detection that drives the inline
// "start the daemon" offer (SPEC-0001 scenario "Daemon not running").
func TestClassifyDialErr(t *testing.T) {
	if classifyDialErr(nil) != startOK {
		t.Error("nil err should be startOK")
	}
	if classifyDialErr(os.ErrNotExist) != startNoDaemon {
		t.Error("ENOENT should be startNoDaemon")
	}
	// A real dial to a socket that isn't there.
	_, err := net.Dial("unix", "/nonexistent/definitely/not/here.sock")
	if err == nil {
		t.Skip("unexpectedly dialed a missing socket")
	}
	if got := classifyDialErr(err); got != startNoDaemon {
		t.Errorf("missing-socket dial classified as %v, want startNoDaemon (%v)", got, startNoDaemon)
	}
	if classifyDialErr(errors.New("permission denied")) != startOtherErr {
		t.Error("an unrelated error should be startOtherErr")
	}
}

// TestReloadBanner verifies the SPEC-0001 scenario "Bad config reload": a
// reload_failed error becomes the non-fatal last-good banner carrying the parse
// location; a non-reload error yields no banner.
func TestReloadBanner(t *testing.T) {
	em := &protocol.ErrorMsg{Code: protocol.ErrReload, Message: "harnessd.toml:12: unexpected token"}
	got := reloadBanner(em)
	if !strings.Contains(got, "last-good") || !strings.Contains(got, ":12:") {
		t.Fatalf("reloadBanner = %q, want last-good + parse location", got)
	}
	if reloadBanner(nil) != "" {
		t.Error("nil err should yield no banner")
	}
	if reloadBanner(&protocol.ErrorMsg{Code: protocol.ErrUnknownHarness, Message: "x"}) != "" {
		t.Error("a non-reload error should not raise the config banner")
	}
}

// TestIsDisconnect verifies the reconnecting-overlay trigger (ADR-0002: the
// client survives daemon loss).
func TestIsDisconnect(t *testing.T) {
	if !isDisconnect(io.EOF) {
		t.Error("EOF should be a disconnect")
	}
	if !isDisconnect(net.ErrClosed) {
		t.Error("ErrClosed should be a disconnect")
	}
	if isDisconnect(nil) {
		t.Error("nil is not a disconnect")
	}
}

// TestEmptyStateText verifies the zero-state copy adapts to profile vs global.
func TestEmptyStateText(t *testing.T) {
	if !strings.Contains(emptyStateText(""), "first harness") {
		t.Error("global empty state should invite creating the first harness")
	}
	if !strings.Contains(emptyStateText("signal-ops"), "signal-ops") {
		t.Error("profile empty state should name the profile")
	}
}

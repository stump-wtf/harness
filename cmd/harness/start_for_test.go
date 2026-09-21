package main

// After-Hours Lease CLI Tests
//
// `harness start NAME --for DURATION` (SPEC-0012 REQ "After-Hours Lease").
// Only the argument-level contract is asserted here — the invalid-duration
// and --all-combination rejections fire in the command tree BEFORE the
// daemon dial, so they are testable without one; the daemon-side behaviour
// is covered by the control-op tests (internal/daemon).
//
// Governing: SPEC-0012 REQ "After-Hours Lease"; issue #383.
//
// @joestump-agent 09/21/2026 - Added for stump.wtf/harness#383.

import (
	"io"
	"strings"
	"testing"
)

// driveStartFor runs the command tree for `harness start NAME --for VALUE`
// with no daemon listening. Every rejection under test fires in the command
// tree before the dial; the dial error itself is the success signal for a
// well-formed invocation.
func driveStartFor(t *testing.T, forValue string) error {
	t.Helper()
	root := newRootCmd()
	// A nonexistent socket keeps the tree's dial from reaching a live daemon
	// on this box's default path — the flags-under-test are decided before it.
	root.SetArgs([]string{"--socket", "/nonexistent/hnd-lease-test.sock", "start", "--for", forValue, "some-harness"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	return root.Execute()
}

func TestStartForRejectsInvalidDuration(t *testing.T) {
	for _, bad := range []string{"bogus", "5", "-2h", "1hour"} {
		err := driveStartFor(t, bad)
		if err == nil {
			t.Errorf("start --for %q without a daemon: expected the dial error (flags accepted), got nil", bad)
			continue
		}
		if !strings.Contains(err.Error(), "invalid --for duration") {
			t.Errorf("--for %q error = %v, want the invalid-duration rejection", bad, err)
		}
	}
}

// A well-formed duration passes the tree and reaches the dial, which fails
// with no daemon — the success signal for the argument contract.
func TestStartForAcceptsWellFormedDuration(t *testing.T) {
	err := driveStartFor(t, "2h30m")
	if err == nil {
		t.Fatal("start --for 2h30m without a daemon: expected the dial error, got nil")
	}
	if strings.Contains(err.Error(), "invalid --for duration") {
		t.Fatalf("well-formed duration rejected: %v", err)
	}
}

// Positivity is checked client-side too, not only at the daemon: a negative
// or zero duration is a typo, and the operator should hear that without a
// daemon round-trip.
func TestStartForRejectsNonPositiveDuration(t *testing.T) {
	for _, bad := range []string{"-2h", "0s"} {
		err := driveStartFor(t, bad)
		if err == nil {
			t.Errorf("start --for %q without a daemon: expected the dial error, got nil", bad)
			continue
		}
		if !strings.Contains(err.Error(), "must be positive") {
			t.Errorf("--for %q error = %v, want the positivity rejection", bad, err)
		}
	}
}

func TestStartForRejectsAllCombination(t *testing.T) {
	root := newRootCmd()
	root.SetArgs([]string{"--socket", "/nonexistent/hnd-lease-test.sock", "start", "--for", "2h", "--all"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "cannot combine --all and --for") {
		t.Fatalf("start --for --all error = %v, want the combination rejection", err)
	}
}

// stop never grew a --for: the flag must not exist on the other lifecycle
// verbs, or a typo would lease something silently.
func TestStopHasNoForFlag(t *testing.T) {
	root := newRootCmd()
	root.SetArgs([]string{"--socket", "/nonexistent/hnd-lease-test.sock", "stop", "--for", "2h", "some-harness"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	err := root.Execute()
	if err == nil || !strings.Contains(err.Error(), "unknown flag: --for") {
		t.Fatalf("stop --for error = %v, want an unknown-flag rejection", err)
	}
}

package main

// Governing tests: SPEC-0011 REQ "Scratchpad Creation" — the auto-attach
// default. `harness run` is the tmux `new-session` gesture, not `new-session
// -d`: by default it drops the caller straight into the scratchpad it just
// minted. These tests pin that default (and its opt-outs: --detach, --json,
// and a non-TTY stdout) against a real in-process daemon so a future change
// to cmdRun can't silently flip the default back to "print and exit" without
// a test noticing.
//
// Two package-level seams make this testable without a real PTY or Bubble
// Tea program (mirrors exitFn in root.go):
//   - runAttachFn stands in for cmdAttach.
//   - runStdoutIsTTY stands in for cliui.WriterIsTTY(os.Stdout), which is
//     always false under `go test` (its stdout is a pipe, never a terminal).

import (
	"encoding/json"
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/harness/internal/client"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// stubAttach installs a fake runAttachFn that records every call and returns
// attachErr, restoring the real cmdAttach on test cleanup.
func stubAttach(t *testing.T, attachErr error) *[]verbOpts {
	t.Helper()
	var calls []verbOpts
	orig := runAttachFn
	runAttachFn = func(o verbOpts) error {
		calls = append(calls, o)
		return attachErr
	}
	t.Cleanup(func() { runAttachFn = orig })
	return &calls
}

// stubTTY forces runStdoutIsTTY to report isTTY, restoring the real
// implementation on test cleanup.
func stubTTY(t *testing.T, isTTY bool) {
	t.Helper()
	orig := runStdoutIsTTY
	runStdoutIsTTY = func() bool { return isTTY }
	t.Cleanup(func() { runStdoutIsTTY = orig })
}

// sleeperDef is a scratchpad definition that idles rather than exiting
// immediately, so the daemon has something running to describe/remove.
func sleeperDef() protocol.ProjectHarness {
	return protocol.ProjectHarness{Harness: "generic", Args: []string{"-c", "sleep 60"}, Enabled: true}
}

// removeScratchpad tears a scratchpad down so its sleep doesn't outlive the
// test daemon shutdown path (same hygiene as TestCmdUpDownRoundTrip).
func removeScratchpad(t *testing.T, c *client.Client, name string) {
	t.Helper()
	if name == "" {
		return
	}
	if _, err := c.Remove(name); err != nil {
		t.Errorf("cleanup: remove %s: %v", name, err)
	}
}

// TestCmdRunAttachesByDefaultOnTTY is the core regression: on an interactive
// terminal, with neither --detach nor --json, `harness run` must attach to
// the scratchpad it just minted — not merely print its name and return.
func TestCmdRunAttachesByDefaultOnTTY(t *testing.T) {
	socket, _ := bootTestDaemon(t)
	c := dialTest(t, socket)
	stubTTY(t, true)
	calls := stubAttach(t, nil)

	o := verbOpts{socket: socket, name: "run-attach-default"}
	out, err := captureStdout(t, func() error {
		return cmdRun(c, o, sleeperDef(), false /* detach */)
	})
	if err != nil {
		t.Fatalf("cmdRun: %v", err)
	}
	defer func() {
		if len(*calls) > 0 {
			removeScratchpad(t, c, (*calls)[0].name)
		}
	}()

	if len(*calls) != 1 {
		t.Fatalf("runAttachFn called %d times, want 1 (auto-attach must be the default)", len(*calls))
	}
	got := (*calls)[0]
	if got.name == "" || got.name == o.name {
		t.Errorf("attach name = %q, want the daemon-minted name (not the pre-mint slug %q)", got.name, o.name)
	}
	if got.socket != socket {
		t.Errorf("attach socket = %q, want %q", got.socket, socket)
	}
	if !strings.Contains(out, got.name) {
		t.Errorf("stdout %q must still print the minted name before attaching", out)
	}
}

// TestCmdRunDetachSkipsAttach is the --detach opt-out: the pre-fix behavior
// (print the name, leave it running, never attach) must still be reachable.
func TestCmdRunDetachSkipsAttach(t *testing.T) {
	socket, _ := bootTestDaemon(t)
	c := dialTest(t, socket)
	stubTTY(t, true)
	calls := stubAttach(t, nil)

	o := verbOpts{socket: socket, name: "run-detach"}
	out, err := captureStdout(t, func() error {
		return cmdRun(c, o, sleeperDef(), true /* detach */)
	})
	if err != nil {
		t.Fatalf("cmdRun: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("runAttachFn called %d times, want 0 (--detach must skip attach)", len(*calls))
	}
	if !strings.Contains(out, "run-detach") {
		t.Errorf("stdout %q must still print the minted name", out)
	}

	hs, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	var found string
	for _, h := range hs {
		if strings.HasPrefix(h.Name, "run-detach") {
			found = h.Name
		}
	}
	if found == "" {
		t.Fatal("--detach must still register+start the scratchpad")
	}
	removeScratchpad(t, c, found)
}

// TestCmdRunNonTTYSkipsAttach: a piped/redirected stdout has no terminal to
// attach to, so `harness run` must fall back to print-and-exit even without
// --detach — the same behavior a script piping `harness run` already relies
// on.
func TestCmdRunNonTTYSkipsAttach(t *testing.T) {
	socket, _ := bootTestDaemon(t)
	c := dialTest(t, socket)
	stubTTY(t, false)
	calls := stubAttach(t, nil)

	o := verbOpts{socket: socket, name: "run-nontty"}
	_, err := captureStdout(t, func() error {
		return cmdRun(c, o, sleeperDef(), false /* detach */)
	})
	if err != nil {
		t.Fatalf("cmdRun: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("runAttachFn called %d times, want 0 (non-TTY stdout must skip attach)", len(*calls))
	}

	hs, err := c.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range hs {
		if strings.HasPrefix(h.Name, "run-nontty") {
			removeScratchpad(t, c, h.Name)
		}
	}
}

// TestCmdRunJSONSkipsAttach: --json is a machine-readable contract, not an
// interactive one, so it must never attach even on a TTY, and must still
// emit the full ScratchRunData payload.
func TestCmdRunJSONSkipsAttach(t *testing.T) {
	socket, _ := bootTestDaemon(t)
	c := dialTest(t, socket)
	stubTTY(t, true)
	calls := stubAttach(t, nil)

	o := verbOpts{socket: socket, name: "run-json", json: true}
	out, err := captureStdout(t, func() error {
		return cmdRun(c, o, sleeperDef(), false /* detach */)
	})
	if err != nil {
		t.Fatalf("cmdRun: %v", err)
	}
	if len(*calls) != 0 {
		t.Fatalf("runAttachFn called %d times, want 0 (--json must skip attach)", len(*calls))
	}
	var data protocol.ScratchRunData
	if err := json.Unmarshal([]byte(out), &data); err != nil {
		t.Fatalf("--json output did not decode as ScratchRunData: %v\noutput: %s", err, out)
	}
	if data.Name == "" {
		t.Error("--json output missing the minted name")
	}
	removeScratchpad(t, c, data.Name)
}

// TestCmdRunPropagatesAttachError: if the attach itself fails, `harness run`
// must surface that error rather than swallowing it — the scratchpad is
// already running (a successful ScratchRun happened first), so only the
// attach step's own failure is at stake.
func TestCmdRunPropagatesAttachError(t *testing.T) {
	socket, _ := bootTestDaemon(t)
	c := dialTest(t, socket)
	stubTTY(t, true)
	calls := stubAttach(t, errBoom)

	o := verbOpts{socket: socket, name: "run-attach-fails"}
	_, err := captureStdout(t, func() error {
		return cmdRun(c, o, sleeperDef(), false /* detach */)
	})
	if err == nil {
		t.Fatal("cmdRun = nil error, want the stubbed attach failure to propagate")
	}
	if len(*calls) != 1 {
		t.Fatalf("runAttachFn called %d times, want 1", len(*calls))
	}
	removeScratchpad(t, c, (*calls)[0].name)
}

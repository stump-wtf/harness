package main

// Command Harness Kind, End To End
//
// A project file declaring a resident `harness = "command"` goes through the
// real client (parse, wire) and the real daemon (re-validation, registration,
// spawn), and `describe` shows what runs: the kind and the argv as written.
//
// Governing: ADR-0023, SPEC-0017 REQ-14 "Project And Wire Front Doors",
// REQ-16 "Visibility".

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/protocol"
)

func TestUpAndDescribeCommandHarness(t *testing.T) {
	cliui.SetJSON(true)
	t.Cleanup(func() { cliui.SetJSON(false) })
	socket, _ := bootTestDaemon(t)
	chdir(t, writeProjectDir(t, `[project]
name = "proj"

[harness.ticker]
harness = "command"
argv = ["sleep", "60", "{{literal", "a b"]
`))
	// The template refusal is the point of the first up: argv[1:] cannot
	// carry "{{" until templates exist, and the client's own parse says so
	// before anything reaches the daemon.
	o := verbOpts{socket: socket, json: true}
	if _, err := captureStdout(t, func() error { return cmdUp(o) }); err == nil || !strings.Contains(err.Error(), `"argv[2]"`) {
		t.Fatalf("up with a \"{{\" in argv = %v, want the argv[2] refusal", err)
	}

	want := []string{"sleep", "60"}
	chdir(t, writeProjectDir(t, `[project]
name = "proj"

[harness.ticker]
harness = "command"
argv = ["sleep", "60"]
description = "resident command"
`))
	if _, err := captureStdout(t, func() error { return cmdUp(o) }); err != nil {
		t.Fatalf("up: %v", err)
	}
	c := dialTest(t, socket)
	waitForRunning(t, c, "proj/ticker")

	out, err := captureStdout(t, func() error {
		return withClient(verbOpts{socket: socket, json: true, name: "ticker"}, nil, projectScoped("describe", false, cmdDescribe))
	})
	if err != nil {
		t.Fatalf("describe --json: %v", err)
	}
	var h protocol.HarnessInfo
	if err := json.Unmarshal([]byte(out), &h); err != nil {
		t.Fatalf("describe output not JSON: %v\n%s", err, out)
	}
	if h.Adapter != "command" || !slices.Equal(h.Argv, want) || h.Args != nil {
		t.Errorf("describe = adapter %q argv %q args %q, want command %q and no args", h.Adapter, h.Argv, h.Args, want)
	}
	if h.PID <= 0 {
		t.Errorf("describe reports no pid for a running command harness: %+v", h)
	}

	cliui.SetJSON(false)
	out, err = captureStdout(t, func() error {
		return withClient(verbOpts{socket: socket, name: "ticker"}, nil, projectScoped("describe", false, cmdDescribe))
	})
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	for _, sub := range []string{"command", `["sleep", "60"]`} {
		if !strings.Contains(out, sub) {
			t.Errorf("describe does not show %q:\n%s", sub, out)
		}
	}
}

// TestUpAndDescribeCommandPromptDelivery: prompt_delivery travels the wire
// from a project file to the daemon, and describe shows it beside the prompt
// and the argv as written (SPEC-0017 REQ-12, REQ-14, REQ-16). A client that
// dropped the key would be refused by the daemon's own re-validation: the
// argv alone delivers nothing, so the prompt would be dropped.
func TestUpAndDescribeCommandPromptDelivery(t *testing.T) {
	cliui.SetJSON(true)
	t.Cleanup(func() { cliui.SetJSON(false) })
	socket, _ := bootTestDaemon(t)
	chdir(t, writeProjectDir(t, `[project]
name = "proj"

[harness.digest]
harness = "command"
argv = ["sleep", "60"]
prompt = "summarise the queue"
prompt_delivery = "stdin"
`))
	o := verbOpts{socket: socket, json: true}
	if _, err := captureStdout(t, func() error { return cmdUp(o) }); err != nil {
		t.Fatalf("up: %v", err)
	}

	out, err := captureStdout(t, func() error {
		return withClient(verbOpts{socket: socket, json: true, name: "digest"}, nil, projectScoped("describe", false, cmdDescribe))
	})
	if err != nil {
		t.Fatalf("describe --json: %v", err)
	}
	var h protocol.HarnessInfo
	if err := json.Unmarshal([]byte(out), &h); err != nil {
		t.Fatalf("describe output not JSON: %v\n%s", err, out)
	}
	if h.PromptDelivery != "stdin" || !slices.Equal(h.Argv, []string{"sleep", "60"}) || h.Prompt != "summarise the queue" {
		t.Errorf("describe = delivery %q argv %q prompt %q, want stdin, the argv and the prompt as written", h.PromptDelivery, h.Argv, h.Prompt)
	}

	cliui.SetJSON(false)
	out, err = captureStdout(t, func() error {
		return withClient(verbOpts{socket: socket, name: "digest"}, nil, projectScoped("describe", false, cmdDescribe))
	})
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	for _, sub := range []string{"prompt via stdin", `["sleep", "60"]`, "summarise the queue"} {
		if !strings.Contains(out, sub) {
			t.Errorf("describe does not show %q:\n%s", sub, out)
		}
	}
}

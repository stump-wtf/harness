package main

// Transcript Binding And The pi/omp Kinds, End To End
//
// A project file declaring a bound command harness and a pi harness goes
// through the real client (parse, wire) and the real daemon (re-validation,
// registration), and `describe` reports the binding. A field dropped anywhere
// on that path — the project parser, wireHarnesses, harnessFromWire, infoFor —
// leaves the harness unobserved with nothing else to show for it.
//
// Governing: ADR-0023, SPEC-0017 REQ-4 "Transcript Binding", REQ-13, REQ-14
// "Project And Wire Front Doors", REQ-16 "Visibility".

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/cliui"
	"github.com/stump-wtf/harness/internal/protocol"
)

func TestUpAndDescribeTranscriptBinding(t *testing.T) {
	cliui.SetJSON(true)
	t.Cleanup(func() { cliui.SetJSON(false) })
	socket, _ := bootTestDaemon(t)
	o := verbOpts{socket: socket, json: true}

	// A binding on an adapter kind is refused by the client's own parse.
	chdir(t, writeProjectDir(t, `[project]
name = "proj"

[harness.bad]
harness = "pi"
transcripts = "pi"
`))
	if _, err := captureStdout(t, func() error { return cmdUp(o) }); err == nil || !strings.Contains(err.Error(), `"transcripts" is only accepted`) {
		t.Fatalf("up with transcripts on pi = %v, want the refusal", err)
	}

	chdir(t, writeProjectDir(t, `[project]
name = "proj"

[harness.hand]
harness = "command"
argv = ["sleep", "60"]
transcripts = "claude-code"

[harness.pi-fix]
harness = "pi"
prompt = "fix the build"
enabled = false
`))
	if _, err := captureStdout(t, func() error { return cmdUp(o) }); err != nil {
		t.Fatalf("up: %v", err)
	}
	c := dialTest(t, socket)
	waitForRunning(t, c, "proj/hand")

	describe := func(name string) protocol.HarnessInfo {
		t.Helper()
		out, err := captureStdout(t, func() error {
			return withClient(verbOpts{socket: socket, json: true, name: name}, nil, projectScoped("describe", false, cmdDescribe))
		})
		if err != nil {
			t.Fatalf("describe %s --json: %v", name, err)
		}
		var h protocol.HarnessInfo
		if err := json.Unmarshal([]byte(out), &h); err != nil {
			t.Fatalf("describe output not JSON: %v\n%s", err, out)
		}
		return h
	}
	if h := describe("hand"); h.Adapter != "command" || h.Transcripts != "claude-code" {
		t.Errorf("describe hand = adapter %q transcripts %q, want command bound to claude-code", h.Adapter, h.Transcripts)
	}
	if h := describe("pi-fix"); h.Adapter != "pi" || h.Prompt != "fix the build" {
		t.Errorf("describe pi-fix = adapter %q prompt %q", h.Adapter, h.Prompt)
	}

	cliui.SetJSON(false)
	out, err := captureStdout(t, func() error {
		return withClient(verbOpts{socket: socket, name: "hand"}, nil, projectScoped("describe", false, cmdDescribe))
	})
	if err != nil {
		t.Fatalf("describe: %v", err)
	}
	if !strings.Contains(out, "transcripts") || !strings.Contains(out, "claude-code") {
		t.Errorf("describe does not show the binding:\n%s", out)
	}
}

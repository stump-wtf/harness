package main

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

func stampAt(h, m, s int) string {
	return time.Date(2026, 9, 11, h, m, s, 0, time.Local).Format(time.RFC3339Nano)
}

// postMortem is the shape of the failed stumpcloud-sweep-pdx run on tars
// (2026-09-11): a crush one-shot that read its prompt, worked, and died on a
// provider context-window error.
func postMortem() protocol.LogsData {
	code := 1
	return protocol.LogsData{
		Name:   "stumpcloud-sweep-pdx",
		Source: protocol.LogSourceAgentTrace,
		Run: &protocol.LogRun{
			Start: stampAt(7, 40, 0), End: stampAt(7, 56, 21), ExitCode: &code,
			Adapter: "crush", Workdir: "/home/agent/sweeps",
		},
		Entries: []protocol.LogEntry{
			{ID: "l1", Time: stampAt(7, 40, 0), Kind: protocol.LogEntryLifecycle, Action: "state", Summary: "stopped → starting"},
			{ID: "s", Time: stampAt(7, 40, 2), Kind: protocol.LogEntrySession, Action: "session", Summary: "e088ec4e · crush · litellm/Qwen3.8-27B"},
			{ID: "t0", Time: stampAt(7, 40, 5), Kind: protocol.LogEntryTool, Action: "read", Tool: "view", Target: "/home/agent/.config/dotfiles/stumpcloud-sweep.prompt.md", Summary: "view stumpcloud-sweep.prompt.md"},
			{ID: "t1", Time: stampAt(7, 41, 10), Kind: protocol.LogEntryTool, Action: "exec", Tool: "bash", Summary: "ssh nuc01\n docker ps", Error: true},
			{ID: "m0", Time: stampAt(7, 56, 20), Kind: protocol.LogEntryMark, Action: "error", Summary: "Bad Request: litellm.ContextWindowExceededError\x1b[31m"},
			{ID: "l2", Time: stampAt(7, 56, 21), Kind: protocol.LogEntryLifecycle, Action: "exited", Summary: "code=1", Error: true},
		},
	}
}

func TestRenderActivityPostMortem(t *testing.T) {
	var buf bytes.Buffer
	renderActivity(&buf, postMortem())
	out := buf.String()

	for _, want := range []string{
		"run 2026-09-11 07:40:00 → 07:56:21 · exit 1 · crush · /home/agent/sweeps\n",
		"07:40:00  state     stopped → starting\n",
		"07:40:02  session   e088ec4e · crush · litellm/Qwen3.8-27B\n",
		"07:40:05  read      /home/agent/.config/dotfiles/stumpcloud-sweep.prompt.md\n",
		"07:41:10  exec      ssh nuc01 docker ps  (failed)\n",
		"07:56:20  ERROR     Bad Request: litellm.ContextWindowExceededError[31m\n",
		"07:56:21  exited    code=1\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- got ---\n%s", want, out)
		}
	}
	if strings.ContainsRune(out, 0x1b) {
		t.Error("an escape byte from a transcript reached the terminal")
	}
}

// TestRenderActivityPrintsLogTextForGenericHarness: a reply that is not the
// activity view (generic adapter, or a pre-ProtoMinor-7 daemon) prints exactly
// as --raw does.
func TestRenderActivityPrintsLogTextForGenericHarness(t *testing.T) {
	var buf bytes.Buffer
	renderActivity(&buf, protocol.LogsData{Name: "ticker", Text: "tick 1\ntick 2"})
	if got := buf.String(); got != "tick 1\ntick 2\n" {
		t.Errorf("output = %q, want the log text verbatim with a trailing newline", got)
	}
}

// TestRenderActivityShowsNoticesWithoutAgentActivity: with nothing
// attributable the view is the run header and the notices, and NOT the durable
// log — whose tail, for a full-screen agent, is a screenshot of its idle TUI
// (#279). It holds even if a daemon sends Text anyway.
func TestRenderActivityShowsNoticesWithoutAgentActivity(t *testing.T) {
	ld := protocol.LogsData{
		Source:  protocol.LogSourceAgentTrace,
		Run:     &protocol.LogRun{Start: stampAt(12, 6, 39), Adapter: "crush", Workdir: "/home/agent/src"},
		Notices: []string{"2 session(s) in this run's window not shown"},
		Text:    "  ╱╱╱╱╱╱ ▄▀▀▀▀ █▀▀▀▄\nThanks for using Crush!\n",
	}
	var buf bytes.Buffer
	renderActivity(&buf, ld)
	out := buf.String()
	for _, want := range []string{"→ running", "note      2 session(s)"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\n--- got ---\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"durable log tail:", "Thanks for using Crush!", "▄▀▀▀▀"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("activity view printed the durable log (%q)\n--- got ---\n%s", unwanted, out)
		}
	}
}

func TestFormatEntryFlagsAmbiguousSessions(t *testing.T) {
	got := formatEntry(protocol.LogEntry{Time: stampAt(9, 0, 0), Kind: protocol.LogEntryTool, Action: "exec", Summary: "gh pr list", Ambiguous: true})
	if !strings.Contains(got, "?exec") {
		t.Errorf("entry = %q, want the ambiguous marker on the label", got)
	}
}

// TestFollowActivityPrintsEachEntryOnce: follow re-fetches overlapping windows
// and must print each entry exactly once, and a new run's header when the
// harness starts again.
func TestFollowActivityPrintsEachEntryOnce(t *testing.T) {
	first := postMortem()
	first.Entries = first.Entries[:2]
	second := postMortem()
	nextRun := protocol.LogsData{
		Source: protocol.LogSourceAgentTrace,
		Run:    &protocol.LogRun{Start: stampAt(9, 30, 0), Adapter: "crush"},
		Entries: []protocol.LogEntry{
			{ID: "l9", Time: stampAt(9, 30, 0), Kind: protocol.LogEntryLifecycle, Action: "state", Summary: "stopped → starting"},
		},
	}

	polls := []protocol.LogsData{second, nextRun}
	var buf bytes.Buffer
	var asked []int
	err := followActivity(&buf, first, func(lines int) (protocol.LogsData, error) {
		asked = append(asked, lines)
		ld := polls[0]
		polls = polls[1:]
		return ld, nil
	}, 50, func() bool { return len(polls) > 0 })
	if err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if n := strings.Count(out, "07:40:02  session"); n != 1 {
		t.Errorf("session entry printed %d times, want 1\n%s", n, out)
	}
	if n := strings.Count(out, "07:56:20  ERROR"); n != 1 {
		t.Errorf("error entry printed %d times, want 1\n%s", n, out)
	}
	if n := strings.Count(out, "run 2026-09-11 07:40:00"); n != 1 {
		t.Errorf("first run header printed %d times, want 1\n%s", n, out)
	}
	if !strings.Contains(out, "run 2026-09-11 09:30:00 → running") {
		t.Errorf("a new run did not print its own header\n%s", out)
	}
	if len(asked) != 2 || asked[0] != 200 {
		t.Errorf("re-fetch line budgets = %v, want [200 200]", asked)
	}
}

func TestInert(t *testing.T) {
	if got := inert("a\tb\n\x1b]0;title\x07cd"); got != "a b ]0;titlecd" {
		t.Errorf("inert = %q", got)
	}
}

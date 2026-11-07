package daemon

// Pipe-Run Streams In The Logs View — Tests
//
// Governing: ADR-0033 "Structured one-shots run on pipes", "What reads the
// records"; SPEC-0017 REQ-18; SPEC-0008 REQ "Per-Run Logs".

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/logview"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/testwait"
)

// pipeOneShot is a scheduled claude-code prompt one-shot; with a fake
// `claude` first on PATH it runs on pipes exactly as a real one would.
func pipeOneShot(name, workdir string) core.Harness {
	h := scheduledSh(name, "", workdir)
	h.Adapter, h.Args, h.Prompt = "claude-code", nil, "triage the queue"
	return h
}

// fakeClaude puts a `claude` running the sh script body first on PATH.
func fakeClaude(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "claude"), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// `harness logs NAME --run N --raw` on a finished pipe run shows the
// stream's parsed entries — the session, each tool call, the run's result —
// and then the run log with the agent's stderr, each under a header; the
// durable log's raw tail says where the stdout went.
func TestLogsRunShowsAPipeRunsStream(t *testing.T) {
	dir := t.TempDir()
	const sid = "01234567-89ab-cdef-0123-456789abcdef"
	// A one-shot that reads a file and lists pulls with a credential on the
	// command line, recording the stream a real claude would.
	fakeClaude(t, `echo '{"type":"system","subtype":"init","cwd":"`+dir+`","session_id":"`+sid+`"}'
echo 'warning: from stderr' >&2
echo '{"type":"assistant","message":{"role":"assistant","model":"claude-test-9","content":[{"type":"tool_use","id":"toolu_1","name":"Read","input":{"file_path":"`+dir+`/note.txt"}}]},"session_id":"`+sid+`"}'
echo '{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_1","type":"tool_result","content":"hello"}]},"session_id":"`+sid+`"}'
echo '{"type":"assistant","message":{"role":"assistant","model":"claude-test-9","content":[{"type":"tool_use","id":"toolu_2","name":"Bash","input":{"command":"GITEA_TOKEN=abc123 tea pr ls"}}]},"session_id":"`+sid+`"}'
echo '{"type":"user","message":{"role":"user","content":[{"tool_use_id":"toolu_2","type":"tool_result","content":"(none)"}]},"session_id":"`+sid+`"}'
echo '{"type":"result","subtype":"success","is_error":false,"num_turns":3,"duration_ms":1500,"total_cost_usd":0.05,"result":"triaged the queue","session_id":"`+sid+`"}'
`)
	td, _, _ := newJobsDaemon(t, pipeOneShot("sweep", dir))
	c := td.dial(t, nil)
	if _, err := c.Trigger("sweep"); err != nil {
		t.Fatal(err)
	}
	waitRunsOver(t, c, "sweep", finishedN(1))

	ld, err := c.RunLogs("sweep", 1, 100)
	if err != nil {
		t.Fatal(err)
	}
	text := ld.Text
	iStream := strings.Index(text, ".stream.jsonl <==")
	iSess := strings.Index(text, "01234567")
	iRead := strings.Index(text, "note.txt")
	iBash := strings.Index(text, "GITEA_TOKEN=[REDACTED] tea pr ls")
	iResult := strings.Index(text, "success · 3 turns")
	iFinal := strings.Index(text, "triaged the queue")
	iLog := strings.Index(text, "/1.log <==")
	iWarn := strings.Index(text, "warning: from stderr")
	if iStream < 0 || iSess <= iStream || iRead < iSess || iBash < iRead || iResult < iBash || iFinal < iResult || iLog < iFinal || iWarn < iLog {
		t.Fatalf("want the stream's entries (session, read, bash, result) then the run log (stderr), each under a header:\n%s", text)
	}
	if !strings.Contains(text, "claude-test-9") {
		t.Errorf("the session entry does not name the run's model:\n%s", text)
	}
	if strings.Contains(text, "abc123") {
		t.Errorf("the stream's credential is not masked:\n%s", text)
	}
	if strings.Contains(text, `{"type":"`) {
		t.Errorf("the raw view shows the stream's JSON instead of its entries:\n%s", text)
	}

	raw, err := c.Logs("sweep", 100)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw.Text, `"type":"system"`) {
		t.Errorf("the durable log carries stdout:\n%s", raw.Text)
	}
	if len(raw.Notices) == 0 || !strings.Contains(raw.Notices[0], "--run N --raw") {
		t.Errorf("the durable log's reply does not say where stdout went: %q", raw.Notices)
	}
}

// While a pipe run is live its text is the stream's entries alone, so a
// `trigger --wait` that prints each poll's new suffix sees the stream grow,
// then the run log appended once, at the end: every poll's text extends the
// last.
func TestLogsRunPipeTextOnlyGrows(t *testing.T) {
	dir := t.TempDir()
	gate := filepath.Join(dir, "go")
	const sid = "fedcba98-7654-3210-fedc-ba9876543210"
	fakeClaude(t, `echo '{"type":"system","subtype":"init","session_id":"`+sid+`"}'
echo 'early stderr' >&2
while [ ! -e '`+gate+`' ]; do sleep 0.02; done
echo '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"result":"done","session_id":"`+sid+`"}'
`)
	td, _, _ := newJobsDaemon(t, pipeOneShot("sweep", dir))
	c := td.dial(t, nil)
	if _, err := c.Trigger("sweep"); err != nil {
		t.Fatal(err)
	}
	var live string
	deadline := time.Now().Add(testwait.Budget(t, 5*time.Second))
	// Capture the live text only once it is stable: a read that lands
	// mid-write of the stream sees the raw tail, and neither a raw fallback
	// nor a still-growing stream is a prefix of the finished text.
	for {
		if time.Now().After(deadline) {
			t.Fatalf("the live run's stream never settled:\n%s", live)
		}
		ld, err := c.RunLogs("sweep", 1, 1<<20)
		if err != nil {
			t.Fatal(err)
		}
		next := ld.Text
		time.Sleep(20 * time.Millisecond)
		if next == live && strings.Contains(next, "fedcba98") && !strings.Contains(next, `{"type":"`) {
			live = next
			break
		}
		live = next
	}
	if strings.Contains(live, "early stderr") {
		t.Errorf("a live run's text carries its run log, which would not stay a prefix:\n%s", live)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitRunsOver(t, c, "sweep", finishedN(1))
	ld, err := c.RunLogs("sweep", 1, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(ld.Text, live) || !strings.Contains(ld.Text[len(live):], "early stderr") {
		t.Errorf("the finished text does not extend the live text with the run log:\nlive:\n%s\nfinished:\n%s", live, ld.Text)
	}
}

// A recorded stream read through the renderer a pipe run's logs view uses
// comes out the way a traced run's session renders: the session, each call
// with its target, the result with its numbers; the credential on the
// command line masked by the reader, as ADR-0008 requires.
func TestPipeStreamEntriesRender(t *testing.T) {
	h := core.Harness{Adapter: "claude-code"}
	start := time.Date(2026, 9, 25, 6, 30, 0, 0, time.UTC)
	entries, ok := pipeStreamEntries(context.Background(), h, filepath.Join("testdata", "pipe-run.stream.jsonl"), start)
	if !ok {
		t.Fatal("the fixture's stream parsed to nothing")
	}
	if len(entries) != 5 {
		t.Fatalf("got %d entries, want the session, the two calls, the turn-end and the result: %+v", len(entries), entries)
	}
	sess, read, bash, turnEnd, result := entries[0], entries[1], entries[2], entries[3], entries[4]
	if sess.Kind != protocol.LogEntrySession || sess.Action != "session" || sess.Session != "5f0c2a4e-0000-4000-8000-000000000132" {
		t.Errorf("session entry = %+v", sess)
	}
	if !strings.Contains(sess.Summary, "claude-sonnet-4-5-20250929") {
		t.Errorf("the session entry does not name the model: %q", sess.Summary)
	}
	if read.Kind != protocol.LogEntryTool || read.Action != "read" || read.Target != "note.txt" {
		t.Errorf("read entry = %+v", read)
	}
	if bash.Tool != "Bash" || !strings.Contains(bash.Summary, "GITEA_TOKEN=[REDACTED]") || strings.Contains(bash.Summary, "abc123") {
		t.Errorf("bash entry = %+v", bash)
	}
	if turnEnd.Kind != protocol.LogEntryMark || turnEnd.Action != "turn-end" {
		t.Errorf("turn-end mark = %+v", turnEnd)
	}
	if result.Kind != protocol.LogEntryMark || result.Action != "result" || result.Error {
		t.Errorf("result entry = %+v", result)
	}
	if !strings.Contains(result.Summary, "success · 3 turns") || !strings.Contains(result.Summary, "only open pull") {
		t.Errorf("result summary = %q", result.Summary)
	}
	var b strings.Builder
	for _, e := range entries {
		b.WriteString(logview.FormatEntry(e))
		b.WriteString("\n")
	}
	if strings.Contains(b.String(), `{"type":"`) {
		t.Errorf("the rendering carries the stream's JSON:\n%s", b.String())
	}
}

// Attaching over the wire to a stream-json one-shot gets its lines as CRLF
// lines (the daemon picks the line mux from the definition before the first
// run), and describe lists the session with no viewport.
func TestAttachToAPipeRunOverTheWire(t *testing.T) {
	dir := t.TempDir()
	gate := filepath.Join(dir, "go")
	fakeClaude(t, `echo '{"type":"system","subtype":"init"}'
echo 'warning: from stderr' >&2
while [ ! -e '`+gate+`' ]; do sleep 0.02; done
echo '{"type":"result","subtype":"success"}'
`)
	td, _, _ := newJobsDaemon(t, pipeOneShot("sweep", dir))
	att := td.dial(t, nil)
	if err := att.AttachOpen(1, "sweep", 120, 40, protocol.AttachRO); err != nil {
		t.Fatal(err)
	}
	ctl := td.dial(t, nil)
	if _, err := ctl.Trigger("sweep"); err != nil {
		t.Fatal(err)
	}
	var got strings.Builder
	pc := att.Conn()
	_ = att.SetReadDeadline(time.Now().Add(testwait.Budget(t, 10*time.Second)))
	for !strings.Contains(got.String(), "warning: from stderr\r\n") || !strings.Contains(got.String(), "{\"type\":\"system\",\"subtype\":\"init\"}\r\n") {
		f, err := pc.ReadFrame()
		if err != nil {
			t.Fatalf("reading attach frames (got %q): %v", got.String(), err)
		}
		switch f.Type {
		case protocol.TypeAttachData:
			if _, data, err := protocol.DecodeAttach(f.Payload); err == nil {
				got.Write(data)
			}
		case protocol.TypePing:
			_ = pc.WriteFrame(protocol.TypePong, nil)
		}
	}
	info, err := ctl.Describe("sweep")
	if err != nil {
		t.Fatal(err)
	}
	if info.AttachViewport != "" || len(info.AttachSessions) != 1 {
		t.Errorf("describe: viewport %q, sessions %+v; want no viewport and the one session", info.AttachViewport, info.AttachSessions)
	}
	if err := os.WriteFile(gate, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitRunsOver(t, ctl, "sweep", finishedN(1))
}

// streamTail bounds what it returns: n lines, each cut at lineMax with a note,
// the oldest dropped past budget.
func TestStreamTailBounds(t *testing.T) {
	in := "a\n" + strings.Repeat("b", 100) + "\nc\nd"
	got := streamTail(strings.NewReader(in), 3, 10, 1<<20)
	want := []string{strings.Repeat("b", 10) + " …[90 more bytes; the whole line is in the stream file]", "c", "d"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("streamTail = %q, want %q", got, want)
	}
	got = streamTail(strings.NewReader("one\ntwo\nthree\n"), 0, 100, 9)
	if strings.Join(got, "|") != "two|three" {
		t.Errorf("budgeted tail = %q, want the newest lines within 9 bytes", got)
	}
}

package adapter

import (
	"bytes"
	"strconv"
	"strings"
	"testing"
)

// The oracle for these shapes is Claude Code's own stream-json output, as
// documented in issue #13: heartbeats arrive as tool_progress/task_progress
// events carrying heartbeat:true, assistant messages carry text and tool_use
// content blocks, and the run ends with a result object.

func renderAll(t *testing.T, chunks ...string) string {
	t.Helper()
	r := &streamJSONRenderer{}
	var out bytes.Buffer
	for _, c := range chunks {
		out.Write(r.FormatPTY([]byte(c)))
	}
	return out.String()
}

func TestStreamJSONHeartbeatsCollapseToOneLine(t *testing.T) {
	hb := func(n int, elapsed float64) string {
		return `{"type":"tool_progress","tool_use_id":"u-` + strconv.Itoa(n) + `","tool_name":"Agent","heartbeat":true,"elapsed_time_seconds":` +
			strconv.FormatFloat(elapsed, 'f', -1, 64) + `}` + "\n"
	}

	got := renderAll(t, hb(1, 30), hb(2, 60), hb(3, 90))
	if strings.Count(got, "heartbeat") != 3 {
		t.Fatalf("each ping leaked its own line, not one live tally:\n%q", got)
	}
	if !strings.HasPrefix(got, "[1 heartbeat over 30s]") {
		t.Errorf("first ping did not paint the tally:\n%q", got)
	}
	// Second and third pings rewrite in place: \r then the new text (the
	// tally only ever grows here, so no padding is needed).
	want2 := "\r[2 heartbeats over 1m00s]"
	if !strings.Contains(got, want2) {
		t.Errorf("second ping did not overwrite the tally:\nwant %q in\n%q", want2, got)
	}
	want3 := "\r[3 heartbeats over 1m30s]"
	if !strings.Contains(got, want3) {
		t.Errorf("third ping did not overwrite the tally:\nwant %q in\n%q", want3, got)
	}
	if strings.HasSuffix(got, "\n") {
		t.Errorf("the tally line must hold the cursor (no trailing newline) while the run is live:\n%q", got)
	}
}

func TestStreamJSONTallyClosesBeforeNextContent(t *testing.T) {
	got := renderAll(t,
		`{"type":"tool_progress","heartbeat":true,"elapsed_time_seconds":30}`+"\n",
		`{"type":"tool_progress","heartbeat":true,"elapsed_time_seconds":60}`+"\n",
		`{"type":"assistant","message":{"content":[{"type":"text","text":"Done."}]}}`+"\n",
	)
	want := "[1 heartbeat over 30s]\r[2 heartbeats over 1m00s]\nDone.\n"
	if got != want {
		t.Errorf("tally did not close cleanly before the next line:\n got %q\nwant %q", got, want)
	}
	// A fresh run starts a fresh tally at [1 heartbeat], not a resumed one.
	got = renderAll(t, got,
		`{"type":"tool_progress","heartbeat":true,"elapsed_time_seconds":10}`+"\n",
	)
	if !strings.Contains(got, "\n[1 heartbeat over 10s]") {
		t.Errorf("a second run did not start its own tally:\n%q", got)
	}
}

func TestStreamJSONToolUseLeadsWithDescription(t *testing.T) {
	got := renderAll(t, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"git push origin main","description":"Push branch to GitHub"}}]}}`+"\n")
	if got != "▸ Push branch to GitHub\n" {
		t.Errorf("tool_use did not lead with its description:\n%q", got)
	}
}

func TestStreamJSONToolUseFallsBackToName(t *testing.T) {
	got := renderAll(t, `{"type":"assistant","message":{"content":[{"type":"tool_use","id":"t1","name":"Agent","input":{"prompt":"do things"}}]}}`+"\n")
	if got != "▸ Agent\n" {
		t.Errorf("tool_use without a description did not fall back to the tool name:\n%q", got)
	}
}

func TestStreamJSONTextBlocksPlain(t *testing.T) {
	got := renderAll(t, `{"type":"assistant","message":{"content":[{"type":"text","text":"Looking at the failing test now.\n"},{"type":"text","text":"Second thought."}]}}`+"\n")
	want := "Looking at the failing test now.\nSecond thought.\n"
	if got != want {
		t.Errorf("text blocks did not render as plain text:\n got %q\nwant %q", got, want)
	}
}

func TestStreamJSONResultSummary(t *testing.T) {
	got := renderAll(t, `{"type":"result","subtype":"success","is_error":false,"duration_ms":41230,"num_turns":12,"total_cost_usd":0.41}`+"\n")
	want := "✓ result · success · 12 turns · 41s · $0.41\n"
	if got != want {
		t.Errorf("result line:\n got %q\nwant %q", got, want)
	}
	got = renderAll(t, `{"type":"result","subtype":"error_max_turns","is_error":true}`+"\n")
	if !strings.HasPrefix(got, "✖ result · error_max_turns") {
		t.Errorf("failed result line:\n%q", got)
	}
}

func TestStreamJSONNoiseSkipped(t *testing.T) {
	got := renderAll(t,
		`{"type":"system","subtype":"init","model":"claude-opus-5"}`+"\n",
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"..."}]}}`+"\n",
		`{"type":"stream_event","event":{"type":"content_block_delta"}}`+"\n",
	)
	if got != "" {
		t.Errorf("noise events leaked into the preview:\n%q", got)
	}
}

func TestStreamJSONNonJSONPassthrough(t *testing.T) {
	raw := "bash: line 1: foo: command not found\n\x1b[31mred escape output\x1b[0m\n"
	if got := renderAll(t, raw); got != raw {
		t.Errorf("non-JSON terminal output was not passed through byte-for-byte:\n got %q\nwant %q", got, raw)
	}
}

func TestStreamJSONUnparseableJSONPassthrough(t *testing.T) {
	raw := "{not json at all\n"
	if got := renderAll(t, raw); got != raw {
		t.Errorf("unparseable JSON was not passed through:\n got %q\nwant %q", got, raw)
	}
}

func TestStreamJSONUnknownEventKindPassthrough(t *testing.T) {
	raw := `{"type":"control_request","request_id":"x","request":{"subtype":"can_use_tool"}}` + "\n"
	if got := renderAll(t, raw); got != raw {
		t.Errorf("unmodeled event kind was not passed through:\n got %q\nwant %q", got, raw)
	}
}

func TestStreamJSONLineSplitAcrossChunks(t *testing.T) {
	line := `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"description":"Split across reads"}}]}}`
	got := renderAll(t, line[:20], line[20:60], line[60:]+"\n")
	if got != "▸ Split across reads\n" {
		t.Errorf("a line split across chunks did not render once:\n%q", got)
	}
}

func TestStreamJSONCRLEFPassthrough(t *testing.T) {
	raw := "{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"hi\"}]}}\r\n"
	if got := renderAll(t, raw); got != "hi\n" {
		t.Errorf("CRLF line was not normalized:\n%q", got)
	}
}

func TestStreamJSONBufferCapFlushesVerbatim(t *testing.T) {
	r := &streamJSONRenderer{}
	big := strings.Repeat("x", streamJSONLineCap+16) // no newline anywhere
	got := r.FormatPTY([]byte(big))
	if string(got) != big {
		t.Errorf("oversized newline-less chunk was not flushed verbatim: %d bytes in, %d out", len(big), len(got))
	}
	if len(r.buf) != 0 {
		t.Errorf("line buffer did not reset after the cap flush: %d bytes held", len(r.buf))
	}
}

func TestStreamJSONTallySurvivesSplitHeartbeatLine(t *testing.T) {
	// A heartbeat whose JSON arrives in two reads still tallies once.
	got := renderAll(t,
		`{"type":"tool_progress","heartbeat":true,"elap`,
		`sed_time_seconds":45}`+"\n",
	)
	if !strings.HasPrefix(got, "[1 heartbeat over 45s]") {
		t.Errorf("split heartbeat line did not paint the tally:\n%q", got)
	}
}

func TestPeekFormatterFor(t *testing.T) {
	if PeekFormatterFor("claude-code") == nil {
		t.Error("claude-code must provide a peek formatter (issue #13)")
	}
	for _, name := range []string{"crush", "codex", "generic", "command", "no-such-adapter"} {
		if PeekFormatterFor(name) != nil {
			t.Errorf("%q must keep the preview byte-faithful (no formatter), got one", name)
		}
	}
}

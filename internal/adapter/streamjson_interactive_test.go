package adapter

// The interactive case the preview formatter was never written for (issue
// #951). A resident `claude` session is a full-screen Ink app, so what reaches
// the formatter is terminal repaints rather than one JSON object per line. Ink
// repaints with *relative* cursor moves that count rows — blank rows included
// — and ends a frame with a cursor-parking tail that has no newline after it.
// A formatter that drops blank rows or holds bytes until the next newline hands
// its emulator a frame shorter than Ink believes it drew, so the next
// "\x1b[5A" lands above where the app means and the redraw overwrites rows it
// never intended to touch.
//
// Only a line that begins with '{' can be a stream-json event, so only such a
// line may be buffered. Everything else is the guest's own terminal output and
// must reach the emulator unchanged, as it arrives.
//
// Governing: ADR-0021 (payloads are opaque), ADR-0011 (agent adapters),
// SPEC-0001 REQ "Dashboard" (live preview).

import (
	"strings"
	"testing"
)

const (
	inkSyncOn  = "\x1b[?2026h"
	inkSyncOff = "\x1b[?2026l"
)

// inkFrame paints Claude Code's folder-trust dialog the way Ink does: eight
// rows, two of them blank, then the cursor-parking tail with no newline after
// it. cursorRow is the row the selection marker sits on.
func inkFrame(cursorRow int) string {
	row := func(n int, text string) string {
		mark := " "
		if n == cursorRow {
			mark = "❯"
		}
		return mark + text + "\r\n"
	}
	var b strings.Builder
	b.WriteString(inkSyncOn)
	b.WriteString("\x1b[H")
	b.WriteString(row(0, " Quick safety check: Is this a project you created or"))
	b.WriteString(row(1, " well-known open source project, or work from your team"))
	b.WriteString("\r\n")
	b.WriteString(row(3, " Yes, I trust this folder"))
	b.WriteString(row(4, " No, exit"))
	b.WriteString("\r\n")
	b.WriteString(row(6, " Claude Code will be able to read, edit, and execute files here."))
	b.WriteString(row(7, " Security guide"))
	b.WriteString(inkSyncOff)
	b.WriteString("\x1b[1C\x1b[3A")
	return b.String()
}

// inkSelectionMoved is Ink's repaint after a Down keypress: three rows up from
// the parked cursor — blank rows counted — to the blank row above the options,
// then those rows rewritten with the marker one row lower.
func inkSelectionMoved() string {
	return inkSyncOn + "\x1b[3A" +
		"\r\n" +
		"   Yes, I trust this folder\r\n" +
		" ❯ No, exit\r\n" +
		inkSyncOff +
		"\x1b[1C\x1b[1A"
}

// inkSession is what a resident claude-code session sends across two reads:
// the first frame, then the repaint after a keypress.
func inkSession() string {
	return inkFrame(3) + inkSelectionMoved()
}

// TestStreamJSONInteractiveBytesPassThroughUnchanged is the whole of #951 in
// one assertion: fed a full-screen Ink app's bytes, the formatter emits exactly
// the bytes it was given — blank rows, relative cursor moves, synchronized
// output markers and the newline-less cursor-parking tail all included.
func TestStreamJSONInteractiveBytesPassThroughUnchanged(t *testing.T) {
	in := inkSession()
	r := &streamJSONRenderer{}
	got := string(r.FormatPTY([]byte(in)))
	if got != in {
		t.Errorf("the formatter reshaped a resident session's bytes.\n got %q\nwant %q", got, in)
	}
}

// TestStreamJSONInteractiveBytesUnchangedAtEverySplit: the daemon reads the
// guest's PTY in whatever pieces the kernel hands it, so byte-faithfulness
// cannot depend on where a read lands. Every two-way split of the session's
// bytes must produce the same bytes out — nothing withheld until a later
// newline, nothing duplicated at the seam.
func TestStreamJSONInteractiveBytesUnchangedAtEverySplit(t *testing.T) {
	in := inkSession()
	for i := 0; i <= len(in); i++ {
		r := &streamJSONRenderer{}
		got := string(r.FormatPTY([]byte(in[:i]))) + string(r.FormatPTY([]byte(in[i:])))
		if got != in {
			t.Fatalf("split at %d changed the bytes.\n got %q\nwant %q", i, got, in)
		}
	}
}

// TestStreamJSONBlankLinesSurvive names the specific reshape that corrupted
// the attached view: a blank row was the one thing the formatter refused to
// forward, and every relative move Ink makes counts those rows.
func TestStreamJSONBlankLinesSurvive(t *testing.T) {
	in := "one\r\n\r\n\r\nfour\r\n"
	if got := renderAll(t, in); got != in {
		t.Errorf("blank lines were dropped:\n got %q\nwant %q", got, in)
	}
	if got := renderAll(t, "\r\n", "\r\n", "text\r\n"); got != "\r\n\r\ntext\r\n" {
		t.Errorf("blank lines split across reads were dropped:\n%q", got)
	}
}

// TestStreamJSONTerminalOutputIsNotHeldForANewline: Ink's cursor-parking tail
// has no newline after it. Holding it until some later write means the cursor
// stays where the previous frame left it, which is the other half of the
// corrupted screen.
func TestStreamJSONTerminalOutputIsNotHeldForANewline(t *testing.T) {
	r := &streamJSONRenderer{}
	tail := "painted\r\n\x1b[1C\x1b[3A"
	if got := string(r.FormatPTY([]byte(tail))); got != tail {
		t.Errorf("a newline-less tail was withheld until the next newline:\n%q", got)
	}
}

// A JSON-looking prefix that turns out to carry an escape sequence is terminal
// output wearing a costume: the bytes already buffered, and the rest of the
// line, must still reach the emulator.
func TestStreamJSONEscapeReleasesTheBufferedLine(t *testing.T) {
	r := &streamJSONRenderer{}
	first := "{not json at all \x1b[31mred"
	got := string(r.FormatPTY([]byte(first)))
	if got != "{not json at all \x1b[31mred" {
		t.Errorf("an escape mid-line did not release the buffer:\n%q", got)
	}
	tail := " still the same line\x1b[0m\r\nnext\r\n"
	if got := string(r.FormatPTY([]byte(tail))); got != tail {
		t.Errorf("the rest of the released line was not forwarded:\n%q", got)
	}
}

// An event line that runs past the cap is not an event either: it and the rest
// of its line pass through rather than being held or half-rendered.
func TestStreamJSONCapReleasesTheRestOfTheLine(t *testing.T) {
	r := &streamJSONRenderer{}
	head := "{" + strings.Repeat("x", streamJSONLineCap)
	if got := string(r.FormatPTY([]byte(head))); got != head {
		t.Fatalf("the cap did not release the buffered bytes verbatim: %d in, %d out", len(head), len(got))
	}
	tail := strings.Repeat("y", 100) + "\r\n"
	if got := string(r.FormatPTY([]byte(tail))); got != tail {
		t.Errorf("the rest of the over-long line was not forwarded:\n%q", got)
	}
}

// The one-shot renderer must keep working: a stream-json run is still one
// object per line, and its events still collapse and render.
func TestStreamJSONStreamOneShotStillRenders(t *testing.T) {
	got := renderAll(t,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"description":"Push the branch"}}]}}`+"\r\n",
		`{"type":"tool_progress","heartbeat":true,"elapsed_time_seconds":30}`+"\r\n",
		`{"type":"result","subtype":"success","is_error":false,"duration_ms":1000,"num_turns":1,"total_cost_usd":0.01}`+"\r\n",
	)
	want := "▸ Push the branch\r\n[1 heartbeat over 30s]\r\n✓ result · success · 1 turns · 1s · $0.01\r\n"
	if got != want {
		t.Errorf("one-shot stream-json rendering changed:\n got %q\nwant %q", got, want)
	}
}

// An escape that releases a buffered line releases the rest of that line, not
// the rest of the chunk: once the released line's newline has passed, a
// well-formed event in the same chunk renders, and the next chunk is not still
// raw. Both edges depend on where the kernel split the read, which is exactly
// what byte-faithfulness may not do.
func TestStreamJSONEscapeReleaseDoesNotSwallowFollowingLines(t *testing.T) {
	event := `{"type":"result","subtype":"success","is_error":false,"duration_ms":1000,"num_turns":1,"total_cost_usd":0.01}`
	rendered := "✓ result · success · 1 turns · 1s · $0.01\r\n"

	r := &streamJSONRenderer{}
	got := string(r.FormatPTY([]byte("{not json \x1b[31mred\r\n" + event + "\r\n")))
	want := "{not json \x1b[31mred\r\n" + rendered
	if got != want {
		t.Errorf("lines after an ESC release were mirrored instead of rendered:\n got %q\nwant %q", got, want)
	}

	r = &streamJSONRenderer{}
	got = string(r.FormatPTY([]byte("{not json \x1b[31mred\r\nnext line\r\n"))) +
		string(r.FormatPTY([]byte(event+"\r\n")))
	want = "{not json \x1b[31mred\r\nnext line\r\n" + rendered
	if got != want {
		t.Errorf("the renderer was still raw after the released line ended:\n got %q\nwant %q", got, want)
	}
}

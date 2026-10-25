package tui

// Coverage for issue #825: attached mode renders a claude-code harness's
// stream-json through the same PeekFormatter the dashboard's preview uses,
// ^b f flips back to the byte-faithful mirror, and the status bar names the
// mode. The transformation itself is adapter-side
// (internal/adapter/streamjson.go); these tests prove the attach wiring: who
// gets a formatter, that its output reaches the emulator, and that the
// toggle moves both ways.

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// attachModelWithAdapter is attachedFake with the first harness's adapter
// named, at a width that keeps one stream-json line un-wrapped so substring
// assertions can see it whole.
func attachModelWithAdapter(t *testing.T, adapterName string) (*Model, *fakeAttach) {
	t.Helper()
	m, fa := attachedFake(200, 50)
	m.harnesses[0].Adapter = adapterName
	return m, fa
}

// toolCall builds one stream-json line carrying a tool call with a
// human-written description — the line a human is reading attach for. Each
// call needs its own description: the emulator keeps what it has already
// painted, so a toggle test must be able to tell a new frame's bytes from an
// older frame's.
func toolCall(desc string) []byte {
	return []byte(`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Bash","input":{"command":"git push","description":"` + desc + `"}}]}}` + "\n")
}

func TestAttachFormatsClaudeCodeStreamJSON(t *testing.T) {
	m, _ := attachModelWithAdapter(t, "claude-code")
	drain(m.attachTo(m.harnesses[0], 0))
	if m.att.fmt == nil {
		t.Fatal("attaching to a claude-code harness opened without a formatter")
	}

	m.Update(attachDataMsg{sessionID: m.att.sessionID, data: toolCall("Push branch to GitHub")})
	got := m.att.view.render()
	if !strings.Contains(got, "Push branch to GitHub") {
		t.Errorf("the tool call's description did not reach the attached view:\n%s", got)
	}
	if strings.Contains(got, `"type":"tool_use"`) {
		t.Errorf("attached view shows raw stream-json:\n%s", got)
	}
}

func TestAttachOtherAdaptersStayByteFaithful(t *testing.T) {
	for _, name := range []string{"", "crush", "generic"} {
		m, _ := attachModelWithAdapter(t, name)
		drain(m.attachTo(m.harnesses[0], 0))
		if m.att.fmt != nil {
			t.Fatalf("%q attach got a formatter; only claude-code opts in", name)
		}
		m.Update(attachDataMsg{sessionID: m.att.sessionID, data: toolCall("raw json for a non-claude backend")})
		if got := m.att.view.render(); !strings.Contains(got, `"type":"tool_use"`) {
			t.Errorf("%q attach no longer mirrors the guest's bytes:\n%s", name, got)
		}
	}
}

// TestAttachRawToggle: ^b f moves between the readable view and the raw
// mirror and back. The toggle takes effect on subsequent frames — the guest's
// screen already holds whatever it painted, and a one-shot keeps writing, so
// each phase asserts on its own new frame's bytes.
func TestAttachRawToggle(t *testing.T) {
	m, _ := attachModelWithAdapter(t, "claude-code")
	drain(m.attachTo(m.harnesses[0], 0))

	m.Update(ctrlKey('b'))
	m.Update(specialKey('f'))
	if !m.att.raw {
		t.Fatal("^b f did not flip the attach to the raw mirror")
	}
	if got := ansi.Strip(m.viewStatusBar()); !strings.Contains(got, "RAW") {
		t.Errorf("status bar does not name the raw mode:\n%s", got)
	}
	m.Update(attachDataMsg{sessionID: m.att.sessionID, data: toolCall("Second call, raw")})
	if got := m.att.view.render(); !strings.Contains(got, `"description":"Second call, raw"`) {
		t.Errorf("raw mode still transformed the guest's bytes:\n%s", got)
	}

	m.Update(ctrlKey('b'))
	m.Update(specialKey('f'))
	if m.att.raw {
		t.Fatal("a second ^b f did not flip back to the readable view")
	}
	m.Update(attachDataMsg{sessionID: m.att.sessionID, data: toolCall("Third call, readable")})
	got := m.att.view.render()
	if !strings.Contains(got, "Third call, readable") {
		t.Errorf("readable mode did not format the new frame:\n%s", got)
	}
	if strings.Contains(got, `"description":"Third call, readable"`) {
		t.Errorf("readable mode passed raw JSON through:\n%s", got)
	}
}

// TestAttachRawToggleIsANoOpWithoutAFormatter: a backend with no formatter
// has only ever had the mirror; ^b f must not flip it into a "raw" mode that
// means nothing (and must not badge it).
func TestAttachRawToggleIsANoOpWithoutAFormatter(t *testing.T) {
	m, _ := attachModelWithAdapter(t, "generic")
	drain(m.attachTo(m.harnesses[0], 0))

	m.Update(ctrlKey('b'))
	m.Update(specialKey('f'))
	if m.att.raw {
		t.Fatal("a formatterless attach was flipped to raw")
	}
	if got := ansi.Strip(m.viewStatusBar()); strings.Contains(got, "RAW") {
		t.Errorf("status bar badges a raw mode that does not exist:\n%s", got)
	}
}

// inkTrustDialog paints Claude Code's folder-trust dialog the way a resident
// Ink app does — eight rows, two of them blank — and ends with the
// cursor-parking tail that has no newline after it. inkSelectionMoved is the
// repaint after a Down keypress: Ink counts rows, blank ones included, to walk
// back up to the options and rewrite them with the marker one row lower (#951).
func inkTrustDialog(markerRow int) string {
	paint := func(n int, text string) string {
		mark := " "
		if n == markerRow {
			mark = "❯"
		}
		return mark + text + "\r\n"
	}
	var b strings.Builder
	b.WriteString("\x1b[?2026h\x1b[H")
	b.WriteString(paint(0, " Quick safety check: Is this a project you created or"))
	b.WriteString(paint(1, " well-known open source project, or work from your team"))
	b.WriteString("\r\n")
	b.WriteString(paint(3, " Yes, I trust this folder"))
	b.WriteString(paint(4, " No, exit"))
	b.WriteString("\r\n")
	b.WriteString(paint(6, " Claude Code will be able to read, edit, and execute files here."))
	b.WriteString(paint(7, " Security guide"))
	b.WriteString("\x1b[?2026l\x1b[1C\x1b[3A")
	return b.String()
}

const inkSelectionMoved = "\x1b[?2026h\x1b[3A" +
	"\r\n" +
	"   Yes, I trust this folder\r\n" +
	" ❯ No, exit\r\n" +
	"\x1b[?2026l\x1b[1C\x1b[1A"

// TestAttachInteractiveClaudeRendersLikeTheMirror is the reported bug as an
// assertion: attaching to a resident claude-code session and watching its
// repaints must leave the same screen `harness capture` shows, which is the
// same bytes given to the emulator unformed. If the readable view reshapes
// Ink's frames — dropping the blank rows Ink's relative moves count, or
// withholding its cursor-parking tail — the redraw lands on the wrong rows and
// the options paint over each other.
func TestAttachInteractiveClaudeRendersLikeTheMirror(t *testing.T) {
	m, _ := attachModelWithAdapter(t, "claude-code")
	drain(m.attachTo(m.harnesses[0], 0))
	if m.att.fmt == nil {
		t.Fatal("attaching to a claude-code harness opened without a formatter")
	}

	frame, repaint := inkTrustDialog(3), inkSelectionMoved
	m.Update(attachDataMsg{sessionID: m.att.sessionID, data: []byte(frame)})
	m.Update(attachDataMsg{sessionID: m.att.sessionID, data: []byte(repaint)})

	mirror := vt.NewEmulator(m.att.view.cols, m.att.view.rows)
	mirror.Write([]byte(frame + repaint))

	for y := 0; y < m.att.view.rows; y++ {
		got := rowText(m.att.view.term, y, m.att.view.cols)
		want := rowText(mirror, y, m.att.view.cols)
		if got != want {
			t.Fatalf("row %d of the attached view is not what the daemon's emulator shows:\n attach: %q\n mirror: %q", y, got, want)
		}
	}
}

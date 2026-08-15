package main

// Governing: stump.wtf/harness#183 — the attach-session table under
// `harness describe` must render the clamping session legibly, including on a
// non-TTY writer (plain text, no color codes), because the table is often read
// piped or from a script.

import (
	"bytes"
	"strings"
	"testing"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

func TestPrintAttachSessionsMarksClamp(t *testing.T) {
	var buf bytes.Buffer
	err := printAttachSessions(&buf, []protocol.AttachSessionInfo{
		{ID: 1, Mode: "rw", Cols: 200, Rows: 49, CreatedAt: "2026-08-15T07:00:00Z"},
		{ID: 1, Mode: "ro", Cols: 80, Rows: 24, CreatedAt: "2026-08-15T01:00:00Z", SetsMin: true},
	})
	if err != nil {
		t.Fatalf("print: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"SESSION", "MODE", "VIEWPORT", "AGE", "80x24", "200x49", "ro", "rw", "clamps guest"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// Exactly one row is flagged, and it is the small one: the marker appears
	// once and on the line carrying 80x24.
	if n := strings.Count(out, "clamps guest"); n != 1 {
		t.Errorf("clamp marker appears %d times, want 1:\n%s", n, out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "clamps guest") && !strings.Contains(line, "80x24") {
			t.Errorf("clamp marker on wrong row: %q", line)
		}
	}
}

func TestPrintAttachSessionsEmptyIsSilent(t *testing.T) {
	var buf bytes.Buffer
	if err := printAttachSessions(&buf, nil); err != nil {
		t.Fatalf("print: %v", err)
	}
	if buf.Len() != 0 {
		t.Errorf("no sessions should print nothing, got:\n%s", buf.String())
	}
}

// TestPrintAttachSessionsUnknownViewport: a 0×0 session (client could not
// detect its size, #183) renders as "unknown" rather than a bogus "0x0".
func TestPrintAttachSessionsUnknownViewport(t *testing.T) {
	var buf bytes.Buffer
	err := printAttachSessions(&buf, []protocol.AttachSessionInfo{
		{ID: 2, Mode: "rw", Cols: 0, Rows: 0, CreatedAt: "2026-08-15T07:00:00Z"},
		{ID: 1, Mode: "rw", Cols: 200, Rows: 49, CreatedAt: "2026-08-15T07:00:00Z", SetsMin: true},
	})
	if err != nil {
		t.Fatalf("print: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "unknown") {
		t.Errorf("0x0 session should render as unknown:\n%s", out)
	}
	if strings.Contains(out, "0x0") {
		t.Errorf("0x0 is not a viewport:\n%s", out)
	}
}

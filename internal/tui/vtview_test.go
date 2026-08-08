package tui

import (
	"strings"
	"testing"
)

// TestVTViewRendersWrittenBytes verifies the client-side embedded terminal
// reproduces the harness screen: bytes written to the emulator appear in the
// rendered grid (SPEC-0001 REQ "Attached Mode": render the real terminal from
// the daemon's x/vt screen; ADR-0003).
func TestVTViewRendersWrittenBytes(t *testing.T) {
	v := newVTView(20, 3)
	v.write([]byte("hello world"))
	out := v.render()
	if !strings.Contains(out, "hello world") {
		t.Fatalf("render missing written text; got %q", out)
	}
}

// TestVTViewColorPreserved verifies an SGR color sequence written to the
// emulator survives into the render (colors work inside the attached pane).
func TestVTViewColorPreserved(t *testing.T) {
	v := newVTView(20, 2)
	v.write([]byte("\x1b[31mRED\x1b[0m"))
	out := v.render()
	if !strings.Contains(out, "RED") {
		t.Fatalf("render lost the text; got %q", out)
	}
	if !strings.Contains(out, "\x1b[") {
		t.Fatalf("render lost the color; got %q", out)
	}
}

// TestVTViewResize verifies resizing the viewport doesn't panic and updates the
// dimensions.
func TestVTViewResize(t *testing.T) {
	v := newVTView(10, 3)
	v.resize(40, 10)
	if v.cols != 40 || v.rows != 10 {
		t.Fatalf("resize to 40x10 gave %dx%d", v.cols, v.rows)
	}
	v.write([]byte("ok"))
	if !strings.Contains(v.render(), "ok") {
		t.Fatal("render broke after resize")
	}
}

// TestVTViewCursorSeqReturnsCUP verifies cursorSeq emits a CUP (CSI row;col H)
// sequence matching the emulator's tracked cursor position. This is what makes
// the hardware cursor visible in attached mode (#48).
func TestVTViewCursorSeqReturnsCUP(t *testing.T) {
	v := newVTView(40, 10)
	// Move cursor to row 5, col 12 (1-indexed in CUP).
	v.write([]byte("\x1b[5;12H"))
	seq := v.cursorSeq()
	// CUP is \x1b[row;colH — the emulator tracks 0-indexed, CUP is 1-indexed.
	want := "\x1b[5;12H"
	if seq != want {
		t.Errorf("cursorSeq = %q, want %q", seq, want)
	}
}

// TestVTViewCursorSeqDefault verifies the cursor starts at 1;1 (home).
func TestVTViewCursorSeqDefault(t *testing.T) {
	v := newVTView(40, 10)
	seq := v.cursorSeq()
	want := "\x1b[1;1H"
	if seq != want {
		t.Errorf("cursorSeq on fresh view = %q, want %q", seq, want)
	}
}

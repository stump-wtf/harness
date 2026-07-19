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

package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
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

// TestVTViewRenderPaintsCursorCell verifies render() paints the emulator's
// cursor cell in reverse video (#48): Bubble Tea owns the hardware cursor, so
// the guest cursor is shown by inverting the cell it occupies.
func TestVTViewRenderPaintsCursorCell(t *testing.T) {
	cases := []struct {
		name  string
		input string
		x, y  int // expected 0-indexed cursor cell
	}{
		{name: "home", input: "", x: 0, y: 0},
		{name: "moved", input: "\x1b[5;12H", x: 11, y: 4}, // CUP is 1-indexed
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := newVTView(40, 10)
			v.write([]byte(tc.input))
			lines := strings.Split(v.render(), "\n")
			const marker = "\x1b[7m"
			for i, ln := range lines {
				if has := strings.Contains(ln, marker); has != (i == tc.y) {
					t.Errorf("row %d: cursor cell present=%v, want %v", i, has, i == tc.y)
				}
			}
			row := lines[tc.y]
			idx := strings.Index(row, marker)
			if idx < 0 {
				t.Fatalf("row %d has no cursor cell: %q", tc.y, row)
			}
			if got := lipgloss.Width(row[:idx]); got != tc.x {
				t.Errorf("cursor cell painted at column %d, want %d", got, tc.x)
			}
		})
	}
}

// TestVTViewRenderRespectsHiddenCursor verifies DECTCEM: a guest that hides
// its cursor (\x1b[?25l) gets no painted cursor cell (#48).
func TestVTViewRenderRespectsHiddenCursor(t *testing.T) {
	v := newVTView(40, 10)
	v.write([]byte("\x1b[?25l"))
	if strings.Contains(v.render(), "\x1b[7m") {
		t.Fatal("hidden cursor must not be painted")
	}
}

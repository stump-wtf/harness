package tui

import "testing"

// TestVTViewDeleteLineWithOversizedScrollRegion reproduces the index-out-of-range
// panic from harness: a guest emits DECSTBM with a bottom margin larger than the
// current terminal height (common when restoring a saved scroll region after a
// resize, or when the daemon's screen is taller than the client viewport). x/vt's
// DECSTBM handler does not clamp `bottom` to the buffer height, so the scroll
// region's Max.Y exceeds len(b.Lines), and the next DL indexes past the end.
//
// The fix installs a clamped DECSTBM handler in newVTView that re-emits a
// bounds-safe sequence when the guest's bottom margin exceeds the buffer height.
func TestVTViewDeleteLineWithOversizedScrollRegion(t *testing.T) {
	v := newVTView(80, 24)
	// Guest sets scroll region rows 1-33 on a 24-row terminal. Without the
	// fix, x/vt accepts bottom=33 verbatim, producing scroll.Max.Y=33 >
	// len(Lines)=24, and DL panics with "index out of range [24] with length 24".
	v.write([]byte("\x1b[1;33r"))
	v.write([]byte("\x1b[24;1H"))
	v.write([]byte("\x1b[M")) // DL 1
	_ = v.render()
}

// TestVTViewInsertLineWithOversizedScrollRegion verifies IL is also safe.
func TestVTViewInsertLineWithOversizedScrollRegion(t *testing.T) {
	v := newVTView(80, 24)
	v.write([]byte("\x1b[1;33r"))
	v.write([]byte("\x1b[24;1H"))
	v.write([]byte("\x1b[L")) // IL 1
	_ = v.render()
}

// TestVTViewScrollUpWithOversizedScrollRegion verifies SU is also safe.
func TestVTViewScrollUpWithOversizedScrollRegion(t *testing.T) {
	v := newVTView(80, 24)
	v.write([]byte("\x1b[1;33r"))
	v.write([]byte("\x1b[S")) // SU 1
	_ = v.render()
}

// TestVTViewInsertCharWithOversizedHorizontalMargins reproduces the same class
// of panic for DECSLRM: right margin exceeds buffer width, causing ICH to
// index past the end of the line slice.
func TestVTViewInsertCharWithOversizedHorizontalMargins(t *testing.T) {
	v := newVTView(80, 24)
	v.write([]byte("\x1b[?69h"))   // enable left-right margin mode
	v.write([]byte("\x1b[1;90s"))  // right=90 > width=80
	v.write([]byte("\x1b[12;80H")) // cursor at right edge
	v.write([]byte("\x1b[@"))      // ICH 1
	_ = v.render()
}

// TestVTViewDeleteLineAfterShrink verifies that a scroll region set at a
// larger size remains safe after the view shrinks.
func TestVTViewDeleteLineAfterShrink(t *testing.T) {
	v := newVTView(80, 40)
	v.write([]byte("\x1b[1;33r"))
	v.write([]byte("\x1b[24;1H"))
	v.resize(80, 24)
	v.write([]byte("\x1b[M"))
	_ = v.render()
}

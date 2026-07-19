// Package attach is the daemon-side data plane: one github.com/charmbracelet/x/vt
// terminal emulator plus a bounded scrollback ring per running harness, fed by
// the supervisor's raw PTY output (via the Manager ExtraOut hook), and the
// attach sessions that stream a snapshot-then-tail-then-live view of it to
// clients with coalesce-to-snapshot backpressure.
//
// Governing: SPEC-0002 (daemon-protocol) REQ "Attach Session" and REQ
// "Backpressure Isolation"; ADR-0003 (native multiplexer: one x/vt emulator per
// harness; smallest-attached-wins resize); ADR-0007 (ring buffer + on-disk log;
// the PTY reader never blocks on a slow client — it repaints from the current
// screen); ADR-0008 (read-only attach).
package attach

import (
	"bytes"
	"sync"
)

// DefaultRingLines is the default scrollback depth (ADR-0007: "default N lines,
// configurable, e.g. 10k"). It bounds memory for chatty long-running agents.
const DefaultRingLines = 10000

// ring is a bounded, line-oriented scrollback buffer of raw PTY bytes. It
// retains at most maxLines completed lines (each including its trailing '\n')
// plus the current partial line, evicting the oldest lines past the cap. Tail
// reconstructs the retained bytes for the scrollback portion of an attach
// (SPEC-0002 REQ "Attach Session": "a bounded tail of scrollback").
type ring struct {
	maxLines int

	mu      sync.Mutex
	lines   [][]byte // completed lines, oldest first, each ending in '\n'
	partial []byte   // bytes since the last newline
}

// newRing builds a ring holding up to maxLines lines (DefaultRingLines if <=0).
func newRing(maxLines int) *ring {
	if maxLines <= 0 {
		maxLines = DefaultRingLines
	}
	return &ring{maxLines: maxLines}
}

// Write appends raw PTY bytes, splitting on newlines and evicting the oldest
// lines once the cap is exceeded. It never returns an error and never blocks on
// anything but its own mutex (ADR-0007 backpressure: writing scrollback must
// not stall the PTY reader).
func (r *ring) Write(p []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for len(p) > 0 {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			r.partial = append(r.partial, p...)
			break
		}
		// Complete the current line up to and including the newline.
		line := make([]byte, 0, len(r.partial)+i+1)
		line = append(line, r.partial...)
		line = append(line, p[:i+1]...)
		r.partial = r.partial[:0]
		r.lines = append(r.lines, line)
		if len(r.lines) > r.maxLines {
			// Evict the oldest; copy down so the backing array can shrink.
			drop := len(r.lines) - r.maxLines
			r.lines = append(r.lines[:0], r.lines[drop:]...)
		}
		p = p[i+1:]
	}
}

// Tail returns a copy of the retained scrollback bytes (completed lines then
// the current partial line), oldest first.
func (r *ring) Tail() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	var n int
	for _, l := range r.lines {
		n += len(l)
	}
	n += len(r.partial)
	out := make([]byte, 0, n)
	for _, l := range r.lines {
		out = append(out, l...)
	}
	out = append(out, r.partial...)
	return out
}

// Lines reports how many completed lines are currently retained (for tests).
func (r *ring) Lines() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.lines)
}

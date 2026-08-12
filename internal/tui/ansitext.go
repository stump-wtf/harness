package tui

// Governing: SPEC-0001 REQ "Scrollback Substate" and REQ "Dashboard" (live
// read-only tail). Both surfaces render the harness's RAW PTY byte stream as
// text — the daemon's durable log (ADR-0007), not a parsed screen. The live
// attached view can render that faithfully because it feeds an x/vt emulator;
// these frozen text views cannot, so whatever they emit is executed verbatim by
// the user's real terminal.
//
// So the question for every byte is not "can we display it" but "what does it
// DO". Cursor addressing, erases, and alt-screen toggles act on the cockpit
// around the text and tear the frame apart; a bare CR returns to column 0 and
// the rest of the line overwrites what the chrome already drew. SGR is the
// exception: it is zero-width, moves nothing, and carries the harness's colors,
// which are worth keeping.

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// inertText makes one line of raw PTY output safe to print inside the cockpit:
// printable graphemes and SGR survive, every other escape sequence and control
// byte is dropped, and a line that opened a style closes it so color cannot
// bleed into the chrome below (the same guarantee vtView.render makes per row).
//
// Tabs become a single space rather than being dropped, so words that were
// tab-separated don't run together — and rather than being kept, since a tab's
// rendered width depends on the terminal's stops, which would put the line's
// measured width at odds with its printed width.
//
// initialState carries the parser state from a preceding line so that escape
// payloads split across line boundaries (DCS/sixel, OSC, APC) are fully
// suppressed rather than having their continuation bytes emitted as text.
func inertText(line string, initialState byte) (string, byte) {
	var (
		b      strings.Builder
		state  = initialState
		styled bool
		rest   = line
	)
	for len(rest) > 0 {
		seq, width, n, newState := ansi.DecodeSequence(rest, state, nil)
		if n == 0 {
			break // undecodable tail; nothing safe left to emit
		}
		switch {
		case width > 0:
			// A printable grapheme — the only thing that occupies a cell.
			b.WriteString(seq)
		case seq == "\t":
			b.WriteByte(' ')
		case isSGR(seq):
			b.WriteString(seq)
			styled = true
		}
		// Everything else — CUP/ED/EL, DEC private modes, OSC, DCS, CR, BS,
		// BEL — is deliberately dropped.
		rest, state = rest[n:], newState
	}
	if styled {
		b.WriteString("\x1b[0m")
	}
	return b.String(), state
}

// inertLines applies inertText to a slice, threading parser state across line
// boundaries so escape payloads split by ring.Write's newline split are fully
// suppressed (issue #146).
func inertLines(lines []string) []string {
	out := make([]string, len(lines))
	var state byte
	for i, ln := range lines {
		out[i], state = inertText(ln, state)
	}
	return out
}

// isSGR reports whether seq is a Select Graphic Rendition sequence (CSI ... m)
// — the styling escapes, which are safe to keep because they occupy no cells
// and move no cursor.
func isSGR(seq string) bool {
	return len(seq) > 2 && seq[0] == 0x1b && seq[1] == '[' && seq[len(seq)-1] == 'm'
}

// Terminal sanitizing for package text a stable's author wrote and the
// operator's terminal will print: the README at info, install and upgrade,
// and the upgrade's per-file diff. That text is untrusted content from a
// remote, so it reaches the terminal as inert characters only: nothing in it
// may clear the screen, move the cursor, retitle the window, write the
// clipboard, or dress a link up as different text (OSC 8).
//
// The existing strippers in the tree don't fit this job. tmpl.stripControls
// removes the control bytes but leaves an escape sequence's parameters
// behind as visible text ("[2J"). ansifold.Text keeps SGR on purpose and
// works line by line on PTY output. This one drops whole sequences,
// parameters and payloads included, and keeps no styling at all.
//
// Governing: ADR-0044 (agent package stables: nothing from a remote is
// executed; package text is data), SPEC-0026 REQ-2 (the package README),
// REQ-8 (the upgrade diff).
//
// @joestump-agent 10/09/2026 - Added for harness#929.
package agentpkg

import (
	"strings"
	"unicode/utf8"
)

const (
	esc = 0x1b
	bel = 0x07

	c1DCS = 0x90
	c1SOS = 0x98
	c1CSI = 0x9b
	c1ST  = 0x9c
	c1OSC = 0x9d
	c1PM  = 0x9e
	c1APC = 0x9f
)

// SanitizeTerminal returns s with everything a terminal would interpret
// removed, so it prints as plain text:
//
//   - ESC sequences of every kind: CSI (ESC [ … final) with its parameters,
//     the string sequences OSC, DCS, SOS, PM and APC (ESC ] P X ^ _) with
//     their whole payload up to a BEL or ST terminator (ESC \ or 0x9C), and
//     two-byte escapes such as ESC c. OSC 8 hyperlinks lose both their
//     opener and closer, and the link text survives as plain text.
//   - C1 controls U+0080..U+009F, whether UTF-8 encoded or as raw 8-bit
//     bytes. The 8-bit introducers (0x9B CSI, 0x9D OSC, 0x90 DCS, 0x98,
//     0x9E, 0x9F) drop their parameters and payloads too, the way their
//     ESC forms do.
//   - C0 controls and DEL, except newline and tab. A CR goes too, so a
//     CRLF file renders as LF and a bare CR cannot overprint a line.
//
// Any other invalid UTF-8 byte becomes U+FFFD, so a mis-encoded README
// still shows where its text was.
//
// A sequence never runs past a newline. An unterminated CSI or string
// sequence ends at the next newline or ESC, or at the end of input. A stray
// "ESC ]" in a README hides the rest of its own line, not the rest of the
// document. The text that follows is printed as plain characters, which is
// harmless once the introducer is gone.
func SanitizeTerminal(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] == esc {
			i = skipEscape(s, i+1)
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			// Invalid UTF-8. A raw 0x80..0x9F byte is an 8-bit C1 control to
			// a terminal not in UTF-8 mode, and its sequence is dropped.
			if c := s[i]; c >= 0x80 && c <= 0x9f {
				i = skipC1(s, c, i+1)
				continue
			}
			b.WriteRune(utf8.RuneError)
		case r >= 0x80 && r <= 0x9f:
			i = skipC1(s, byte(r), i+size)
			continue
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f:
			// Other C0 controls, CR among them, and DEL.
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

// skipEscape consumes the sequence an ESC at s[j-1] introduced and returns
// the index of the first byte after it.
func skipEscape(s string, j int) int {
	if j >= len(s) {
		return j
	}
	switch c := s[j]; {
	case c == '[':
		return skipCSI(s, j+1)
	case c == ']' || c == 'P' || c == 'X' || c == '^' || c == '_':
		return skipString(s, j+1)
	case c >= 0x20 && c <= 0x2f:
		// nF escape: intermediates, then one final byte.
		for j < len(s) && s[j] >= 0x20 && s[j] <= 0x2f {
			j++
		}
		if j < len(s) && s[j] >= 0x30 && s[j] <= 0x7e {
			j++
		}
		return j
	case c >= 0x30 && c <= 0x7e:
		// A two-byte escape (ESC c, ESC 7, ESC =, …).
		return j + 1
	default:
		// ESC before a control or a non-ASCII byte: drop the ESC alone and
		// let the main loop judge what follows.
		return j
	}
}

// skipC1 consumes the sequence a C1 control c introduced, with j indexing
// the byte after it.
func skipC1(s string, c byte, j int) int {
	switch c {
	case c1CSI:
		return skipCSI(s, j)
	case c1OSC, c1DCS, c1SOS, c1PM, c1APC:
		return skipString(s, j)
	default:
		return j
	}
}

// skipCSI consumes a control sequence's parameter and intermediate bytes
// (0x20..0x3F) and its final byte (0x40..0x7E). Anything else ends it
// early and is left for the main loop.
func skipCSI(s string, j int) int {
	for j < len(s) && s[j] >= 0x20 && s[j] <= 0x3f {
		j++
	}
	if j < len(s) && s[j] >= 0x40 && s[j] <= 0x7e {
		j++
	}
	return j
}

// skipString consumes an OSC, DCS, SOS, PM or APC payload and its
// terminator: BEL, ESC \ or ST (raw 0x9C or UTF-8 U+009C). A newline, or
// an ESC that does not begin ST, ends the payload unconsumed (see
// SanitizeTerminal).
func skipString(s string, j int) int {
	for j < len(s) {
		switch c := s[j]; {
		case c == bel || c == c1ST:
			return j + 1
		case c == 0xc2 && j+1 < len(s) && s[j+1] == c1ST:
			return j + 2
		case c == esc:
			if j+1 < len(s) && s[j+1] == '\\' {
				return j + 2
			}
			return j
		case c == '\n':
			return j
		}
		j++
	}
	return j
}

package tui

// Governing: SPEC-0001 REQ "Dashboard" (live peek) and REQ "Scrollback
// Substate" — both freeze or tail the harness log as chat-style lines, and
// both were fed the RAW log tail: escape bytes and one repaint frame per
// refresh tick, so a TUI agent running a 60-second tool showed ~60 junk lines
// ("✻ Working (3s)…", "(4s)…") instead of "the tool was called" (#280).
//
// sanitizeTailLines makes the chat view robust at the consumption side:
// contentless lines dropped, and consecutive status-chatter lines — identical
// except for a ticking counter or spinner glyph — collapsed to their first
// occurrence. Lines are returned verbatim (styling intact); safety is applied
// downstream by inertLines, exactly as before. The daemon-side fix (#279 /
// PR #281) cleans the log at the source; this keeps the client honest against
// noisy sources (pre-fix log files, cmd harnesses that print their own
// timers).

import (
	"regexp"
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// digitRun matches any run of digits, so "Working (3s)" and "Working (4s)"
// share the signature "Working (#s)".
var digitRun = regexp.MustCompile(`[0-9]+`)

// spinnerGlyphs are the decorations status lines cycle through while a tool
// runs; they carry no information a signature needs.
const spinnerGlyphs = "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏✻✽✳✱✲⋅·…"

// chatterSignature reduces a line to what it says, ignoring a ticking counter
// or spinner glyph: digits collapsed to "#", spinner glyphs removed,
// whitespace squeezed. Two consecutive lines with the same signature are the
// same status update rendered a second later — the per-second junk this file
// exists to collapse.
func chatterSignature(line string) string {
	s := ansi.Strip(line)
	s = digitRun.ReplaceAllString(s, "#")
	s = strings.Map(func(r rune) rune {
		if strings.ContainsRune(spinnerGlyphs, r) {
			return -1
		}
		return r
	}, s)
	return strings.Join(strings.Fields(s), " ")
}

// sanitizeTailLines cleans a raw log tail into readable chat lines: lines with
// no visible content dropped, and runs of consecutive chatter lines (same
// signature) collapsed to the first. The kept lines are returned verbatim.
func sanitizeTailLines(text string) []string {
	raw := strings.Split(text, "\n")
	out := make([]string, 0, len(raw))
	prevSig := "\x00" // no line has this signature; forces the first through
	for _, ln := range raw {
		if strings.TrimSpace(ansi.Strip(ln)) == "" {
			continue
		}
		sig := chatterSignature(ln)
		if sig == prevSig {
			continue // same status line, one tick later
		}
		prevSig = sig
		out = append(out, ln)
	}
	return out
}

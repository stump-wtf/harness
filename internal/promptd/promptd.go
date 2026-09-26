// Package promptd detects a harness frozen at an interactive TTY prompt.
//
// Governing: issue #735; ADR-0040 (screen observability). The daemon is
// deliberately agnostic about what runs inside a harness — payloads stay
// opaque, and agent-awareness bolts on as a detector. This package is that
// detector, and it keeps the agnosticism: it pattern-matches PLAIN TEXT the
// harness has already rendered to its terminal (the x/vt emulator's screen the
// attach plane maintains, ADR-0003). It never interprets backend semantics,
// never reads a transcript, and never sends input.
//
// A match alone means nothing — an agent prints the word "approve" in ordinary
// prose all the time. Callers must gate a verdict on idleness: the screen has
// not changed for DefaultIdle (or their own threshold) AND a prompt pattern is
// visible. That conjunction is what makes "frozen at a prompt nobody will
// answer" distinguishable from "busy and repaint-heavy".
package promptd

import (
	"regexp"
	"strings"
	"time"
)

// DefaultIdleThreshold is how long a harness's screen must go unchanged before
// a prompt pattern on it is believed: an interactive dialog sits still for
// seconds while a busy TUI repaints constantly. Sweep callers poll on cadences
// of their own; this default only bounds how fast a verdict can be trusted.
const DefaultIdleThreshold = 30 * time.Second

// Hit is one detected prompt shape: a stable Label for alert text and the
// line it was seen on.
type Hit struct {
	// Label names the pattern, e.g. "y/n prompt" — stable enough for a
	// machine reader, short enough for a table cell.
	Label string
	// Line is the screen row the pattern matched, trimmed of padding. It is
	// content the HARNESS printed, so a caller showing it is showing user
	// output, not advice.
	Line string
}

// pattern is one named regex over the visible screen text. Matching is
// case-insensitive and line-oriented: interactive dialogs are drawn row-wise,
// and the cue lives on a single row ("Do you want to proceed? › Yes").
type pattern struct {
	label string
	re    *regexp.Regexp
}

// compile builds the pattern table once. Each entry is a shape a REAL
// interactive prompt draws — permission dialogs, workspace-trust screens, MCP
// consent, y/n confirmations, "press enter to continue" walls. Kept curated:
// a broad heuristic ("any line ending in ?") fires on tool output asking a
// HUMAN a rhetorical question mid-run, and a false "waiting" is worse than
// none — it cries wolf precisely when operators stop reading the column.
var patterns = compile([]struct{ label, re string }{
	// Explicit confirmations, the most common unanswerable shape.
	{"y/n prompt", `(?i)\b\(?(y/n|y/N|Y/n|yes/no)\b\)?\s*[:?]?$|\[[yY]/[nN]\]`},
	{"do you want", `(?i)\bdo you want (to|me|this)\b|\bwould you like (to|me|us)\b|\bshall i\b`},
	{"proceed confirm", `(?i)\b(proceed|continue|confirm)\s*\?\s*$`},
	// Permission dialogs (agent CLIs, and the shells they run in).
	{"permission request", `(?i)\ballow\b.{0,40}\?\s*$|\bpermission (to|required)\b|\bapprove (this|the|it)\b|\bdeny\b.{0,30}\ballow\b`},
	{"read outside workdir", `(?i)\b(read|write|access)\b.{0,20}\boutside (of )?(the )?(working|this) (director|folder)`},
	// Workspace / directory trust screens (claude-code, codex, editors).
	{"trust prompt", `(?i)\bdo you trust\b|\btrust (the )?(files|this folder|authors|this project)\b|\buntrusted (folder|directory|workspace)\b`},
	// MCP / tool consent screens.
	{"consent prompt", `(?i)\b(consent|authorize|grant access)\b|\bconnect(ion)? (to|request)\b.{0,30}\?\s*$`},
	// Press-a-key walls and explicit idle declarations.
	{"press enter", `(?i)\bpress (enter|return|any key|a key)\b|\bhit enter\b`},
	{"waiting for input", `(?i)\bwaiting (for|on)\b.{0,30}(input|approval|confirmation|response|you)\b|\bawaiting (input|approval|confirmation)\b`},
	// Selection menus a stuck TUI parks on: a cursor prompt with nothing
	// selected and no timeout.
	{"select prompt", `(?i)^.*(❯|>)\s*(yes|no|allow|deny|skip|cancel|continue|retry)\s*$`},
})

func compile(specs []struct{ label, re string }) []pattern {
	out := make([]pattern, 0, len(specs))
	for _, s := range specs {
		out = append(out, pattern{label: s.label, re: regexp.MustCompile(s.re)})
	}
	return out
}

// Match returns the first prompt-shaped row on the visible screen, or false.
// rows are the emulator's current screen rows, blank padding already trimmed —
// the ScreenState the attach plane serves (ADR-0003). First match wins because
// every pattern is itself evidence; the order above is most-specific first.
func Match(rows []string) (Hit, bool) {
	for _, ln := range rows {
		t := strings.TrimSpace(ln)
		if t == "" {
			continue
		}
		for _, p := range patterns {
			if p.re.MatchString(t) {
				return Hit{Label: p.label, Line: t}, true
			}
		}
	}
	return Hit{}, false
}

// Waiting is the verdict a caller renders: a prompt-shaped screen that has sat
// unchanged long enough to believe.
func Waiting(rows []string, idle time.Duration, threshold time.Duration) (Hit, bool) {
	if idle < threshold {
		return Hit{}, false
	}
	return Match(rows)
}

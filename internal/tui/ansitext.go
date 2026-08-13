package tui

// inertText and inertLines delegate to internal/ansifold so that cmd/harness
// (harness logs) and internal/tui share the same inert-filtering logic.
//
// Governing: SPEC-0001 REQ "Scrollback Substate" and REQ "Dashboard" (live
// read-only tail). See internal/ansifold/ansifold.go for the full governing
// comment and the eviction-case documentation.

import "gitea.stump.rocks/stump.wtf/harness/internal/ansifold"

func inertText(line string, initialState byte) (string, byte) {
	return ansifold.Text(line, initialState)
}

func inertLines(lines []string) []string {
	return ansifold.Lines(lines)
}

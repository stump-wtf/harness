// Scan findings: the shared result type of the SPEC-0026 content scan, the
// decision the confirmation gate makes over them, and the new-finding
// comparison upgrades use. A finding never carries matched text — only the
// package-relative file, the line, the pattern identifier and the severity —
// so no log or error built from a finding can echo the scanned content
// (SPEC-0026 Error Handling Standards).
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-5 (content scan
// and severity), Error Handling Standards.
//
// @joestump-agent 10/02/2026 - Added for harness#812.
package agentpkg

import (
	"fmt"
	"hash/fnv"
)

// Severity is a scan finding's classification (SPEC-0026 REQ-5): high
// blocks install and upgrade behind --force-unsafe and a retype; low is
// shown and never blocks.
type Severity string

const (
	// SeverityHigh blocks unless --force-unsafe is given with a re-typed
	// <stable>/<package>.
	SeverityHigh Severity = "high"
	// SeverityLow is shown in the confirmation output and never blocks.
	SeverityLow Severity = "low"
)

// Finding is one scan hit. File is the package-relative path, Line is
// 1-based, PatternID names the fixed pattern that matched, and LineHash
// fingerprints the matched line so an upgrade can tell a re-worded finding
// from one that merely moved lines. The matched text itself is deliberately
// absent (Error Handling Standards).
type Finding struct {
	File      string
	Line      int
	PatternID string
	Severity  Severity
	LineHash  uint64
}

// Where renders the finding's location: "file:line", or just "file" for a
// finding about a whole file (Line 0), such as package.no-readme.
func (f Finding) Where() string {
	if f.Line == 0 {
		return f.File
	}
	return fmt.Sprintf("%s:%d", f.File, f.Line)
}

// NewSince returns the candidate findings that were absent from the
// installed pin's findings, keyed by file, pattern id and a hash of the
// matched line — never by line number, because the spec's scenario is a file
// "unchanged by line count but altered in content": a finding that only
// moved lines is not new, and a re-worded one on the same line count is
// (SPEC-0026 REQ-5, upgrade).
func NewSince(installed, candidate []Finding) []Finding {
	type key struct {
		file    string
		pattern string
		hash    uint64
	}
	seen := make(map[key]bool, len(installed))
	for _, f := range installed {
		seen[key{f.File, f.PatternID, f.LineHash}] = true
	}
	var out []Finding
	for _, f := range candidate {
		if !seen[key{f.File, f.PatternID, f.LineHash}] {
			out = append(out, f)
		}
	}
	return out
}

// HashLine fingerprints a scanned line. It never leaves the scan pipeline:
// only Finding.LineHash, a 64-bit FNV digest, is kept.
func HashLine(line string) uint64 {
	h := fnv.New64a()
	h.Write([]byte(line))
	return h.Sum64()
}

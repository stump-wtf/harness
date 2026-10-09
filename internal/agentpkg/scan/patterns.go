// Package scan is the SPEC-0026 content scan: a fixed, versioned pattern
// table compiled into the binary that lints a package's manifest text and
// every bundled .md/.txt file before an install or upgrade confirms. It is
// a heuristic tripwire, not a certification — every output that carries
// findings also states that a clean scan is not a guarantee of safety.
//
// The table is deliberately not configurable: a project file that could
// weaken the scan before an install runs against it is exactly the surface
// keeping it out of config removes. Versioning with the binary makes a scan
// result reproducible from a Harness version alone.
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-5 (content
// scan and severity), Error Handling Standards.
//
// @joestump-agent 10/02/2026 - Added for harness#812.
package scan

import (
	"regexp"

	"github.com/stump-wtf/harness/internal/agentpkg"
)

// TableVersion identifies the compiled pattern table. Bump it whenever a
// pattern is added, reworded or re-graded, so an install record can name
// which table produced its findings.
const TableVersion = "2"

// Pattern is one entry of the fixed pattern table. ID is the stable
// identifier a blocked error and a false-positive report name; Severity is
// its REQ-5 classification.
type Pattern struct {
	ID       string
	Severity agentpkg.Severity
	// re matches within a single line.
	re *regexp.Regexp
}

// patterns is the fixed high/low table (SPEC-0026 REQ-5). Severity decisions:
//
//   - override.ignore-instructions, exfil.credentials and shell.pipe-to-shell
//     are high by the spec's own wording: they target overriding the outer
//     or system prompt, credential handling/exfiltration, and pipe-to-shell.
//   - encoded.large-block is HIGH by decision: an oversized encoded run is
//     an obfuscation channel the operator cannot read at confirm time, and
//     no legitimate skill prose carries one. The cost of a false positive is
//     one typed override; the cost of a miss is an unread payload executing.
//   - imperative.prose is LOW by decision: ADR-0011's own skills are full of
//     MUST/ALWAYS/NEVER, and imperative language alone is shown, never
//     blocking (design.md's false-positive risk).
//   - link.foreign-domain is LOW: a link to a domain other than the
//     package's declared homepage is a prompt to look, not a block.
//
// No config key or flag can alter this table (REQ-5, design.md).
var patterns = []Pattern{
	{
		ID:       "override.ignore-instructions",
		Severity: agentpkg.SeverityHigh,
		re:       regexp.MustCompile(`(?i)\b(?:disregard|ignore|forget|override|bypass)\b[^.\n]{0,60}\b(?:previous|prior|above|earlier|initial|original|preceding|outer|system|developer|assistant|user)\b[^.\n]{0,30}\b(?:instructions?|prompts?|rules?|directives?|messages?|context)\b`),
	},
	{
		ID:       "exfil.credentials",
		Severity: agentpkg.SeverityHigh,
		re:       regexp.MustCompile(`(?i)\b(?:send|upload|post|transmit|exfiltrate|leak|share|include|attach|embed)\b[^.\n]{0,60}\b(?:credentials?|secrets?|passwords?|passphrases?|api[_ -]?keys?|tokens?|ssh[_ -]?keys?|\.env|private[_ -]?keys?)\b`),
	},
	{
		ID:       "shell.pipe-to-shell",
		Severity: agentpkg.SeverityHigh,
		re:       regexp.MustCompile(`(?i)\b(?:curl|wget|fetch)\b[^\n|]{0,120}\|\s*(?:sudo\s+)?(?:sh|bash|zsh|dash|ksh)\b`),
	},
	{
		ID:       "encoded.large-block",
		Severity: agentpkg.SeverityHigh,
		re:       regexp.MustCompile(`[A-Za-z0-9+/]{400,}={0,2}`),
	},
	{
		ID:       "imperative.prose",
		Severity: agentpkg.SeverityLow,
		re:       regexp.MustCompile(`(?i)\b(?:must not|must|shall|always|never|do not|don't)\b`),
	},
}

// linkPattern extracts markdown links and bare http(s) URLs from a line; the
// foreign-domain decision is made in code against the package's homepage,
// not by a regex.
var linkPattern = regexp.MustCompile(`(?:\[[^\]]*\]\((https?://[^)\s]+)\)|(https?://[^\s)\]]+))`)

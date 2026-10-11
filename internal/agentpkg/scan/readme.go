// The package.no-readme check: a package without a README.md draws one low
// finding. It is shown on info, install and upgrade and never blocks;
// `harness agent stable lint` is where a missing README becomes an error.
// The README itself, when present, is scanned like any other .md file.
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-2 (layout:
// README.md expected), REQ-5 (content scan and severity).
//
// @joestump-agent 10/09/2026 - Added for harness#929.
package scan

import "github.com/stump-wtf/harness/internal/agentpkg"

// readmeFindings returns the package.no-readme finding when pkgDir has no
// README.md as a regular file. The finding names the file that should exist
// and carries Line 0, since it is about the whole file rather than one line
// of it.
func readmeFindings(pkgDir string) ([]agentpkg.Finding, error) {
	ok, err := agentpkg.HasReadme(pkgDir)
	if err != nil || ok {
		return nil, err
	}
	return []agentpkg.Finding{{
		File:      agentpkg.ReadmeFile,
		Line:      0,
		PatternID: agentpkg.FindingNoReadme,
		Severity:  agentpkg.SeverityLow,
		LineHash:  agentpkg.HashLine(""),
	}}, nil
}

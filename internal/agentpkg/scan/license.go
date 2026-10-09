// The package.no-license finding: a manifest that declares no
// [package].license gets one low finding, shown at info, install and upgrade
// and never blocking. `harness agent stable lint` runs this same scan and
// promotes the finding to an error, so a stable cannot publish an unlicensed
// package while an operator can still install one knowingly.
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-3 (manifest
// schema: [package].license), REQ-5 (content scan and severity).
//
// @joestump-agent 10/09/2026 - Added for harness#932.
package scan

import (
	"strings"

	"github.com/stump-wtf/harness/internal/agentpkg"
)

// licenseFindings returns the package.no-license finding when man declares
// no license. It points at the [package] header in package.toml — where the
// key belongs — and hashes a fixed empty line, so an upgrade that merely
// moves or reformats the header never reports the finding as new.
func licenseFindings(manifestText string, man *agentpkg.Manifest) []agentpkg.Finding {
	if man == nil || man.Package.License != "" {
		return nil
	}
	line := 1
	for i, l := range strings.Split(manifestText, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "[package]") {
			line = i + 1
			break
		}
	}
	return []agentpkg.Finding{{
		File:      ManifestFile,
		Line:      line,
		PatternID: agentpkg.FindingNoLicense,
		Severity:  agentpkg.SeverityLow,
		LineHash:  agentpkg.HashLine(""),
	}}
}

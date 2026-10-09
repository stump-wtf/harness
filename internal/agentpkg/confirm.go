// The confirmation gate every install and upgrade runs before a pin
// changes (SPEC-0026 REQ-4, REQ-5): the report the operator reads, the pure
// decision function over findings/requests/flags/TTY/retype, and the install
// record that retains an overridden finding.
//
// The report always ends with the no-guarantee statement — the scan is a
// tripwire, not a certification, and the human always reads the manifest and
// the file list regardless of the scan result. Logging carries only paths,
// pattern ids and severities, never scanned content.
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-4 (capability
// requests), REQ-5 (content scan and severity), Error Handling Standards.
//
// @joestump-agent 10/02/2026 - Added for harness#812.
package agentpkg

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	clog "github.com/charmbracelet/log"
)

// NoGuarantee is the statement every scan-bearing output ends with.
const NoGuarantee = "A clean scan is a heuristic tripwire, not a guarantee of safety — read the manifest and the bundled files above."

// ReportInput is everything the confirmation report renders.
type ReportInput struct {
	// Ref is "<stable>/<package>".
	Ref string
	// ManifestRaw is the package.toml text, printed verbatim.
	ManifestRaw []byte
	// Man is the decoded manifest (for the itemized requests).
	Man *Manifest
	// Findings is the candidate's full scan result.
	Findings []Finding
	// NewFindings, on an upgrade, is NewSince(installed, candidate); each
	// renders with a (new) marker.
	NewFindings []Finding
	// BundledFiles is the package's bundled file list.
	BundledFiles []string
	// Readme is the package's README.md, rendered after the requests
	// (harness#929). The zero value renders as "no README".
	Readme Readme
	// ReadmeFull prints the whole README instead of capping it at
	// ReadmeMaxLines (--readme-full).
	ReadmeFull bool
}

// RenderReport writes the confirmation report: the manifest verbatim, the
// [requests] table as itemized lines with absent keys stated explicitly, the
// mcp_allow "write" sibling warning, the package README (sanitized and
// capped, harness#929), the findings as file:line pattern
// severity, the bundled files, and — every time, last — the no-guarantee
// statement (SPEC-0026 REQ-4, REQ-5).
func RenderReport(w io.Writer, in ReportInput) {
	fmt.Fprintf(w, "agent: %s\n", in.Ref)
	if in.Man != nil {
		fmt.Fprintf(w, "license: %s\n", LicenseLabel(in.Man.Package.License))
	}
	fmt.Fprintf(w, "manifest (verbatim):\n")
	for _, line := range strings.Split(strings.TrimSuffix(string(in.ManifestRaw), "\n"), "\n") {
		fmt.Fprintf(w, "  %s\n", line)
	}

	fmt.Fprintf(w, "requests:\n")
	for _, line := range RenderRequests(in.Man) {
		fmt.Fprintf(w, "  %s\n", line)
	}

	// The declared environment, names only (SPEC-0026 REQ-3, issue #930).
	fmt.Fprintf(w, "environment:\n")
	for _, line := range RenderEnv(in.Man) {
		fmt.Fprintf(w, "  %s\n", line)
	}

	// The README sits before the findings so that the findings and the
	// no-guarantee statement stay closest to the prompt (harness#929).
	RenderReadme(w, in.Readme, in.ReadmeFull)
	fmt.Fprintf(w, "scan findings:\n")
	isNew := make(map[Finding]bool, len(in.NewFindings))
	for _, f := range in.NewFindings {
		isNew[f] = true
	}
	if len(in.Findings) == 0 {
		fmt.Fprintf(w, "  none\n")
	}
	for _, f := range in.Findings {
		marker := ""
		if isNew[f] {
			marker = "  (new)"
		}
		fmt.Fprintf(w, "  %s  %s  %s%s\n", f.Where(), f.PatternID, f.Severity, marker)
	}

	fmt.Fprintf(w, "bundled files:\n")
	for _, f := range in.BundledFiles {
		fmt.Fprintf(w, "  %s\n", f)
	}
	fmt.Fprintf(w, "%s\n", NoGuarantee)
}

// RenderRequests itemizes the [requests] table, stating each absent key
// explicitly (SPEC-0026 REQ-4), and appends the sibling-control warning when
// mcp_allow carries "write" (ADR-0010).
func RenderRequests(man *Manifest) []string {
	var req Requests
	if man != nil {
		req = man.Requests
	}
	var lines []string
	switch {
	case req.SkillPaths == nil:
		lines = append(lines, "does not declare modifying skill paths")
	case *req.SkillPaths:
		lines = append(lines, "requests its skills directory on the operator's skill path")
	default:
		lines = append(lines, "explicitly declines the skill path")
	}
	if len(req.MCPAllow) == 0 {
		lines = append(lines, "does not declare any MCP scopes")
	} else {
		scopes := append([]string(nil), req.MCPAllow...)
		sort.Strings(scopes)
		for _, s := range scopes {
			lines = append(lines, fmt.Sprintf("requests MCP scope %q", s))
		}
	}
	switch {
	case req.Network == nil || !*req.Network:
		lines = append(lines, "does not declare needing network access")
	default:
		lines = append(lines, "requests network access")
	}
	if RequestsWrite(req) {
		lines = append(lines, `mcp_allow includes "write": the installed harness could start, stop, or restart its siblings`)
	}
	return lines
}

// RequestsWrite reports whether an mcp_allow scope carries write access.
func RequestsWrite(req Requests) bool {
	for _, s := range req.MCPAllow {
		if strings.Contains(strings.ToLower(s), "write") {
			return true
		}
	}
	return false
}

// Outcome is what the confirmation decision wants next.
type Outcome int

const (
	// Proceed: the gate is satisfied; install or upgrade may continue.
	Proceed Outcome = iota
	// Confirm: an interactive operator must answer the ordinary y/N prompt.
	Confirm
	// Retype: an interactive operator must retype <stable>/<package> at the
	// prompt; the caller reads it and decides again.
	Retype
	// Refuse: the gate blocks; the reason names why.
	Refuse
)

// Decision is the outcome of Decide with its reason.
type Decision struct {
	Outcome Outcome
	// Reason names why, including the file and pattern id of the blocking
	// finding when one exists (Error Handling Standards).
	Reason string
	// Blocked is true when a high-severity finding refused the gate; the
	// caller wraps the reason in ErrBlockedFinding.
	Blocked bool
}

// DecisionInput is everything the pure decision function sees. Interactive
// is whether stdin is a TTY; Retyped is the text typed at the retype prompt
// ("" when none was read yet).
type DecisionInput struct {
	Findings    []Finding
	Requests    Requests
	Yes         bool
	ForceUnsafe bool
	Interactive bool
	Retyped     string
	Ref         string
}

// Decide is the confirmation decision, pure and table-tested (SPEC-0026
// REQ-4, REQ-5). Precedence: a high-severity finding first, then an
// mcp_allow "write" request, then the ordinary --yes / interactive prompt.
// --yes never suppresses a high block; --force-unsafe never works
// unattended; the retype must match Ref exactly.
func Decide(in DecisionInput) Decision {
	var high *Finding
	for i := range in.Findings {
		if in.Findings[i].Severity == SeverityHigh {
			high = &in.Findings[i]
			break
		}
	}
	if high != nil {
		if !in.ForceUnsafe {
			return Decision{
				Outcome: Refuse,
				Reason: fmt.Sprintf("blocked by high-severity finding %s:%d (%s) — rerun with --force-unsafe to override it; --yes does not clear it",
					high.File, high.Line, high.PatternID),
				Blocked: true,
			}
		}
		if !in.Interactive {
			return Decision{
				Outcome: Refuse,
				Reason:  "an unattended session cannot override a high-severity finding; run interactively with --force-unsafe and retype " + in.Ref,
				Blocked: true,
			}
		}
		switch in.Retyped {
		case "":
			return Decision{Outcome: Retype, Reason: "retype " + in.Ref + " to override the high-severity finding"}
		case in.Ref:
			return Decision{Outcome: Proceed, Reason: "high-severity finding overridden with --force-unsafe (recorded in the install record)"}
		default:
			return Decision{
				Outcome: Refuse,
				Reason:  fmt.Sprintf("retyped text does not match %q", in.Ref),
				Blocked: true,
			}
		}
	}

	if RequestsWrite(in.Requests) {
		if !in.Interactive {
			return Decision{
				Outcome: Refuse,
				Reason:  `mcp_allow includes "write" and cannot be accepted unattended; run interactively and retype ` + in.Ref,
				Blocked: false,
			}
		}
		switch in.Retyped {
		case "":
			return Decision{Outcome: Retype, Reason: `retype ` + in.Ref + ` to accept the mcp_allow "write" request`}
		case in.Ref:
			return Decision{Outcome: Proceed, Reason: `mcp_allow "write" accepted with a typed confirmation`}
		default:
			return Decision{Outcome: Refuse, Reason: fmt.Sprintf("retyped text does not match %q", in.Ref)}
		}
	}

	if in.Yes {
		return Decision{Outcome: Proceed, Reason: "--yes"}
	}
	if !in.Interactive {
		return Decision{Outcome: Refuse, Reason: "confirmation required; rerun with --yes"}
	}
	return Decision{Outcome: Confirm, Reason: "show the report and ask y/N"}
}

// LogBlocked emits one structured key-value line per high-severity finding,
// carrying only the package, the path, the pattern id and the severity —
// never the scanned file's content (SPEC-0026 Error Handling Standards).
func LogBlocked(ref string, findings []Finding) {
	for _, f := range findings {
		if f.Severity != SeverityHigh {
			continue
		}
		clog.Warn("agent package scan blocked",
			"package", ref, "file", f.File, "pattern", f.PatternID, "severity", string(f.Severity))
	}
}

// InstallRecord is the durable record of one install, kept next to — never
// inside — the immutable pin directory, so writing it cannot imply the pin
// itself is writable. It retains an overridden finding and the fact that it
// was overridden (SPEC-0026 REQ-5); #813's install calls the writer.
type InstallRecord struct {
	// Source is the canonical "<stable>/<package>@<sha>".
	Source string `toml:"source"`
	// ScannerVersion is the pattern table version that produced Findings.
	ScannerVersion string `toml:"scanner_version"`
	// Findings is every finding the scan produced at install time.
	Findings []Finding `toml:"findings"`
	// OverriddenWithForceUnsafe is the recorded fact that a high-severity
	// finding was overridden with --force-unsafe and a typed retype.
	OverriddenWithForceUnsafe bool `toml:"overridden_with_force_unsafe"`
	// OverriddenAt is when the override was typed.
	OverriddenAt time.Time `toml:"overridden_at"`
}

// RecordPath is the install record's location, beside the pin directory
// rather than inside it: <installed>/<stable>/<package>/<sha>.record.toml.
func RecordPath(s Source) string {
	return filepath.Join(InstalledRoot(), s.Stable, s.Package, s.SHA+".record.toml")
}

// WriteInstallRecord writes the record atomically (temp file, rename) next
// to the pin directory. The pin directory itself is never touched.
func WriteInstallRecord(s Source, rec InstallRecord) error {
	dir := filepath.Dir(RecordPath(s))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("agentpkg: create record directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".record-*")
	if err != nil {
		return fmt.Errorf("agentpkg: temp record: %w", err)
	}
	defer os.Remove(tmp.Name())
	enc := toml.NewEncoder(tmp)
	if err := enc.Encode(rec); err != nil {
		tmp.Close()
		return fmt.Errorf("agentpkg: encode install record: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("agentpkg: close install record: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("agentpkg: chmod install record: %w", err)
	}
	if err := os.Rename(tmp.Name(), RecordPath(s)); err != nil {
		return fmt.Errorf("agentpkg: place install record: %w", err)
	}
	return nil
}

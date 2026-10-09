package scan

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/agentpkg"
)

// A manifest with no [package].license gets exactly one low
// package.no-license finding pointing at the [package] header; declaring a
// license clears it; it never blocks the gate; and an upgrade that keeps it
// does not report it as new (SPEC-0026 REQ-3, REQ-5). The manifest is the
// real LoadManifest result, as install and stable lint load it.
func TestNoLicenseFinding(t *testing.T) {
	const unlicensed = "# a comment first\n\n[package]\nname = \"x\"\n\n[harness]\nharness = \"crush\"\n"
	load := func(text string) (string, *agentpkg.Manifest) {
		dir := writePkg(t, map[string]string{"package.toml": text})
		man, err := agentpkg.LoadManifest(filepath.Join(dir, ManifestFile))
		if err != nil {
			t.Fatal(err)
		}
		return dir, man
	}

	dir, man := load(unlicensed)
	findings := scanPkg(t, dir, man)
	var hits []agentpkg.Finding
	for _, f := range findings {
		if f.PatternID == agentpkg.FindingNoLicense {
			hits = append(hits, f)
		}
	}
	if len(hits) != 1 {
		t.Fatalf("want one %s finding, got %v", agentpkg.FindingNoLicense, findings)
	}
	if f := hits[0]; f.File != ManifestFile || f.Line != 3 || f.Severity != agentpkg.SeverityLow {
		t.Fatalf("the finding must be low and point at the [package] header (package.toml:3), got %+v", f)
	}
	if d := agentpkg.Decide(agentpkg.DecisionInput{Findings: findings, Yes: true, Ref: "s/x"}); d.Outcome != agentpkg.Proceed {
		t.Fatalf("a missing license must never block: %+v", d)
	}

	// The header moving lines is not a new finding on upgrade.
	movedDir, movedMan := load("[package]\nname = \"x\"\n\n[harness]\nharness = \"crush\"\n")
	if fresh := agentpkg.NewSince(findings, scanPkg(t, movedDir, movedMan)); hasFinding(fresh, agentpkg.FindingNoLicense) {
		t.Fatalf("an unchanged missing license must not read as new: %v", fresh)
	}

	licensedDir, licensedMan := load(strings.Replace(unlicensed, "name = \"x\"\n", "name = \"x\"\nlicense = \"MIT\"\n", 1))
	if hasFinding(scanPkg(t, licensedDir, licensedMan), agentpkg.FindingNoLicense) {
		t.Fatal("a declared license must clear the finding")
	}
}

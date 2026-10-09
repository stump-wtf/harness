package agentpkg

// License Display
//
// The license in the install and upgrade confirmations: the report names
// it (or says none is declared), and the upgrade diff calls out a change
// between pins as a terms change (SPEC-0026 REQ-3, REQ-8).
//
// Governing: ADR-0044, SPEC-0026 REQ-3, REQ-8.
//
// @joestump-agent 10/09/2026 - Added for harness#932.

import (
	"strings"
	"testing"
)

// A license change between pins is its own diff row and says it is a terms
// change; an unchanged license adds no row (REQ-8).
func TestManifestChangesLicense(t *testing.T) {
	old := &Manifest{Package: PackageMeta{Name: "p", License: "MIT"}}
	nw := &Manifest{Package: PackageMeta{Name: "p", License: "GPL-3.0-only"}}
	got := strings.Join(ManifestChanges(old, nw), "\n")
	if !strings.Contains(got, "package.license: MIT -> GPL-3.0-only  (license change: the package's terms changed)") {
		t.Fatalf("the license change must be called out:\n%s", got)
	}

	added := strings.Join(ManifestChanges(&Manifest{Package: PackageMeta{Name: "p"}}, old), "\n")
	if !strings.Contains(added, "package.license: unset -> MIT") {
		t.Fatalf("a newly declared license must render from unset:\n%s", added)
	}
	dropped := strings.Join(ManifestChanges(old, &Manifest{Package: PackageMeta{Name: "p"}}), "\n")
	if !strings.Contains(dropped, "package.license: MIT -> unset") {
		t.Fatalf("a dropped license must render to unset:\n%s", dropped)
	}
	if same := ManifestChanges(old, old); len(same) != 0 {
		t.Fatalf("an unchanged license adds no row, got %v", same)
	}
}

// The install confirmation names the license, or says there is none.
func TestRenderReportLicense(t *testing.T) {
	var b strings.Builder
	RenderReport(&b, ReportInput{Ref: "s/p", Man: &Manifest{Package: PackageMeta{Name: "p", License: "Apache-2.0"}}})
	if !strings.Contains(b.String(), "license: Apache-2.0\n") {
		t.Fatalf("the report must show the license:\n%s", b.String())
	}
	b.Reset()
	RenderReport(&b, ReportInput{Ref: "s/p", Man: &Manifest{Package: PackageMeta{Name: "p"}}})
	if !strings.Contains(b.String(), "license: none declared (package.no-license)\n") {
		t.Fatalf("the report must say no license is declared:\n%s", b.String())
	}
}

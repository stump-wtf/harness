package agentpkg

// License
//
// SPEC-0026 REQ-3 coverage for [package].license: the SPDX expression
// grammar (AND, OR, WITH, parentheses, LicenseRef-*), unknown IDs named
// against the pinned list version, and the manifest loader's handling of a
// valid, unknown, custom, empty and non-string value through the real
// ParseManifest.
//
// Governing: ADR-0044, SPEC-0026 REQ-3.
//
// @joestump-agent 10/09/2026 - Added for harness#932.

import (
	"errors"
	"strings"
	"testing"
)

func TestValidateLicense(t *testing.T) {
	tests := []struct {
		expr    string
		wantErr string // "" = valid
	}{
		{expr: "MIT"},
		{expr: "Apache-2.0"},
		{expr: "mit"}, // IDs match case-insensitively (SPDX Annex D)
		{expr: "MIT OR Apache-2.0"},
		{expr: "MIT AND BSD-3-Clause"},
		{expr: "GPL-2.0-or-later WITH Classpath-exception-2.0"},
		{expr: "(MIT OR Apache-2.0) AND BSD-2-Clause"},
		{expr: "((MIT))"},
		{expr: "LGPL-2.1-only OR (GPL-2.0-only WITH Classpath-exception-2.0 AND MIT)"},
		{expr: "LicenseRef-Foo"},
		{expr: "LicenseRef-acme.internal-1"},
		{expr: "MIT OR LicenseRef-Foo"},
		{expr: "GPL-2.0+"}, // a deprecated ID that is itself on the list

		{expr: "", wantErr: "must not be empty"},
		{expr: "   ", wantErr: "must not be empty"},
		{expr: "NotARealLicense", wantErr: `unknown SPDX license ID "NotARealLicense"`},
		{expr: "MIT OR Bogus-1.0", wantErr: `unknown SPDX license ID "Bogus-1.0"`},
		{expr: "MIT+", wantErr: `"+" operator is not supported`},
		{expr: "GPL-2.0-only WITH Nope-exception", wantErr: `unknown SPDX license exception "Nope-exception"`},
		{expr: "MIT WITH", wantErr: "WITH must be followed by"},
		{expr: "MIT WITH LicenseRef-Foo", wantErr: "unknown SPDX license exception"},
		{expr: "mit or apache-2.0", wantErr: `operator "or" must be uppercase (OR)`},
		{expr: "MIT and Apache-2.0", wantErr: `operator "and" must be uppercase (AND)`},
		{expr: "MIT Apache-2.0", wantErr: `unexpected "Apache-2.0"`},
		{expr: "MIT OR", wantErr: "ends where a license ID was expected"},
		{expr: "AND MIT", wantErr: `unexpected "AND"`},
		{expr: "(MIT OR Apache-2.0", wantErr: `unclosed "("`},
		{expr: "MIT)", wantErr: `unexpected ")"`},
		{expr: "()", wantErr: `unexpected ")"`},
		{expr: "LicenseRef-", wantErr: "not a valid LicenseRef"},
		{expr: "DocumentRef-x:LicenseRef-y", wantErr: "DocumentRef-"},
		{expr: "MIT/Apache-2.0", wantErr: "not valid in an SPDX license expression"},
		{expr: strings.Repeat("(", 40) + "MIT" + strings.Repeat(")", 40), wantErr: "nests parentheses deeper"},
	}
	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			err := ValidateLicense(tt.expr)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("%q must be valid: %v", tt.expr, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("%q: want error containing %q, got %v", tt.expr, tt.wantErr, err)
			}
		})
	}
}

// An unknown ID names the pinned SPDX list version, so a "valid upstream,
// unknown here" report is diagnosable from the error alone.
func TestValidateLicenseNamesListVersion(t *testing.T) {
	err := ValidateLicense("Bogus-1.0")
	if err == nil || !strings.Contains(err.Error(), "v"+SPDXListVersion) {
		t.Fatalf("the error must name the pinned list version %s, got %v", SPDXListVersion, err)
	}
}

// The vendored list is the real SPDX list, not an empty stub: well-known
// IDs and exceptions are present, and the generated version is set.
func TestVendoredSPDXList(t *testing.T) {
	if SPDXListVersion == "" {
		t.Fatal("SPDXListVersion must be set by the generator")
	}
	if len(spdxLicenseIDs) < 500 || len(spdxExceptionIDs) < 40 {
		t.Fatalf("the vendored list looks truncated: %d licenses, %d exceptions", len(spdxLicenseIDs), len(spdxExceptionIDs))
	}
	lic, exc := spdxIndex()
	for _, id := range []string{"MIT", "Apache-2.0", "GPL-3.0-only", "BSD-3-Clause", "0BSD", "Unlicense"} {
		if !lic[strings.ToLower(id)] {
			t.Errorf("license %s missing from the vendored list", id)
		}
	}
	for _, id := range []string{"Classpath-exception-2.0", "LLVM-exception"} {
		if !exc[strings.ToLower(id)] {
			t.Errorf("exception %s missing from the vendored list", id)
		}
	}
}

// The manifest loader: a valid single ID, a valid expression and a
// LicenseRef load; an unknown ID, an empty string and a non-string value
// fail as schema violations naming the key. No license at all loads (it is
// a low finding, not an error).
func TestParseManifestLicense(t *testing.T) {
	manifest := func(licenseLine string) string {
		return "[package]\nname = \"x\"\n" + licenseLine + "\n[harness]\nharness = \"crush\"\n"
	}
	tests := []struct {
		name    string
		line    string
		want    string // the decoded License on success
		wantErr string // "" = loads
	}{
		{name: "single id", line: `license = "MIT"`, want: "MIT"},
		{name: "expression", line: `license = "MIT OR Apache-2.0"`, want: "MIT OR Apache-2.0"},
		{name: "with exception", line: `license = "GPL-2.0-only WITH Classpath-exception-2.0"`, want: "GPL-2.0-only WITH Classpath-exception-2.0"},
		{name: "license ref", line: `license = "LicenseRef-Foo"`, want: "LicenseRef-Foo"},
		{name: "absent", line: ``, want: ""},
		{name: "unknown id", line: `license = "Bogus-1.0"`, wantErr: `[package].license: unknown SPDX license ID "Bogus-1.0"`},
		{name: "unknown id in expression", line: `license = "MIT OR Bogus-1.0"`, wantErr: `"Bogus-1.0"`},
		{name: "empty", line: `license = ""`, wantErr: "[package].license: must not be empty"},
		{name: "integer", line: `license = 1`, wantErr: "[package].license must be a string"},
		{name: "list", line: `license = ["MIT"]`, wantErr: "[package].license must be a string"},
		{name: "table", line: "[package.license]\nid = \"MIT\"", wantErr: "[package].license must be a string"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m, err := ParseManifest([]byte(manifest(tt.line)), "stables/s/packages/x/package.toml")
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("must load: %v", err)
				}
				if m.Package.License != tt.want {
					t.Fatalf("license = %q, want %q", m.Package.License, tt.want)
				}
				return
			}
			if !errors.Is(err, ErrManifestViolation) {
				t.Fatalf("want ErrManifestViolation, got %v", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) || !strings.Contains(err.Error(), "stables/s/packages/x/package.toml") {
				t.Fatalf("want error naming %q and the manifest path, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestLicenseLabel(t *testing.T) {
	if got := LicenseLabel("MIT OR Apache-2.0"); got != "MIT OR Apache-2.0" {
		t.Errorf("a declared license renders as written, got %q", got)
	}
	if got := LicenseLabel(""); got != "none declared (package.no-license)" {
		t.Errorf("an absent license says so and names the finding, got %q", got)
	}
}

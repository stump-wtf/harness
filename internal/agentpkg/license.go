// The [package].license validator: an SPDX license expression checked
// against the SPDX License List vendored at a pinned version (spdx_list.go).
// A package is someone else's prompt and configuration run on the
// installing machine, so whether it may be used, modified and redistributed
// is stated in the manifest and shown at install, not guessed from a
// stable's top-level LICENSE.
//
// The expression grammar is deliberately the useful core of SPDX Annex D:
//
//	expr   = and *( "OR" and )
//	and    = term *( "AND" term )
//	term   = "(" expr ")" / simple [ "WITH" exception ]
//	simple = license-id / "LicenseRef-" idstring
//
// so WITH binds tighter than AND, and AND tighter than OR. IDs match the
// list case-insensitively (SPDX's own matching rule); the operators must be
// uppercase. The "+" operator, DocumentRef- and anything else outside that
// grammar is a load error, never a silent pass — an unknown ID fails the
// load naming it.
//
// Governing: ADR-0044 (agent package stables), SPEC-0026 REQ-3 (manifest
// schema: [package].license), REQ-5 (the package.no-license low finding).
//
// @joestump-agent 10/09/2026 - Added for harness#932.
package agentpkg

//go:generate go run ./internal/spdxgen -version v3.29.0 -out spdx_list.go

import (
	"fmt"
	"strings"
	"sync"
)

// FindingNoLicense is the pattern id of the low finding a package without a
// [package].license gets (SPEC-0026 REQ-5): shown at info, install and
// upgrade, never blocking. `harness agent stable lint` promotes it to an
// error.
const FindingNoLicense = "package.no-license"

// LicenseLabel renders a declared license for info, the install and upgrade
// confirmations and `harness agent list`: the expression as written, or a
// statement that the package declares none.
func LicenseLabel(expr string) string {
	if expr == "" {
		return "none declared (" + FindingNoLicense + ")"
	}
	return expr
}

// maxLicenseDepth bounds parenthesis nesting so a hostile manifest cannot
// drive the recursive parser arbitrarily deep.
const maxLicenseDepth = 32

// licenseRefPrefix opens a custom-terms identifier (SPDX "LicenseRef-").
const licenseRefPrefix = "LicenseRef-"

// spdxIndex is the case-folded lookup over the vendored lists, built once.
var spdxIndex = sync.OnceValues(func() (map[string]bool, map[string]bool) {
	lic := make(map[string]bool, len(spdxLicenseIDs))
	for _, id := range spdxLicenseIDs {
		lic[strings.ToLower(id)] = true
	}
	exc := make(map[string]bool, len(spdxExceptionIDs))
	for _, id := range spdxExceptionIDs {
		exc[strings.ToLower(id)] = true
	}
	return lic, exc
})

// ValidateLicense checks one SPDX license expression. The error names the
// offending token and, for an unknown ID, the pinned list version; callers
// wrap it with the manifest path and key.
func ValidateLicense(expr string) error {
	toks, err := lexLicense(expr)
	if err != nil {
		return err
	}
	if len(toks) == 0 {
		return fmt.Errorf("must not be empty; want an SPDX license expression such as \"MIT\" or \"MIT OR Apache-2.0\"")
	}
	p := &licenseParser{toks: toks}
	if err := p.expr(0); err != nil {
		return err
	}
	if p.pos < len(p.toks) {
		tok := p.toks[p.pos]
		if up := strings.ToUpper(tok); isLicenseOperator(up) {
			return fmt.Errorf("operator %q must be uppercase (%s)", tok, up)
		}
		return fmt.Errorf("unexpected %q; join license IDs with AND, OR or WITH", tok)
	}
	return nil
}

// lexLicense splits an expression into parentheses and words. A word is
// the SPDX idstring alphabet (letters, digits, ".", "-") plus "+", which
// only appears in the list's own deprecated "GPL-2.0+"-style IDs.
func lexLicense(expr string) ([]string, error) {
	var toks []string
	word := strings.Builder{}
	flush := func() {
		if word.Len() > 0 {
			toks = append(toks, word.String())
			word.Reset()
		}
	}
	for _, r := range expr {
		switch {
		case r == ' ' || r == '\t':
			flush()
		case r == '(' || r == ')':
			flush()
			toks = append(toks, string(r))
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '-', r == '+':
			word.WriteRune(r)
		case r == ':':
			return nil, fmt.Errorf("DocumentRef- identifiers are not supported; use LicenseRef-<name> for custom terms")
		default:
			return nil, fmt.Errorf("contains %q, which is not valid in an SPDX license expression", r)
		}
	}
	flush()
	return toks, nil
}

type licenseParser struct {
	toks []string
	pos  int
}

func (p *licenseParser) peek() string {
	if p.pos < len(p.toks) {
		return p.toks[p.pos]
	}
	return ""
}

func (p *licenseParser) expr(depth int) error {
	if depth > maxLicenseDepth {
		return fmt.Errorf("nests parentheses deeper than %d", maxLicenseDepth)
	}
	if err := p.and(depth); err != nil {
		return err
	}
	for p.peek() == "OR" {
		p.pos++
		if err := p.and(depth); err != nil {
			return err
		}
	}
	return nil
}

func (p *licenseParser) and(depth int) error {
	if err := p.term(depth); err != nil {
		return err
	}
	for p.peek() == "AND" {
		p.pos++
		if err := p.term(depth); err != nil {
			return err
		}
	}
	return nil
}

func (p *licenseParser) term(depth int) error {
	tok := p.peek()
	switch tok {
	case "":
		return fmt.Errorf("ends where a license ID was expected")
	case "(":
		p.pos++
		if err := p.expr(depth + 1); err != nil {
			return err
		}
		if p.peek() != ")" {
			return fmt.Errorf("has an unclosed \"(\"")
		}
		p.pos++
		return nil
	case ")", "AND", "OR", "WITH":
		return fmt.Errorf("unexpected %q where a license ID was expected", tok)
	}
	if err := checkLicenseID(tok); err != nil {
		return err
	}
	p.pos++
	if p.peek() == "WITH" {
		p.pos++
		exc := p.peek()
		if exc == "" || exc == "(" || exc == ")" || isLicenseOperator(exc) {
			return fmt.Errorf("WITH must be followed by an SPDX license exception ID")
		}
		if _, known := spdxIndex(); !known[strings.ToLower(exc)] {
			return fmt.Errorf("unknown SPDX license exception %q after WITH (not on the SPDX License List v%s)", exc, SPDXListVersion)
		}
		p.pos++
	}
	return nil
}

// checkLicenseID accepts a listed SPDX license ID (any case) or a
// LicenseRef-<idstring>; it names anything else.
func checkLicenseID(id string) error {
	if up := strings.ToUpper(id); isLicenseOperator(up) {
		return fmt.Errorf("operator %q must be uppercase (%s)", id, up)
	}
	if ref, ok := strings.CutPrefix(id, licenseRefPrefix); ok {
		if ref == "" || strings.Contains(ref, "+") {
			return fmt.Errorf("%q is not a valid LicenseRef; want LicenseRef- followed by letters, digits, \".\" or \"-\"", id)
		}
		return nil
	}
	if known, _ := spdxIndex(); known[strings.ToLower(id)] {
		return nil
	}
	if strings.HasSuffix(id, "+") {
		return fmt.Errorf("unknown SPDX license ID %q: the \"+\" operator is not supported; name the license, e.g. \"GPL-2.0-or-later\"", id)
	}
	return fmt.Errorf("unknown SPDX license ID %q (not on the SPDX License List v%s; use LicenseRef-<name> for custom terms)", id, SPDXListVersion)
}

func isLicenseOperator(tok string) bool {
	return tok == "AND" || tok == "OR" || tok == "WITH"
}

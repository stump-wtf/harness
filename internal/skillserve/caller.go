// Caller identity and serve_to scoping: who may search which skill repo.
//
// The caller itself is attributed by SPEC-0005 REQ "Caller Identity" (the
// daemon attributes every facade call to the harness that owns the presented
// token); this file only answers what a skill repo's serve_to selectors say
// about that caller.
//
// Governing: SPEC-0007 REQ "Search And Retrieval Tools", REQ "Skill Repos";
// ADR-0030.
package skillserve

import (
	"path"
	"strings"
)

// Caller is the identity a retrieval is scoped against.
type Caller struct {
	// Harness is the qualified harness name the call is attributed to
	// ("reduit/agent"), empty for an unattributed caller.
	Harness string
	// Run is the run id the retrieval count is recorded against; it is not
	// part of the scoping decision.
	Run string
}

// Unattributed reports whether the call carries no attributed harness: a
// session started outside Harness, or one whose token did not resolve.
func (c Caller) Unattributed() bool { return strings.TrimSpace(c.Harness) == "" }

// ServesTo reports whether a skill repo's serve_to selectors admit the caller.
// The bare "*" matches every harness, unattributed callers included; any other
// selector is a glob over qualified harness names in which "*" does not match
// "/", so "reduit/*" never reaches "reduit/one/two" and never reaches an
// unattributed caller (SPEC-0007 REQ "Skill Repos").
func ServesTo(serveTo []string, c Caller) bool {
	for _, sel := range serveTo {
		sel = strings.TrimSpace(sel)
		if sel == "" {
			continue
		}
		if sel == "*" {
			return true
		}
		if c.Unattributed() {
			continue
		}
		if ok, err := path.Match(sel, c.Harness); err == nil && ok {
			return true
		}
	}
	return false
}

// servedNames returns the declared repos the caller may search, in declaration
// order. An empty result means the caller is scoped out of every repo, which
// is an empty answer rather than an error. Callers hold m.mu.
func (m *Manager) servedNames(c Caller) []string {
	out := make([]string, 0, len(m.repos))
	for _, r := range m.repos {
		if ServesTo(r.ServeToOrDefault(), c) {
			out = append(out, r.Name)
		}
	}
	return out
}

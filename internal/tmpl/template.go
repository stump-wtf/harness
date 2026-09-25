// Package tmpl is the closed placeholder grammar behind templated argv and
// prompts: a hand-written scanner for exactly four forms, a parsed Template
// whose references config validation can check by location, a Render that
// never splits, trims or re-quotes a value, and the untrusted fence.
//
// TL;DR: `{{path}}` is required, `{{path?}}` is optional, `{{untrusted path}}`
// is a fenced free-text field, `{{literal_open}}` is the two characters `{{`.
// Nothing else is special. There are no functions, filters, pipelines,
// conditionals or loops, so the set of values a template can reach is exactly
// the set of paths it names, and Refs() lists them.
//
// The package depends on nothing in this module. Config validation calls Parse
// and Refs at load; the supervisor calls Render at spawn with a Context it
// builds from the harness, the run and the event's typed values. Which paths
// exist, and which are allowed where, is the caller's business: the grammar
// only guarantees that a template cannot name anything its Refs do not show.
//
// Governing: ADR-0023 (command one-shots and templating), SPEC-0017 REQ-6
// "Template Grammar", REQ-10 "Untrusted Free Text" (fence format), REQ-11
// "Rendering", REQ "Error Handling Standards"; design.md § "A new
// internal/tmpl package owns grammar, context and rendering".
//
// @joestump-agent 09/23/2026 - Added for #501.
package tmpl

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors for the two failure modes SPEC-0017 REQ "Error Handling
// Standards" says callers distinguish. The located detail rides on
// *GrammarError and *UnresolvedError, which unwrap to these, so a caller can
// errors.Is the category and errors.As the detail without matching text.
var (
	// ErrGrammar is a template that does not parse: a `{{` that does not begin
	// a well-formed placeholder.
	ErrGrammar = errors.New("template grammar error")
	// ErrUnresolved is a required `{{path}}` whose value is absent at render
	// time.
	ErrUnresolved = errors.New("unresolved template value")
)

// GrammarError is a parse failure located in the template source. Line and
// Col are 1-based; Col counts runes, not bytes, so it matches what an editor
// shows. The caller wraps it with the harness name and the config file and
// line the template came from.
type GrammarError struct {
	Line int
	Col  int
	Msg  string
}

func (e *GrammarError) Error() string {
	return fmt.Sprintf("template line %d, column %d: %s", e.Line, e.Col, e.Msg)
}

// Unwrap makes errors.Is(err, ErrGrammar) true.
func (e *GrammarError) Unwrap() error { return ErrGrammar }

// UnresolvedError is a required placeholder whose path the Context does not
// hold. It carries the path's name and never a value: SPEC-0017 REQ-11 records
// the path on a skipped run, and nothing rendered is ever persisted.
type UnresolvedError struct {
	Path string
}

func (e *UnresolvedError) Error() string {
	return fmt.Sprintf("template value %q is not available for this run", e.Path)
}

// Unwrap makes errors.Is(err, ErrUnresolved) true.
func (e *UnresolvedError) Unwrap() error { return ErrUnresolved }

// Kind is the form a placeholder was written in.
type Kind int

const (
	// Required is `{{path}}`: rendering fails with ErrUnresolved when absent.
	Required Kind = iota
	// Optional is `{{path?}}`: renders empty when absent.
	Optional
	// Untrusted is `{{untrusted path}}`: renders a fenced block (REQ-10).
	Untrusted
)

func (k Kind) String() string {
	switch k {
	case Required:
		return "required"
	case Optional:
		return "optional"
	case Untrusted:
		return "untrusted"
	}
	return fmt.Sprintf("Kind(%d)", int(k))
}

// Ref is one placeholder reference, located so a config error can point at
// it. `{{literal_open}}` is not a reference and never appears here.
type Ref struct {
	Path string
	Kind Kind
	Line int // 1-based line of the placeholder's `{{`
	Col  int // 1-based rune column of the placeholder's `{{`
}

// segment is either literal text (path == "") or one placeholder.
type segment struct {
	lit string
	ref Ref
}

func (s segment) isLiteral() bool { return s.ref.Path == "" }

// Template is a parsed template. The zero value is the empty template, which
// renders "" and has no references.
type Template struct {
	src  string
	segs []segment
}

// Source returns the template exactly as written, for surfaces such as
// `harness describe` that show the template rather than a rendering.
func (t Template) Source() string { return t.src }

// Refs returns every placeholder reference in source order, one entry per
// occurrence, so a caller checking an allow set can report each bad use at
// its own location.
func (t Template) Refs() []Ref {
	var refs []Ref
	for _, s := range t.segs {
		if !s.isLiteral() {
			refs = append(refs, s.ref)
		}
	}
	return refs
}

// Context supplies the values a template renders against. The two lookups
// are deliberately separate namespaces: Lookup answers `{{path}}` and
// `{{path?}}`, LookupUntrusted answers only `{{untrusted path}}`. A Context
// that keeps free text out of Lookup makes a bare `{{event.title}}`
// unresolvable at render time, not merely rejected at config load, so text an
// outside party wrote can only ever leave this package inside a fence.
type Context interface {
	Lookup(path string) (value string, ok bool)
	LookupUntrusted(path string) (value string, ok bool)
}

// SourcePath is the Context path whose value fills the fence's source
// attribute: the SPEC-0014 source that delivered the event.
const SourcePath = "event.source"

// Map is a Context backed by two maps. A nil map holds nothing.
type Map struct {
	Values    map[string]string
	Untrusted map[string]string
}

// Lookup implements Context.
func (m Map) Lookup(path string) (string, bool) {
	v, ok := m.Values[path]
	return v, ok
}

// LookupUntrusted implements Context.
func (m Map) LookupUntrusted(path string) (string, bool) {
	v, ok := m.Untrusted[path]
	return v, ok
}

// Render expands the template against ctx. Each value is inserted verbatim:
// never split, trimmed, globbed, re-quoted or re-parsed, so a value holding
// `{{`, quotes, `$()` or a leading `-` lands as those exact bytes (REQ-11).
// A required path that ctx lacks returns *UnresolvedError and no output.
func (t Template) Render(ctx Context) (string, error) {
	var b strings.Builder
	for _, s := range t.segs {
		if s.isLiteral() {
			b.WriteString(s.lit)
			continue
		}
		switch s.ref.Kind {
		case Required:
			v, ok := ctx.Lookup(s.ref.Path)
			if !ok {
				return "", &UnresolvedError{Path: s.ref.Path}
			}
			b.WriteString(v)
		case Optional:
			v, _ := ctx.Lookup(s.ref.Path)
			b.WriteString(v)
		case Untrusted:
			v, ok := ctx.LookupUntrusted(s.ref.Path)
			src, _ := ctx.Lookup(SourcePath)
			f, err := Fence(src, s.ref.Path, v, ok)
			if err != nil {
				return "", err
			}
			b.WriteString(f)
		}
	}
	return b.String(), nil
}

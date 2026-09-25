package tmpl

// Governing: ADR-0023 (command one-shots and templating), SPEC-0017 REQ-6
// "Template Grammar"; design.md § "A new internal/tmpl package owns grammar,
// context and rendering" (hand-written scanner, not text/template).
//
// The scanner is a single left-to-right pass. Every `{{` must begin one of the
// four placeholder forms, closed by the first `}}` after it; anything else is
// a located grammar error. A `}}` with no `{{` before it is literal text.
//
// @joestump-agent 09/23/2026 - Added for #501.

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	openDelim  = "{{"
	closeDelim = "}}"

	kwUntrusted   = "untrusted"
	kwLiteralOpen = "literal_open"

	// maxQuoted bounds how much of a malformed placeholder an error quotes,
	// so one runaway `{{` in a long prompt file does not make a wall of text.
	maxQuoted = 40
)

// Parse scans s into a Template. It returns a *GrammarError, located at the
// offending `{{`, for a `{{` with no closing `}}`, or a placeholder body that
// is not exactly one of `path`, `path?`, `untrusted path` or `literal_open`
// (with optional whitespace just inside the braces).
func Parse(s string) (Template, error) {
	t := Template{src: s}
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			t.segs = append(t.segs, segment{lit: lit.String()})
			lit.Reset()
		}
	}

	i := 0
	for i < len(s) {
		j := strings.Index(s[i:], openDelim)
		if j < 0 {
			lit.WriteString(s[i:])
			break
		}
		at := i + j
		lit.WriteString(s[i:at])

		bodyStart := at + len(openDelim)
		k := strings.Index(s[bodyStart:], closeDelim)
		if k < 0 {
			return Template{}, grammarError(s, at,
				`"{{" does not begin a placeholder: no closing "}}" (write {{literal_open}} for literal braces)`)
		}
		body := s[bodyStart : bodyStart+k]
		i = bodyStart + k + len(closeDelim)

		kind, path, err := parseBody(body)
		if err != "" {
			return Template{}, grammarError(s, at, err)
		}
		if path == "" { // {{literal_open}}
			lit.WriteString(openDelim)
			continue
		}
		flush()
		line, col := position(s, at)
		t.segs = append(t.segs, segment{ref: Ref{Path: path, Kind: kind, Line: line, Col: col}})
	}
	flush()
	return t, nil
}

// parseBody classifies the text between `{{` and `}}`. It returns an empty
// path for `literal_open`, and a non-empty message when the body is not one
// of the four forms.
func parseBody(body string) (Kind, string, string) {
	b := strings.Trim(body, " \t\r\n")
	malformed := func() string {
		return fmt.Sprintf("malformed placeholder {{%s}}: expected {{path}}, {{path?}}, "+
			"{{untrusted path}} or {{literal_open}}; templates have no functions, filters or pipelines",
			quoteTrunc(body))
	}

	switch {
	case b == kwLiteralOpen:
		return 0, "", ""
	case b == kwUntrusted:
		return 0, "", fmt.Sprintf("malformed placeholder {{%s}}: untrusted needs a field path, as in {{untrusted event.title}}",
			quoteTrunc(body))
	case strings.HasPrefix(b, kwUntrusted) && isSpace(b[len(kwUntrusted)]):
		p := strings.TrimLeft(b[len(kwUntrusted):], " \t\r\n")
		if !validPath(p) {
			return 0, "", malformed()
		}
		return Untrusted, p, ""
	}

	kind, p := Required, b
	if strings.HasSuffix(b, "?") {
		kind, p = Optional, b[:len(b)-1]
	}
	if !validPath(p) {
		return 0, "", malformed()
	}
	if p == kwUntrusted || p == kwLiteralOpen {
		// {{untrusted?}} and {{literal_open?}}: reserved words, never paths.
		return 0, "", malformed()
	}
	return kind, p, ""
}

// validPath reports whether p matches ^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)*$.
// Hand-rolled rather than a regexp so the scanner stays allocation-light and
// obviously total under fuzzing.
func validPath(p string) bool {
	if p == "" {
		return false
	}
	segStart := true
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case segStart:
			if c < 'a' || c > 'z' {
				return false
			}
			segStart = false
		case c == '.':
			segStart = true
		case (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_':
		default:
			return false
		}
	}
	return !segStart // a trailing '.' leaves an empty last segment
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

// position converts a byte offset into a 1-based line and rune column.
func position(s string, off int) (int, int) {
	pre := s[:off]
	line := strings.Count(pre, "\n") + 1
	lineStart := strings.LastIndexByte(pre, '\n') + 1
	return line, utf8.RuneCountInString(pre[lineStart:]) + 1
}

func grammarError(s string, off int, msg string) *GrammarError {
	line, col := position(s, off)
	return &GrammarError{Line: line, Col: col, Msg: msg}
}

// quoteTrunc shortens a placeholder body for an error message, on a rune
// boundary, marking the cut.
func quoteTrunc(s string) string {
	if len(s) <= maxQuoted {
		return s
	}
	cut := maxQuoted
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

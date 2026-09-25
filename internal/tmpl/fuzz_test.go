package tmpl

// Governing: ADR-0023, SPEC-0017 REQ-6 "Template Grammar"; design.md § "A new
// internal/tmpl package owns grammar, context and rendering" — a dedicated
// package "is easy to fuzz". `make fuzz` runs this for a bounded time, and
// `make check` (the CI gate) runs `make fuzz`.
//
// @joestump-agent 09/23/2026 - Added for #501.

import (
	"errors"
	"strings"
	"testing"
)

// FuzzParse feeds arbitrary input to Parse and holds it to its contract:
// never panic; reject only with a located *GrammarError; and, whatever it
// accepts, yield well-formed located refs and render without error against a
// context that resolves every path, with one fence per untrusted ref.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
		"",
		"plain text",
		"{{event.repo}}",
		"{{event.number?}}",
		"{{untrusted event.title}}",
		"{{literal_open}}",
		"use {{literal_open}}x}} syntax",
		"{{ harness.name }}\n{{\tmodel? }}",
		`{{printf "%s" event.repo}}`,
		"{{",
		"}}",
		"{{}}",
		"{{{x}}}",
		"{{untrusted}}",
		"{{untrusted event.title?}}",
		"a {{b}} c {{d?}} e {{untrusted f.g}} h",
		"é{{日本}}",
		"{{a.b.c.d_1}}}}",
		"\xff{{x}}\x00",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		tm, err := Parse(s)
		if err != nil {
			var ge *GrammarError
			if !errors.As(err, &ge) || !errors.Is(err, ErrGrammar) {
				t.Fatalf("Parse(%q) returned a non-grammar error %T: %v", s, err, err)
			}
			if ge.Line < 1 || ge.Col < 1 {
				t.Fatalf("Parse(%q) error not located: %+v", s, ge)
			}
			return
		}

		values := map[string]string{}
		untrusted := map[string]string{}
		nUntrusted := 0
		for _, r := range tm.Refs() {
			if !validPath(r.Path) || r.Line < 1 || r.Col < 1 {
				t.Fatalf("Parse(%q) produced a bad ref %+v", s, r)
			}
			values[r.Path] = "V"
			untrusted[r.Path] = "U"
			if r.Kind == Untrusted {
				nUntrusted++
			}
		}
		out, err := tm.Render(Map{Values: values, Untrusted: untrusted})
		if err != nil {
			t.Fatalf("Render of accepted %q with every path resolved: %v", s, err)
		}
		if got := strings.Count(out, "\n</untrusted-data nonce=\""); got < nUntrusted {
			t.Fatalf("Render(%q): %d fences for %d untrusted refs", s, got, nUntrusted)
		}
	})
}

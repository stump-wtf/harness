package tmpl

// Governing: ADR-0023, SPEC-0017 REQ-6 "Template Grammar" — scenarios
// "Literal braces" and "No functions", and the located-error rule.
//
// @joestump-agent 09/23/2026 - Added for #501.

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestParseForms(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want []Ref
	}{
		{"required", "{{event.repo}}", []Ref{{Path: "event.repo", Kind: Required, Line: 1, Col: 1}}},
		{"optional", "x {{event.meta.queue?}}", []Ref{{Path: "event.meta.queue", Kind: Optional, Line: 1, Col: 3}}},
		{"untrusted", "{{untrusted event.title}}", []Ref{{Path: "event.title", Kind: Untrusted, Line: 1, Col: 1}}},
		{"inner whitespace ignored", "{{  run.id \t}}{{\n untrusted   event.body\n}}{{ model? }}", []Ref{
			{Path: "run.id", Kind: Required, Line: 1, Col: 1},
			{Path: "event.body", Kind: Untrusted, Line: 1, Col: 15},
			{Path: "model", Kind: Optional, Line: 3, Col: 3},
		}},
		{"digits and underscores", "{{a_1.b2_c}}", []Ref{{Path: "a_1.b2_c", Kind: Required, Line: 1, Col: 1}}},
		{"paths beginning with a keyword", "{{untrusted_x}}{{literal_open.y?}}", []Ref{
			{Path: "untrusted_x", Kind: Required, Line: 1, Col: 1},
			{Path: "literal_open.y", Kind: Optional, Line: 1, Col: 16},
		}},
		{"rune columns and lines", "héllo\n  wörld {{harness.name}}", []Ref{{Path: "harness.name", Kind: Required, Line: 2, Col: 9}}},
		{"no placeholders", "plain }} text } {", nil},
		{"literal_open is not a ref", "{{literal_open}}{{ literal_open }}", nil},
		{"empty", "", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tm, err := Parse(tc.src)
			if err != nil {
				t.Fatalf("Parse(%q): %v", tc.src, err)
			}
			if got := tm.Refs(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("Refs() = %+v, want %+v", got, tc.want)
			}
			if tm.Source() != tc.src {
				t.Fatalf("Source() = %q, want %q", tm.Source(), tc.src)
			}
		})
	}
}

// REQ-6 Scenario "Literal braces", plus the lone-`}}` rule.
func TestLiteralBraces(t *testing.T) {
	for src, want := range map[string]string{
		"use {{literal_open}}x}} syntax":   "use {{x}} syntax",
		"}} and }}}":                       "}} and }}}",
		"{{literal_open}}{{literal_open}}": "{{{{",
		"a {{ literal_open }} b":           "a {{ b",
		"{{x}}}":                           "X}",
	} {
		tm, err := Parse(src)
		if err != nil {
			t.Fatalf("Parse(%q): %v", src, err)
		}
		got, err := tm.Render(Map{Values: map[string]string{"x": "X"}})
		if err != nil {
			t.Fatalf("Render(%q): %v", src, err)
		}
		if got != want {
			t.Errorf("Render(%q) = %q, want %q", src, got, want)
		}
	}
}

func TestParseGrammarErrors(t *testing.T) {
	cases := []struct {
		name      string
		src       string
		line, col int
		msgHas    string
	}{
		// REQ-6 Scenario "No functions".
		{"no functions", `argv {{printf "%s" event.repo}}`, 1, 6, "no functions"},
		{"stray open", "a {{ b", 1, 3, `no closing "}}"`},
		{"stray open later line", "ok {{x}}\n  {{", 2, 3, `no closing "}}"`},
		{"empty", "{{}}", 1, 1, "malformed"},
		{"blank", "{{   }}", 1, 1, "malformed"},
		{"uppercase", "{{Event.repo}}", 1, 1, "malformed"},
		{"leading digit", "{{1x}}", 1, 1, "malformed"},
		{"trailing dot", "{{event.}}", 1, 1, "malformed"},
		{"double dot", "{{event..repo}}", 1, 1, "malformed"},
		{"hyphen", "{{event-repo}}", 1, 1, "malformed"},
		{"space before ?", "{{event.number ?}}", 1, 1, "malformed"},
		{"double ?", "{{event.number??}}", 1, 1, "malformed"},
		{"pipeline", "{{event.repo | upper}}", 1, 1, "malformed"},
		{"conditional", "{{if event.repo}}", 1, 1, "malformed"},
		{"dot root", "{{.Event}}", 1, 1, "malformed"},
		{"nested open", "{{ {{x}} }}", 1, 1, "malformed"},
		{"triple open", "{{{x}}", 1, 1, "malformed"},
		{"bare untrusted", "{{untrusted}}", 1, 1, "needs a field path"},
		{"optional untrusted", "{{untrusted event.title?}}", 1, 1, "malformed"},
		{"untrusted bad path", "{{untrusted Event}}", 1, 1, "malformed"},
		{"untrusted two paths", "{{untrusted a b}}", 1, 1, "malformed"},
		{"reserved optional untrusted", "{{untrusted?}}", 1, 1, "malformed"},
		{"reserved optional literal", "{{literal_open?}}", 1, 1, "malformed"},
		{"rune column", "ü {{Bad}}", 1, 3, "malformed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse(tc.src)
			if err == nil {
				t.Fatalf("Parse(%q) = nil error, want a grammar error", tc.src)
			}
			if !errors.Is(err, ErrGrammar) {
				t.Fatalf("errors.Is(%v, ErrGrammar) = false", err)
			}
			var ge *GrammarError
			if !errors.As(err, &ge) {
				t.Fatalf("errors.As(%v, *GrammarError) = false", err)
			}
			if ge.Line != tc.line || ge.Col != tc.col {
				t.Fatalf("located at %d:%d, want %d:%d (%v)", ge.Line, ge.Col, tc.line, tc.col, err)
			}
			if !strings.Contains(err.Error(), tc.msgHas) {
				t.Fatalf("error %q does not mention %q", err, tc.msgHas)
			}
		})
	}
}

// A long malformed body is quoted, not dumped, and the cut lands on a rune.
func TestGrammarErrorQuotesBoundedBody(t *testing.T) {
	body := strings.Repeat("é", 100)
	_, err := Parse("{{" + body + "}}")
	if err == nil {
		t.Fatal("want a grammar error")
	}
	msg := err.Error()
	if strings.Contains(msg, body) {
		t.Fatalf("error quotes the whole %d-byte body", len(body))
	}
	if !strings.Contains(msg, "…") || strings.ContainsRune(msg, '�') {
		t.Fatalf("error %q is not cut cleanly on a rune boundary", msg)
	}
}

// Unknown-path checking is the caller's, done through Refs() against an allow
// set that depends on where the template sits (REQ-6, REQ-7, REQ-10). This is
// the shape config validation takes: each disallowed ref reported at its own
// location.
func TestRefsAgainstAllowSet(t *testing.T) {
	argvAllow := map[string]bool{"harness.name": true, "event.repo": true, "event.number": true, "prompt": true}
	check := func(tm Template) []string {
		var bad []string
		for _, r := range tm.Refs() {
			switch {
			case r.Kind == Untrusted:
				bad = append(bad, "untrusted text is never permitted in argv: "+r.Path)
			case !argvAllow[r.Path]:
				bad = append(bad, "unknown path: "+r.Path)
			}
		}
		return bad
	}

	ok, err := Parse("--repo={{event.repo}} --pr={{event.number?}} {{prompt}}")
	if err != nil {
		t.Fatal(err)
	}
	if bad := check(ok); len(bad) != 0 {
		t.Fatalf("allowed template rejected: %v", bad)
	}

	notOK, err := Parse("{{event.repo}} {{event.nope}}\n{{untrusted event.title}}")
	if err != nil {
		t.Fatal(err)
	}
	bad := check(notOK)
	want := []string{"unknown path: event.nope", "untrusted text is never permitted in argv: event.title"}
	if !reflect.DeepEqual(bad, want) {
		t.Fatalf("check = %v, want %v", bad, want)
	}
	refs := notOK.Refs()
	if refs[1].Line != 1 || refs[1].Col != 16 || refs[2].Line != 2 || refs[2].Col != 1 {
		t.Fatalf("refs not located: %+v", refs)
	}
}

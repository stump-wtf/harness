package core

// Argv template rendering tests: one element renders to exactly one argument,
// whatever the values hold. The property check is run against the real
// renderer AND against a whitespace-splitting one, and must reject the latter:
// a check that passed both would prove nothing about the renderer.
//
// Governing: ADR-0023, SPEC-0017 REQ-11 "Rendering".

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/tmpl"
)

// renderFunc is the shape of RenderCommandArgs, so the property can be run
// against a planted mutant.
type renderFunc func(args []string, ctx tmpl.Context) ([]string, error)

// splittingRender is the bug the property exists to catch: render, then split
// on whitespace the way a shell (or a "helpful" strings.Fields) would.
func splittingRender(args []string, ctx tmpl.Context) ([]string, error) {
	rendered, err := RenderCommandArgs(args, ctx)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, a := range rendered {
		out = append(out, strings.Fields(a)...)
	}
	return out, nil
}

// randomValue draws a value built from the characters most likely to be
// mangled: whitespace of every kind, quotes, shell metacharacters, braces, a
// leading dash, and sometimes nothing at all.
func randomValue(r *rand.Rand) string {
	const alphabet = " \t\n\r'\"$();|&*?[]{}}\\-=abc日本"
	if r.IntN(6) == 0 {
		return ""
	}
	runes := []rune(alphabet)
	n := 1 + r.IntN(24)
	var b strings.Builder
	for range n {
		b.WriteRune(runes[r.IntN(len(runes))])
	}
	return b.String()
}

// argvLengthHolds checks the REQ-11 property over n randomized contexts: the
// rendered argv is exactly as long as the configured one, and every element
// is byte-identical to its template with the values substituted in.
func argvLengthHolds(render renderFunc, n int) error {
	r := rand.New(rand.NewPCG(503, 17))
	args := []string{"--model={{model}}", "{{run.source?}}", "{{harness.workdir}}", "x {{run.trigger?}} y", "{{model?}}{{run.source?}}"}
	for i := range n {
		vals := map[string]string{
			PathModel:          randomValue(r),
			PathHarnessWorkdir: randomValue(r),
		}
		if r.IntN(2) == 0 {
			vals[PathRunSource] = randomValue(r)
		}
		if r.IntN(2) == 0 {
			vals[PathRunTrigger] = randomValue(r)
		}
		got, err := render(args, tmpl.Map{Values: vals})
		if err != nil {
			return fmt.Errorf("case %d: %v", i, err)
		}
		if len(got) != len(args) {
			return fmt.Errorf("case %d: rendered %d arguments from %d elements: %q", i, len(got), len(args), got)
		}
		want := []string{
			"--model=" + vals[PathModel],
			vals[PathRunSource],
			vals[PathHarnessWorkdir],
			"x " + vals[PathRunTrigger] + " y",
			vals[PathModel] + vals[PathRunSource],
		}
		if !slices.Equal(got, want) {
			return fmt.Errorf("case %d: rendered %q, want %q", i, got, want)
		}
	}
	return nil
}

// TestRenderCommandArgsKeepsArgvLength is the acceptance property: argv length
// equals the configured length across randomized values, and the same check
// fails a renderer that splits on whitespace.
func TestRenderCommandArgsKeepsArgvLength(t *testing.T) {
	if err := argvLengthHolds(RenderCommandArgs, 2000); err != nil {
		t.Fatalf("RenderCommandArgs: %v", err)
	}
	if err := argvLengthHolds(splittingRender, 2000); err == nil {
		t.Fatal("the property accepted a whitespace-splitting renderer, so it cannot detect one")
	}
}

// TestRenderCommandArgsSpacesAndEmpty is REQ-11's "Spaces do not split" and
// "Empty optional value": a value with spaces, quotes and newlines stays one
// argument, and an absent optional value is an empty argument, not a missing
// one.
func TestRenderCommandArgsSpacesAndEmpty(t *testing.T) {
	val := "fix the \"thing\"\nand 'this' too"
	got, err := RenderCommandArgs([]string{"--msg={{model}}", "{{run.source?}}"}, tmpl.Map{Values: map[string]string{PathModel: val}})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"--msg=" + val, ""}; !slices.Equal(got, want) {
		t.Errorf("rendered %q, want exactly %q", got, want)
	}
}

// TestRenderCommandArgsUnresolvedNamesThePath: a required value the context
// lacks returns no argv at all, an error naming the element and the path, and
// nothing of any value that was present.
func TestRenderCommandArgsUnresolvedNamesThePath(t *testing.T) {
	const secretish = "value-that-must-not-appear"
	got, err := RenderCommandArgs([]string{"{{model}}", "{{run.id}}"}, tmpl.Map{Values: map[string]string{PathModel: secretish}})
	if got != nil {
		t.Errorf("a failed render returned an argv: %q", got)
	}
	var unres *tmpl.UnresolvedError
	if !errors.As(err, &unres) || unres.Path != PathRunID {
		t.Fatalf("err = %v, want *tmpl.UnresolvedError for run.id", err)
	}
	if !strings.Contains(err.Error(), `"argv[2]"`) {
		t.Errorf("error %q does not name the element", err)
	}
	if strings.Contains(err.Error(), secretish) {
		t.Errorf("error %q carries a rendered value", err)
	}
}

// TestCheckCommandArgvUntrustedNeverInArgv is REQ-10's "A title never reaches
// argv", in every form a path can be written.
func TestCheckCommandArgvUntrustedNeverInArgv(t *testing.T) {
	for _, el := range []string{"{{untrusted event.title}}", "{{event.body}}", "{{event.comment?}}", "--ref={{event.ref}}", "{{untrusted harness.name}}"} {
		err := CheckCommandArgv([]string{"tool", el})
		if err == nil || !strings.Contains(err.Error(), "untrusted text is never permitted in argv") {
			t.Errorf("argv element %q: err = %v, want the untrusted refusal", el, err)
		}
	}
}

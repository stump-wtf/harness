package core

// Command Argv Templates
//
// TL;DR: a `command` harness's argv[1:] elements are SPEC-0017 templates. This
// file is the ONE place that decides which context paths an argv element may
// name, which of them a harness can never have a value for, and how an element
// renders to exactly one argument. The config parser, the project/scratchpad
// wire, the TUI form and spawn all call it, so they cannot disagree.
//
// What argv may reference is a closed allow set over the operator and daemon
// tiers of the context: harness.name, harness.workdir, model, and the run.*
// paths. Event paths are refused for now (a later story wires the event
// context), `prompt`/`prompt_file` wait for a prompt delivery (REQ-12), and
// untrusted free text is refused in argv in every form, permanently: text an
// outside party wrote never becomes an argument (REQ-10).
//
// Governing: ADR-0023 (command one-shots and templating), SPEC-0017 REQ-6
// "Template Grammar", REQ-7 "Template Context", REQ-10 "Untrusted Free Text",
// REQ-11 "Rendering"; design.md § "Validation matrix (config load)".
//
// @joestump-agent 09/24/2026 - Added for #503.

import (
	"errors"
	"fmt"
	"strings"

	"github.com/stump-wtf/harness/internal/tmpl"
)

// Context paths a command argv element may render (SPEC-0017 REQ-7). Named
// so the supervisor's render context and this allow set share one spelling.
const (
	PathHarnessName    = "harness.name"
	PathHarnessWorkdir = "harness.workdir"
	PathModel          = "model"
	PathRunID          = "run.id"
	PathRunTrigger     = "run.trigger"
	PathRunSource      = "run.source"
	PathRunStartedAt   = "run.started_at"
	PathRunDate        = "run.date"
)

// argvPaths is the argv allow set. Every path here is an identifier, a
// timestamp or an operator-written value: none carries a byte an outside party
// chose.
var argvPaths = map[string]bool{
	PathHarnessName: true, PathHarnessWorkdir: true, PathModel: true,
	PathRunID: true, PathRunTrigger: true, PathRunSource: true,
	PathRunStartedAt: true, PathRunDate: true,
}

// untrustedPaths are REQ-10's free-text fields. They are refused in argv in
// every form, including {{untrusted …}}, which renders a fence only inside a
// prompt template.
var untrustedPaths = map[string]bool{
	"event.title": true, "event.body": true, "event.comment": true,
	"event.ref": true, "event.content": true,
}

// eventPath reports whether p is one of REQ-7's event.* paths (other than the
// untrusted ones). They are real context paths, so the error says "not yet"
// rather than "unknown".
func eventPath(p string) bool {
	switch p {
	case "event.file", "event.id", "event.kind", "event.source", "event.received_at",
		"event.name", "event.repo", "event.number", "event.url", "event.sha",
		"event.action", "event.actor":
		return true
	}
	return strings.HasPrefix(p, "event.meta.")
}

// argvElement names argv[i] the way every command-argv error does.
func argvElement(i int) string { return fmt.Sprintf("%q", fmt.Sprintf("argv[%d]", i)) }

// parseArgvTemplates parses argv[1:], returning one Template per element (the
// entry for argv[0] is the zero Template: argv[0] is never a template). A
// grammar error is wrapped with the element's index and still unwraps to
// tmpl.ErrGrammar.
func parseArgvTemplates(argv []string) ([]tmpl.Template, error) {
	out := make([]tmpl.Template, len(argv))
	for i := 1; i < len(argv); i++ {
		t, err := tmpl.Parse(argv[i])
		if err != nil {
			return nil, fmt.Errorf("%s: %w", argvElement(i), err)
		}
		out[i] = t
	}
	return out, nil
}

// checkArgvRef applies the argv location's allow set to one reference.
func checkArgvRef(i int, r tmpl.Ref) error {
	at := fmt.Sprintf("%s line %d, column %d", argvElement(i), r.Line, r.Col)
	switch {
	case untrustedPaths[r.Path] || r.Kind == tmpl.Untrusted:
		return fmt.Errorf("%s: untrusted text is never permitted in argv ({{untrusted %s}} renders only inside a prompt template; give the program {{event.file}} to read instead)", at, r.Path)
	case argvPaths[r.Path]:
		return nil
	case eventPath(r.Path):
		return fmt.Errorf("%s: {{%s}} is not available in argv yet (event context in templates is not implemented; read $HARNESS_EVENT_FILE from the environment)", at, r.Path)
	case r.Path == "prompt" || r.Path == "prompt_file":
		return fmt.Errorf("%s: {{%s}} needs a prompt delivery, which a command harness does not have yet", at, r.Path)
	}
	return fmt.Errorf("%s: unknown template path %q (argv may reference: harness.name, harness.workdir, model, run.id, run.trigger, run.source, run.started_at, run.date)", at, r.Path)
}

// CommandArgvRefs returns every placeholder reference in argv[1:], each paired
// with the index of the element it appears in. The argv must already have
// passed CheckCommandArgv.
func CommandArgvRefs(argv []string) []ArgvRef {
	ts, err := parseArgvTemplates(argv)
	if err != nil {
		return nil
	}
	var refs []ArgvRef
	for i, t := range ts {
		for _, r := range t.Refs() {
			refs = append(refs, ArgvRef{Index: i, Ref: r})
		}
	}
	return refs
}

// ArgvRef is one placeholder reference in a command argv, located by element.
type ArgvRef struct {
	Index int
	tmpl.Ref
}

// CheckCommandTemplateContext applies the rules that depend on the rest of the
// harness, not on argv alone: whether `model` is set, and whether the harness
// is scheduled or triggered at all. Each rule rejects a template whose every
// render would fail (or a key that would do nothing), at load, instead of
// letting every firing turn into a skip nobody asked for.
//
//   - `model` is accepted only when an argv element references {{model}}
//     (REQ-3), and a required {{model}} needs `model` set.
//   - A scheduled harness's clock firings carry no source, so a required
//     {{run.source}} would skip every one of them (REQ-7).
//   - A harness with neither `schedule` nor `triggers` has no run records, so
//     a required {{run.id}}, {{run.trigger}} or {{run.source}} could never
//     render (REQ-7).
//
// The argv must already have passed CheckCommandArgv. The error is meant to
// follow "harness %q: ".
// Governing: ADR-0023, SPEC-0017 REQ-3 "Command Harness Modes And
// Exclusions", REQ-7 "Template Context".
func CheckCommandTemplateContext(argv []string, model string, scheduled, triggered bool) error {
	refs := CommandArgvRefs(argv)
	usesModel := false
	for _, r := range refs {
		at := fmt.Sprintf("%s line %d, column %d", argvElement(r.Index), r.Line, r.Col)
		required := r.Kind == tmpl.Required
		switch r.Path {
		case PathModel:
			usesModel = true
			if required && model == "" {
				return fmt.Errorf("%s: {{model}} is required but \"model\" is not set (set it, or write {{model?}} to pass an empty argument)", at)
			}
		case PathRunSource:
			if required && !triggered {
				return fmt.Errorf("%s: {{run.source}} can never render: the harness has no \"schedule\" or \"triggers\", so it has no run records (write {{run.source?}} for an empty argument)", at)
			}
			if required && scheduled {
				return fmt.Errorf("%s: {{run.source}} is absent on every scheduled firing, which would skip them all (write {{run.source?}} for an empty argument)", at)
			}
		case PathRunID, PathRunTrigger:
			if required && !triggered {
				return fmt.Errorf("%s: {{%s}} can never render: the harness has no \"schedule\" or \"triggers\", so it has no run records (write {{%s?}} for an empty argument)", at, r.Path, r.Path)
			}
		}
	}
	if model != "" && !usesModel {
		return errors.New("\"model\" is unused: no argv element references {{model}}, so nothing would pass it to the program")
	}
	return nil
}

// RenderCommandArgs renders a command harness's arguments (argv[1:], exactly
// as configured) against ctx. Each element renders to exactly one argument:
// nothing is split, trimmed, globbed or re-quoted, and an element that
// renders empty stays as an empty argument, so the result is always as long
// as args (REQ-11).
//
// A required path ctx lacks returns an error that unwraps to
// *tmpl.UnresolvedError, naming the path and never a value; a grammar error
// unwraps to tmpl.ErrGrammar. Either way no argv is returned, so nothing can
// be exec'd with a partial rendering.
// Governing: ADR-0023, SPEC-0017 REQ-11 "Rendering".
func RenderCommandArgs(args []string, ctx tmpl.Context) ([]string, error) {
	out := make([]string, len(args))
	for i, a := range args {
		t, err := tmpl.Parse(a)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", argvElement(i+1), err)
		}
		v, err := t.Render(ctx)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", argvElement(i+1), err)
		}
		out[i] = v
	}
	return out, nil
}

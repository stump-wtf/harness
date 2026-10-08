package core

// Prompt Templates
//
// TL;DR: `prompt_template` and `prompt_template_file` are templated prompt
// sources beside the verbatim `prompt` and `prompt_file`. This file is the ONE
// place that decides which context paths a prompt template may name and which
// of them a harness can ever have a value for; the config parser calls it at
// load and the supervisor calls it again at spawn, because a template file can
// change between the two.
//
// The allow set is the operator, daemon and event tiers of the REQ-7
// context, the same paths an argv element may render. `prompt` and
// `prompt_file` are never in the context (a prompt template IS the prompt;
// REQ-7 forbids naming them). The event paths render from the envelope's
// validated `typed` object, never the raw payload; the free-text fields
// behind {{untrusted …}} stay refused until the untrusted-inline story
// lands, and event.actor until trusted_actors does.
//
// Governing: ADR-0023 (command one-shots and templating), SPEC-0017 REQ-5
// "Templated Prompt Keys", REQ-6 "Template Grammar", REQ-7 "Template
// Context", REQ-10 "Untrusted Free Text", REQ-11 "Rendering".

import (
	"fmt"

	"github.com/stump-wtf/harness/internal/tmpl"
)

// CheckPromptTemplate parses src and checks every reference against the
// prompt-template allow set. A grammar error carries its line and column
// within src; for a template file those are the file's own line and column.
// Governing: SPEC-0017 REQ-5, REQ-6.
func CheckPromptTemplate(src string) error {
	t, err := tmpl.Parse(src)
	if err != nil {
		return err
	}
	for _, r := range t.Refs() {
		if err := checkPromptRef(r); err != nil {
			return err
		}
	}
	return nil
}

// checkPromptRef applies the prompt template's allow set to one reference.
func checkPromptRef(r tmpl.Ref) error {
	at := fmt.Sprintf("line %d, column %d", r.Line, r.Col)
	switch {
	case r.Path == "prompt" || r.Path == "prompt_file":
		return fmt.Errorf("%s: a prompt template cannot reference {{%s}} (it IS the prompt; the verbatim keys are never expanded and never enter the context)", at, r.Path)
	case r.Kind == tmpl.Untrusted || untrustedPaths[r.Path]:
		return fmt.Errorf("%s: {{%s}} is not available yet (free text renders through {{untrusted …}} only once the untrusted-inline story lands; {{event.file}} is the recommended way to hand an agent the event)", at, r.Path)
	case argvPaths[r.Path] || eventPath(r.Path):
		return nil
	case r.Path == "event.actor":
		return fmt.Errorf("%s: {{event.actor}} is not available yet (trusted_actors, SPEC-0017 REQ-9, is not implemented)", at)
	}
	return fmt.Errorf("%s: unknown template path %q (a prompt template may reference: harness.name, harness.workdir, model, run.*, event.*)", at, r.Path)
}

// CheckPromptTemplateContext applies the REQ-7 rules that depend on how the
// harness is fired, at load, so a template whose every render would fail is
// heard about once instead of skipping or failing every firing:
//
//   - a scheduled harness's clock firings carry no source, so a required
//     {{run.source}} would skip every one of them;
//   - a harness with neither `schedule` nor `triggers` has no run records,
//     so a required {{run.id}}, {{run.trigger}} or {{run.source}} could
//     never render;
//   - the same two rules for a required event.* reference: clock firings
//     carry no event, and a harness with no triggers never has one.
//
// Governing: SPEC-0017 REQ-7 "Template Context".
func CheckPromptTemplateContext(src string, scheduled, triggered bool) error {
	t, err := tmpl.Parse(src)
	if err != nil {
		return err
	}
	for _, r := range t.Refs() {
		if r.Kind != tmpl.Required {
			continue
		}
		at := fmt.Sprintf("line %d, column %d", r.Line, r.Col)
		switch r.Path {
		case PathRunSource:
			if scheduled {
				return fmt.Errorf("%s: {{run.source}} is absent on every scheduled firing, which would fail them all (write {{run.source?}} for an empty expansion)", at)
			}
			if !triggered {
				return fmt.Errorf("%s: {{run.source}} can never render: the harness has no \"schedule\" or \"triggers\", so it has no run records (write {{run.source?}} for an empty expansion)", at)
			}
		case PathRunID, PathRunTrigger:
			if !triggered {
				return fmt.Errorf("%s: {{%s}} can never render: the harness has no \"schedule\" or \"triggers\", so it has no run records (write {{%s?}} for an empty expansion)", at, r.Path, r.Path)
			}
		default:
			if eventPath(r.Path) {
				if err := checkRequiredEvent(at, r.Path, scheduled, triggered); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// RenderPrompt parses src, re-checks every reference against the allow set,
// and renders it against ctx. Load already validated the same text, but a
// template file may have changed between load and spawn, so spawn re-applies
// the whole gate: a grammar error or a reference the allow set refuses fails
// the render, and no text is returned — nothing rendered can be half right.
// Governing: SPEC-0017 REQ-5, REQ-6, REQ-11.
func RenderPrompt(src string, ctx tmpl.Context) (string, error) {
	if err := CheckPromptTemplate(src); err != nil {
		return "", err
	}
	t, err := tmpl.Parse(src)
	if err != nil {
		return "", err
	}
	return t.Render(ctx)
}

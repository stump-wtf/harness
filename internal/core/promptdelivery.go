package core

// Command Prompt Delivery
//
// TL;DR: a `command` harness may carry a prompt (`prompt` or `prompt_file`),
// and `prompt_delivery` says how the program receives it: in argv through
// {{prompt}}, on a stdin pipe, or as a 0600 file named by {{prompt_file}} and
// HARNESS_PROMPT_FILE. This file is the ONE validation matrix for that key;
// the config parser, the project/scratchpad wire, the TUI form and the spawn
// backstop all call CheckCommandPrompt, so they cannot disagree about which
// combination drops a prompt on the floor.
//
// The prompt is always the operator's own text, taken verbatim. Nothing an
// event carries is ever part of it: event text reaches a run only as the
// 0600 HARNESS_EVENT_FILE (SPEC-0014), whatever the delivery.
//
// Governing: ADR-0023 (command one-shots and templating), SPEC-0017 REQ-12
// "Prompt Delivery"; design.md § "Validation matrix (config load)".
//
// @joestump-agent 09/24/2026 - Added for #505.

import (
	"errors"
	"fmt"
)

// The prompt_delivery values (SPEC-0017 REQ-12).
const (
	// PromptDeliveryArgv fills the {{prompt}} placeholder: one argv element,
	// never split. The default when an argv element references {{prompt}}.
	PromptDeliveryArgv = "argv"
	// PromptDeliveryStdin writes the prompt to a pipe on the child's fd 0 and
	// closes it; fd 1 and fd 2 stay the PTY, which is the controlling tty.
	PromptDeliveryStdin = "stdin"
	// PromptDeliveryFile writes the prompt, mode 0600, to a file named by
	// HARNESS_PROMPT_FILE and {{prompt_file}}.
	PromptDeliveryFile = "file"
)

// ValidPromptDelivery reports whether d is a prompt_delivery value. Empty is
// valid: it means the default (EffectivePromptDelivery).
func ValidPromptDelivery(d string) bool {
	switch d {
	case "", PromptDeliveryArgv, PromptDeliveryStdin, PromptDeliveryFile:
		return true
	}
	return false
}

// EffectivePromptDelivery resolves a command harness's delivery: the
// configured value when set, else "argv" when an argv element references
// {{prompt}}, else "" (nothing delivers a prompt).
func EffectivePromptDelivery(argv []string, delivery string) string {
	if delivery != "" {
		return delivery
	}
	for _, r := range CommandArgvRefs(argv) {
		if r.Path == PathPrompt {
			return PromptDeliveryArgv
		}
	}
	return ""
}

// CheckCommandPrompt is REQ-12's validation matrix. adapter is the harness
// kind, argv a command harness's argv (already through CheckCommandArgv),
// delivery the configured prompt_delivery ("" when unset) and hasPrompt
// whether `prompt` or `prompt_file` is set. It fails when:
//
//   - prompt_delivery is set on a kind other than `command`;
//   - prompt_delivery is not one of argv, stdin, file;
//   - prompt_delivery is set without a prompt source;
//   - {{prompt}} or {{prompt_file}} is referenced with no prompt source;
//   - {{prompt}} appears outside `argv` delivery;
//   - {{prompt_file}} appears outside `file` delivery;
//   - a prompt source has no delivery path, so the prompt would be dropped.
//
// The error is meant to follow "harness %q: ".
// Governing: ADR-0023, SPEC-0017 REQ-12 "Prompt Delivery".
func CheckCommandPrompt(adapter string, argv []string, delivery string, hasPrompt bool) error {
	if adapter != AdapterCommand {
		if delivery != "" {
			return fmt.Errorf("\"prompt_delivery\" is only accepted on harness = %q (a %q harness synthesizes its own argv and always passes the prompt as an argument)", AdapterCommand, adapter)
		}
		return nil
	}
	if !ValidPromptDelivery(delivery) {
		return fmt.Errorf("\"prompt_delivery\" %q is not one of argv, stdin, file", delivery)
	}
	if delivery != "" && !hasPrompt {
		return fmt.Errorf("\"prompt_delivery\" is set but the harness has no \"prompt\" or \"prompt_file\" to deliver")
	}
	eff := EffectivePromptDelivery(argv, delivery)
	usesPrompt := false
	for _, r := range CommandArgvRefs(argv) {
		at := fmt.Sprintf("%s line %d, column %d", argvElement(r.Index), r.Line, r.Col)
		switch r.Path {
		case PathPrompt:
			usesPrompt = true
			switch {
			case !hasPrompt:
				return fmt.Errorf("%s: {{prompt}} references a prompt, but the harness has no \"prompt\" or \"prompt_file\"", at)
			case eff != PromptDeliveryArgv:
				return fmt.Errorf("%s: {{prompt}} is only available with prompt_delivery = %q (this harness delivers it by %q)", at, PromptDeliveryArgv, eff)
			}
		case PathPromptFile:
			switch {
			case !hasPrompt:
				return fmt.Errorf("%s: {{prompt_file}} references a prompt file, but the harness has no \"prompt\" or \"prompt_file\"", at)
			case eff != PromptDeliveryFile:
				return fmt.Errorf("%s: {{prompt_file}} is only available with prompt_delivery = %q", at, PromptDeliveryFile)
			}
		}
	}
	if !hasPrompt {
		return nil
	}
	if eff == "" || (eff == PromptDeliveryArgv && !usesPrompt) {
		return errDroppedPrompt
	}
	return nil
}

// errDroppedPrompt is REQ-12's "A prompt nothing delivers".
var errDroppedPrompt = errors.New(`the prompt would be dropped: nothing delivers it to the program (reference {{prompt}} in an argv element, or set prompt_delivery = "stdin" or "file")`)

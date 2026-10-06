package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestParsePromptTemplate covers the REQ-5 happy path: a harness templating
// its instruction loads, keeps the template verbatim on the harness (REQ-15),
// and counts as a prompt source for every prompt-dependent key.
// Governing: SPEC-0017 REQ-5, REQ-15.
func TestParsePromptTemplate(t *testing.T) {
	toml := "[harness.sweep]\nharness = \"claude-code\"\nprompt_template = \"sweep run {{run.id}} on {{harness.name}}\"\nmodel = \"claude-opus-5\"\nschedule = \"@daily\"\n"

	cfg, err := Parse([]byte(toml), "test.toml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	h, ok := cfg.Harnesses["sweep"]
	if !ok {
		t.Fatalf("sweep harness missing; order = %v", cfg.HarnessOrder)
	}
	if h.PromptTemplate != "sweep run {{run.id}} on {{harness.name}}" {
		t.Errorf("PromptTemplate = %q, want the template verbatim", h.PromptTemplate)
	}
	if h.IsAgent() != true {
		t.Error("a prompt_template harness is not an agent one-shot")
	}
}

// TestParsePromptTemplateFile: the resolved PATH is stored, not the contents,
// exactly as prompt_file stores a path (REQ-5, ADR-0018).
func TestParsePromptTemplateFile(t *testing.T) {
	path := writePromptFile(t, "Sweep {{harness.name}}.\n")
	toml := "[harness.sweep]\nharness = \"crush\"\nprompt_template_file = \"" + path + "\"\n"

	cfg, err := Parse([]byte(toml), "test.toml")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	h := cfg.Harnesses["sweep"]
	if h.PromptTemplateFile != path {
		t.Errorf("PromptTemplateFile = %q, want the resolved path %q", h.PromptTemplateFile, path)
	}
	if h.PromptTemplate != "" || h.Prompt != "" {
		t.Errorf("contents leaked into config truth: %q / %q", h.PromptTemplate, h.Prompt)
	}
}

// TestParsePromptTemplateGrammarError: a malformed placeholder in the inline
// template is a load error located in the template (REQ-5, REQ-6).
func TestParsePromptTemplateGrammarError(t *testing.T) {
	toml := "[harness.sweep]\nharness = \"crush\"\nprompt_template = \"sweep {{printf \\\"%s\\\" run.id}}\"\n"

	_, err := Parse([]byte(toml), "test.toml")
	if err == nil {
		t.Fatal("Parse accepted a malformed placeholder, want a grammar error")
	}
	if !strings.Contains(err.Error(), "line") || !strings.Contains(err.Error(), "column") {
		t.Errorf("grammar error is not located: %v", err)
	}
}

// TestParsePromptTemplateFileGrammarError: a grammar error in a template FILE
// fails the load naming the file and the line (REQ-5: "parsed at load, with a
// located grammar error (file and line)"). Line 2 is where the bad placeholder
// sits in the file below.
func TestParsePromptTemplateFileGrammarError(t *testing.T) {
	path := writePromptFile(t, "Sweep the fleet,\nthen report {{run.idd}.\n")
	toml := "[harness.sweep]\nharness = \"crush\"\nprompt_template_file = \"" + path + "\"\n"

	_, err := Parse([]byte(toml), "test.toml")
	if err == nil {
		t.Fatal("Parse accepted a malformed template file, want a grammar error")
	}
	if !strings.Contains(err.Error(), path) {
		t.Errorf("error does not name the file: %v", err)
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Errorf("error does not name the template's line: %v", err)
	}
}

// TestParsePromptTemplateMissingFile: an unreadable template file fails the
// load, exactly as prompt_file does (REQ-5 follows prompt_file's rules).
func TestParsePromptTemplateFileMissing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.prompt.md")
	toml := "[harness.sweep]\nharness = \"crush\"\nprompt_template_file = \"" + path + "\"\n"

	if _, err := Parse([]byte(toml), "test.toml"); err == nil {
		t.Fatal("Parse accepted a missing template file")
	}
}

// TestParsePromptTemplateExclusivity: the four prompt sources exclude one
// another, naming the keys set, and each excludes args (REQ-5; REQ-17's
// amendment to SPEC-0006 "Prompt Source").
func TestParsePromptTemplateExclusivity(t *testing.T) {
	cases := []struct{ name, body string }{
		{"prompt+template", "prompt = \"x\"\nprompt_template = \"{{run.id}}\""},
		{"file+template", "prompt_file = \"/tmp/x.md\"\nprompt_template = \"{{run.id}}\""},
		{"template+file", "prompt_template = \"{{run.id}}\"\nprompt_template_file = \"/tmp/x.md\""},
		{"template+args", "prompt_template = \"{{run.id}}\"\nargs = [\"-v\"]"},
		{"template_file+args", "prompt_template_file = \"/tmp/x.md\"\nargs = [\"-v\"]"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			toml := "[harness.sweep]\nharness = \"claude-code\"\n" + tc.body + "\n"
			_, err := Parse([]byte(toml), "test.toml")
			if err == nil {
				t.Fatalf("Parse accepted %s", tc.name)
			}
			if !strings.Contains(err.Error(), "mutually exclusive") {
				t.Errorf("error does not name the exclusivity: %v", err)
			}
		})
	}
}

// TestParsePromptTemplateRefusesPromptReference: {{prompt}} in a prompt
// template is a load error (REQ-7: the prompt template itself SHALL NOT
// reference prompt or prompt_file).
func TestParsePromptTemplateRefusesPromptReference(t *testing.T) {
	for _, ref := range []string{"{{prompt}}", "{{prompt_file}}"} {
		toml := "[harness.sweep]\nharness = \"crush\"\nprompt_template = \"echo " + ref + "\"\n"
		_, err := Parse([]byte(toml), "test.toml")
		if err == nil {
			t.Fatalf("Parse accepted a template referencing %s", ref)
		}
		if !strings.Contains(err.Error(), "cannot reference") {
			t.Errorf("%s: error does not name the forbidden reference: %v", ref, err)
		}
	}
}

// TestParsePromptTemplateContextRules: a template referencing a required path
// no firing of the harness can supply fails at load (REQ-7) — run.source on a
// scheduled harness, and run.id anywhere without schedule or triggers.
func TestParsePromptTemplateContextRules(t *testing.T) {
	cases := []struct{ name, extra, template string }{
		{"scheduled+run.source", "schedule = \"@daily\"", "via {{run.source}}: {{run.id?}}"},
		{"unscheduled+run.id", "", "run {{run.id}}"},
		{"unscheduled+run.trigger", "", "via {{run.trigger}}"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			toml := "[harness.sweep]\nharness = \"crush\"\nprompt_template = \"" + tc.template + "\"\n" + tc.extra + "\n"
			if _, err := Parse([]byte(toml), "test.toml"); err == nil {
				t.Fatalf("Parse accepted %s", tc.name)
			}
		})
	}
	// The optional form is fine everywhere: an absent path renders empty.
	toml := "[harness.sweep]\nharness = \"crush\"\nprompt_template = \"via {{run.source?}}\"\n"
	if _, err := Parse([]byte(toml), "test.toml"); err != nil {
		t.Errorf("Parse rejected an optional run.source reference: %v", err)
	}
}

// TestParsePromptTemplateOnOtherKinds: generic runs sh (no prompt synthesis)
// and command delivers no prompt yet, so neither takes the templated keys,
// exactly as neither takes prompt or prompt_file.
func TestParsePromptTemplateOnOtherKinds(t *testing.T) {
	for _, kind := range []string{"generic", "command"} {
		body := "prompt_template = \"{{run.id}}\""
		if kind == "command" {
			body = "argv = [\"run\"]\n" + body
		}
		toml := "[harness.sweep]\nharness = \"" + kind + "\"\n" + body + "\n"
		if _, err := Parse([]byte(toml), "test.toml"); err == nil {
			t.Errorf("Parse accepted prompt_template on %s", kind)
		}
	}
}

// TestParsePromptTemplateFileBlank: a whitespace-only template file is an
// error, matching the ReadPromptFile emptiness check.
func TestParsePromptTemplateFileBlank(t *testing.T) {
	path := writePromptFile(t, " \t\n")
	toml := "[harness.sweep]\nharness = \"crush\"\nprompt_template_file = \"" + path + "\"\n"
	if _, err := Parse([]byte(toml), "test.toml"); err == nil {
		t.Fatal("Parse accepted a whitespace-only template file")
	}
}

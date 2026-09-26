package persona

import (
	"fmt"
	"strings"
	"text/template"
)

// Render applies the template's prompt.md and system.md texts over answers.
//
// The answers are the INIT ANSWERS (persona name, workdir, owner, and the
// like — plain strings the operator supplied), never event or todo content:
// ADR-0021's data-not-instructions boundary is structural here, because this
// function has no parameter a payload could ride in on.
//
// Governing: ADR-0021; SPEC-0018 REQ-4.
func Render(t *Template, answers map[string]string) (prompt, system string, err error) {
	if prompt, err = renderText(t.Name, "prompt.md", t.Prompt, answers); err != nil {
		return "", "", err
	}
	if system, err = renderText(t.Name, "system.md", t.System, answers); err != nil {
		return "", "", err
	}
	return prompt, system, nil
}

func renderText(name, file, text string, answers map[string]string) (string, error) {
	// missingkey=error: an answer init did not supply (or a misspelled
	// {{.key}}) fails the render instead of shipping "<no value>" in a prompt.
	tmpl, err := template.New(name + "/" + file).Option("missingkey=error").Parse(text)
	if err != nil {
		return "", fmt.Errorf("%w: %s/%s: %w", ErrInvalid, name, file, err)
	}
	var b strings.Builder
	if err := tmpl.Execute(&b, answers); err != nil {
		return "", fmt.Errorf("%w: %s/%s: %w", ErrInvalid, name, file, err)
	}
	return b.String(), nil
}

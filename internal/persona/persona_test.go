package persona

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Governing: SPEC-0018 REQ-4, REQ-29.

// TestBuiltinsValidateAndRender is the table test REQ-4 asks for: every
// built-in template must parse, validate, and render over plain answers —
// and every rendered prompt.md must tell the agent that todo payloads are
// untrusted data (ADR-0021's boundary, carried into every persona).
func TestBuiltinsValidateAndRender(t *testing.T) {
	list, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) < 6 {
		t.Fatalf("got %d templates, want at least the six built-ins", len(list))
	}
	for _, tpl := range list {
		t.Run(tpl.Name, func(t *testing.T) {
			prompt, system, err := Render(tpl, map[string]string{
				"persona": tpl.Name, "workdir": "/tmp/wd", "owner": "joestump",
			})
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			if strings.Contains(prompt, "{{") || strings.Contains(system, "{{") {
				t.Errorf("unrendered template action left in output")
			}
			if !strings.Contains(prompt, "UNTRUSTED DATA") {
				t.Errorf("prompt.md does not mark payloads untrusted")
			}
		})
	}
}

func TestVerifierRunsADifferentFamilyFromImplementer(t *testing.T) {
	impl, err := Load("implementer")
	if err != nil {
		t.Fatal(err)
	}
	ver, err := Load("verifier")
	if err != nil {
		t.Fatal(err)
	}
	if impl.Model == "" || ver.Model == "" {
		t.Fatalf("implementer model %q, verifier model %q: both must pin a family", impl.Model, ver.Model)
	}
	if impl.Model == ver.Model {
		t.Errorf("verifier must default to a different model family from the implementer")
	}
}

func TestUserTemplateShadowsBuiltin(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	name := "reviewer"
	tplDir := filepath.Join(dir, "harness", "templates", name)
	if err := os.MkdirAll(tplDir, 0o700); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(tplDir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("persona.toml", "description = \"my reviewer\"\nmode = \"one-shot\"\nclient = \"crush\"\n")
	write("prompt.md", "Shadow prompt for {{.persona}}.\n")
	write("system.md", "Shadow system.\n")

	tpl, err := Load(name)
	if err != nil {
		t.Fatal(err)
	}
	if !tpl.Shadowed {
		t.Error("a user template over a built-in must be flagged Shadowed")
	}
	if tpl.Description != "my reviewer" || tpl.Client != "crush" {
		t.Errorf("shadow did not win: %+v", tpl)
	}
	// And the listing drops the built-in row rather than showing both.
	list, err := List()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, t2 := range list {
		if t2.Name == name {
			n++
		}
	}
	if n != 1 {
		t.Errorf("listing shows %d rows for %q, want exactly the shadow", n, name)
	}
}

func TestMalformedTemplateFailsNamingFileAndKey(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	tplDir := filepath.Join(dir, "harness", "templates", "broken")
	if err := os.MkdirAll(tplDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "persona.toml"), []byte("description = \"x\"\nmode = \"wizard\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load("broken")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("error must wrap ErrInvalid: %v", err)
	}
	for _, want := range []string{"persona.toml", "\"mode\""} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q must name %s", err, want)
		}
	}
	// Missing prompt.md, same story.
	tplDir = filepath.Join(dir, "harness", "templates", "noprompt")
	if err := os.MkdirAll(tplDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "persona.toml"), []byte("description = \"x\"\nmode = \"resident\"\nclient = \"crush\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = Load("noprompt")
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "prompt.md") {
		t.Fatalf("missing prompt.md must fail with ErrInvalid naming the file: %v", err)
	}
}

func TestLoadUnknown(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	if _, err := Load("no-such-template"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// writeUserTemplate writes a user template under XDG_CONFIG_HOME and returns
// its directory.
func writeUserTemplate(t *testing.T, xdg, name, personaTOML, prompt string) string {
	t.Helper()
	dir := filepath.Join(xdg, "harness", "templates", name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for f, body := range map[string]string{"persona.toml": personaTOML, "prompt.md": prompt, "system.md": "system\n"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestUnknownKeyFailsNamingIt: `allowed_tool` (a typo) must not load as a
// persona with no tool restriction (review of #720).
func TestUnknownKeyFailsNamingIt(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	writeUserTemplate(t, xdg, "typo",
		"description = \"x\"\nmode = \"one-shot\"\nclient = \"claude-code\"\nallowed_tool = [\"Read\"]\n", "p\n")
	_, err := Load("typo")
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "allowed_tool") {
		t.Fatalf("unknown key must fail with ErrInvalid naming it: %v", err)
	}
}

// TestBlankListEntryFails: list fields are validated like the scalars.
func TestBlankListEntryFails(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	writeUserTemplate(t, xdg, "blank",
		"description = \"x\"\nmode = \"one-shot\"\nclient = \"claude-code\"\nallowed_tools = [\" Read \", \"\"]\n", "p\n")
	_, err := Load("blank")
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "allowed_tools") {
		t.Fatalf("blank allowed_tools entry must fail naming the key: %v", err)
	}
}

// TestRenderMissingAnswerFails: a template key init did not answer must fail
// the render, not ship "<no value>" in a prompt.
func TestRenderMissingAnswerFails(t *testing.T) {
	tpl, err := Load("coordinator")
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = Render(tpl, map[string]string{"persona": "lead", "workdir": "/w"}) // no owner
	if !errors.Is(err, ErrInvalid) || !strings.Contains(err.Error(), "owner") {
		t.Fatalf("missing answer must fail naming it: %v", err)
	}
}

// TestListSkipsHiddenDirs: an operator who versions their templates has a
// .git directory beside them; it is not a template.
func TestListSkipsHiddenDirs(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	if err := os.MkdirAll(filepath.Join(xdg, "harness", "templates", ".git"), 0o700); err != nil {
		t.Fatal(err)
	}
	list, err := List()
	if err != nil {
		t.Fatalf("a .git dir must not break the listing: %v", err)
	}
	if len(list) != len(builtinNames) {
		t.Errorf("listed %d templates, want the %d built-ins", len(list), len(builtinNames))
	}
}

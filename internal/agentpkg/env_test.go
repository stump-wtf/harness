// Governing: ADR-0044, ADR-0038; SPEC-0026 REQ-3 (the [[env]] table
// declares names, never values), REQ-12 (install and info list them).
package agentpkg

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

const envManifestHead = "[package]\nname = \"issue-triager\"\n[harness]\nharness = \"claude-code\"\n"

// The issue's own example parses into ordered declarations with every flag
// and description intact, through the real ParseManifest.
func TestParseManifestEnvValid(t *testing.T) {
	src := envManifestHead + `
[[env]]
name        = "GH_TOKEN"
required    = false              # gh may already be logged in
secret      = true
description = "Fine-grained PAT: Pull requests read/write, Contents read"

[[env]]
name        = "TRIAGE_REPOS"
required    = true
description = "Space-separated <host>/<owner>/<repo> list"
`
	m, err := ParseManifest([]byte(src), "package.toml")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []EnvVar{
		{Name: "GH_TOKEN", Secret: true, Description: "Fine-grained PAT: Pull requests read/write, Contents read"},
		{Name: "TRIAGE_REPOS", Required: true, Description: "Space-separated <host>/<owner>/<repo> list"},
	}
	if !reflect.DeepEqual(m.Env, want) {
		t.Errorf("Env = %+v, want %+v", m.Env, want)
	}

	// The inline-table form is the same declaration. It goes first: a bare
	// key after [harness] would belong to [harness].
	inline := "env = [{ name = \"X_TOKEN\", required = true }]\n" + envManifestHead
	m, err = ParseManifest([]byte(inline), "package.toml")
	if err != nil {
		t.Fatalf("parse inline: %v", err)
	}
	if len(m.Env) != 1 || m.Env[0].Name != "X_TOKEN" || !m.Env[0].Required {
		t.Errorf("inline Env = %+v", m.Env)
	}

	// No [[env]] at all is the common case and declares nothing.
	m, err = ParseManifest([]byte(envManifestHead), "package.toml")
	if err != nil || m.Env != nil {
		t.Errorf("no [[env]]: Env = %+v, err = %v", m.Env, err)
	}
}

func TestParseManifestEnvViolations(t *testing.T) {
	tests := []struct {
		name    string
		env     string
		wantErr string
	}{
		{"bad name lower case", "[[env]]\nname = \"gh_token\"\n", "must match"},
		{"bad name leading digit", "[[env]]\nname = \"1TOKEN\"\n", "must match"},
		{"bad name with dash", "[[env]]\nname = \"GH-TOKEN\"\n", "must match"},
		{"missing name", "[[env]]\nrequired = true\n", "name is required"},
		{"name not a string", "[[env]]\nname = 3\n", "name must be a string"},
		{"duplicate name", "[[env]]\nname = \"A\"\n[[env]]\nname = \"A\"\n", "more than once"},
		{"unknown key", "[[env]]\nname = \"A\"\nfile = \"x\"\n", `"file" is not an allowed key`},
		{"value-like key value", "[[env]]\nname = \"A\"\nvalue = \"hunter2\"\n", "never values"},
		{"value-like key default", "[[env]]\nname = \"A\"\ndefault = \"x\"\n", "never values"},
		{"required not a boolean", "[[env]]\nname = \"A\"\nrequired = \"yes\"\n", "required must be a boolean"},
		{"secret not a boolean", "[[env]]\nname = \"A\"\nsecret = 1\n", "secret must be a boolean"},
		{"description not a string", "[[env]]\nname = \"A\"\ndescription = 1\n", "description must be a string"},
		{"secret reference in description", "[[env]]\nname = \"A\"\ndescription = \"copy of ${GH_TOKEN}\"\n", `"${"`},
		{"control character in description", "[[env]]\nname = \"A\"\ndescription = \"line one\\nB=evil\"\n", "single line"},
		{"escape sequence in description", "[[env]]\nname = \"A\"\ndescription = \"\\u001b[2Jcleared\"\n", "control characters"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseManifest([]byte(envManifestHead+tt.env), "package.toml")
			if !errors.Is(err, ErrManifestViolation) {
				t.Fatalf("want ErrManifestViolation, got %v", err)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
			if !strings.Contains(err.Error(), "package.toml") {
				t.Errorf("error %q does not name the manifest path", err.Error())
			}
		})
	}

	// [env] as a plain table (not an array of tables) is refused too.
	_, err := ParseManifest([]byte("[env]\nname = \"A\"\n"+envManifestHead), "package.toml")
	if !errors.Is(err, ErrManifestViolation) || !strings.Contains(err.Error(), "array of tables") {
		t.Errorf("[env] table: got %v", err)
	}
}

// RenderEnv and EnvFileSkeleton print names, flags and descriptions — the
// skeleton comments out optional names so pasting it never blanks a value
// the daemon's environment already supplies.
func TestRenderEnvAndSkeleton(t *testing.T) {
	m := &Manifest{Env: []EnvVar{
		{Name: "GH_TOKEN", Secret: true, Description: "PAT"},
		{Name: "TRIAGE_REPOS", Required: true, Description: "repo list"},
	}}
	if got, want := RenderEnv(m), []string{
		"GH_TOKEN (optional, secret): PAT",
		"TRIAGE_REPOS (required): repo list",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("RenderEnv = %q, want %q", got, want)
	}
	if got, want := EnvFileSkeleton(m), []string{
		"# optional, secret: PAT",
		"# GH_TOKEN=",
		"# required: repo list",
		"TRIAGE_REPOS=",
	}; !reflect.DeepEqual(got, want) {
		t.Errorf("EnvFileSkeleton = %q, want %q", got, want)
	}
	if got := RenderEnv(&Manifest{}); !reflect.DeepEqual(got, []string{"declares no environment variables"}) {
		t.Errorf("RenderEnv(empty) = %q", got)
	}
	if got := EnvFileSkeleton(&Manifest{}); got != nil {
		t.Errorf("EnvFileSkeleton(empty) = %q, want nil", got)
	}
}

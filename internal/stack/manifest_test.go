package stack

import (
	"errors"
	"strings"
	"testing"
)

// TestEmbeddedManifestIsPinnable is REQ-16 as a build failure: any `latest`
// tag, empty tag, missing digest, minimum-above-tag, bad plugin ref, or a
// note naming an unknown component in the EMBEDDED manifest fails `make
// test`, naming the component. Renovate bumps tag and digest together; this
// test is what stops a sloppy bump landing.
func TestEmbeddedManifestIsPinnable(t *testing.T) {
	if _, err := Load(); err != nil {
		t.Fatal(err)
	}
}

const goodDoc = `
version = 1
[postgres]
image = "p"
tag = "17.6"
digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
minimum = "17.0"
[switchboard]
image = "s"
tag = "v0.4.1"
digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
minimum = "v0.4.0"
[cairn]
image = "c"
tag = "v1.0.0"
digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
minimum = "v1.0.0"
[objectstore]
image = "g"
tag = "1.2.0"
digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
minimum = "1.2.0"
[caddy]
image = "cd"
tag = "2.10.0"
digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
minimum = "2.10.0"
[plugin.harness]
repo = "stump-wtf/claude-plugin-harness"
ref = "v0.1.0"
`

func TestParseRejectsForbiddenForms(t *testing.T) {
	digest64 := "sha256:0000000000000000000000000000000000000000000000000000000000000000"
	block := "tag = \"v0.4.1\"\ndigest = \"" + digest64 + "\"\nminimum = \"v0.4.0\""
	tests := []struct {
		name    string
		patch   string
		wantSub string
	}{
		{
			name:    "a latest tag in the manifest",
			patch:   "tag = \"latest\"\ndigest = \"" + digest64 + "\"\nminimum = \"v0.4.0\"",
			wantSub: `tag "latest" is forbidden`,
		},
		{
			name:    "an empty tag",
			patch:   "tag = \"\"\ndigest = \"" + digest64 + "\"\nminimum = \"v0.4.0\"",
			wantSub: "tag\" is empty",
		},
		{
			name:    "a digest missing",
			patch:   "tag = \"v0.4.1\"\ndigest = \"\"\nminimum = \"v0.4.0\"",
			wantSub: "must be sha256",
		},
		{
			name:    "a minimum above the tag",
			patch:   "tag = \"v0.4.1\"\ndigest = \"" + digest64 + "\"\nminimum = \"v0.5.0\"",
			wantSub: "exceeds",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			doc := strings.Replace(goodDoc, block, tc.patch, 1)
			_, err := Parse(doc)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want it to mention %q", err, tc.wantSub)
			}
			if !errors.Is(err, ErrManifestInvalid) {
				t.Errorf("error must wrap ErrManifestInvalid: %v", err)
			}
		})
	}
}

func TestNoteMustReferenceKnownComponent(t *testing.T) {
	doc := strings.Replace(goodDoc, "[plugin.harness]",
		"[[note]]\ncomponent = \"notacomponent\"\nversion = \"v1\"\ntext = \"x\"\n\n[plugin.harness]", 1)
	_, err := Parse(doc)
	if err == nil || !strings.Contains(err.Error(), "unknown component") {
		t.Fatalf("note naming an unknown component must be refused: %v", err)
	}
}

func TestPluginRefNeverNamesABranch(t *testing.T) {
	for _, bad := range []string{"main", "develop", "release/2026-09", "feature/x", "1.x", "v2", "2026-hotfix"} {
		if err := validatePlugin("switchboard", Plugin{Repo: "stump-wtf/x", Ref: bad}); err == nil {
			t.Errorf("ref %q must be rejected as a branch", bad)
		}
	}
	for _, good := range []string{"v0.1.0", "1.2.3", "v2.0.0-rc.1", "8f1c2d3", "8f1c2d3a4b5c6d7e8f9a0b1c2d3e4f5a6b7c8d9e"} {
		if err := validatePlugin("switchboard", Plugin{Repo: "stump-wtf/x", Ref: good}); err != nil {
			t.Errorf("ref %q must be accepted: %v", good, err)
		}
	}
}

func TestValidateOverrideRequiresDigest(t *testing.T) {
	if err := ValidateOverride("ghcr.io/stump-wtf/switchboard:v0.4.1"); !errors.Is(err, ErrOverrideNoDigest) {
		t.Errorf("override without digest must be refused: %v", err)
	}
	good := "ghcr.io/stump-wtf/switchboard:v0.4.1@sha256:" + strings.Repeat("a", 64)
	if err := ValidateOverride(good); err != nil {
		t.Errorf("digest override refused: %v", err)
	}
}

func TestPostgresMajorFollowsTag(t *testing.T) {
	doc := strings.Replace(goodDoc, "minimum = \"17.0\"", "minimum = \"17.0\"\nmajor = 17", 1)
	if _, err := Parse(doc); err != nil {
		t.Fatalf("major 17 with tag 17.6 must parse: %v", err)
	}
	bumped := strings.Replace(doc, "tag = \"17.6\"", "tag = \"18.0\"", 1)
	_, err := Parse(bumped)
	if err == nil || !strings.Contains(err.Error(), "does not match tag") {
		t.Fatalf("a tag bump that leaves major behind must be refused: %v", err)
	}
}

func TestStableTagNamesItself(t *testing.T) {
	doc := strings.Replace(goodDoc, "tag = \"2.10.0\"", "tag = \"stable\"", 1)
	_, err := Parse(doc)
	if err == nil || !strings.Contains(err.Error(), `tag "stable" is forbidden`) {
		t.Fatalf("err = %v, want it to name the \"stable\" tag", err)
	}
}

func TestFirstInvalidComponentIsStable(t *testing.T) {
	// cairn and caddy both invalid: the fixed order reports cairn every time.
	doc := strings.Replace(goodDoc, "tag = \"v1.0.0\"", "tag = \"latest\"", 1)
	doc = strings.Replace(doc, "tag = \"2.10.0\"", "tag = \"latest\"", 1)
	for i := 0; i < 20; i++ {
		_, err := Parse(doc)
		if err == nil || !strings.Contains(err.Error(), "cairn:") {
			t.Fatalf("run %d: err = %v, want cairn reported first", i, err)
		}
	}
}

func TestCompareLoose(t *testing.T) {
	tests := []struct {
		a, b string
		want int
	}{
		{"17.0", "17", 0},
		{"17", "17.0", 0},
		{"v0.4.0", "v0.4.1", -1},
		{"1.10.0", "1.9.0", 1},
		{"v0.0.0-placeholder", "v0.0.0-placeholder", 0},
	}
	for _, tc := range tests {
		if got := compareLoose(tc.a, tc.b); sign(got) != tc.want {
			t.Errorf("compareLoose(%q, %q) = %d, want sign %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func sign(n int) int {
	switch {
	case n > 0:
		return 1
	case n < 0:
		return -1
	}
	return 0
}

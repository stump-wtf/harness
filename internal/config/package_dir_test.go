package config

// A package-sourced command harness may name its bundled script with
// {{package.dir}}; a harness no package defines cannot, because the path would
// never render. The template itself is kept verbatim at load — spawn renders it,
// against the pin.
//
// Governing: ADR-0044; SPEC-0017 REQ-7 "Template Context"; SPEC-0026 REQ-7.

import (
	"path/filepath"
	"strings"
	"testing"
)

const commandManifest = `[package]
name = "pr-reviewer"
[harness]
harness = "command"
argv = ["bash", "{{package.dir}}/watch.sh"]
`

func TestPackageSourcedCommandHarnessLoadsPackageDir(t *testing.T) {
	path := installTestPin(t, commandManifest)
	writeConfig(t, path, "[harness.w]\nsource = \"stump-wtf/pr-reviewer@"+testSHA+"\"\n")
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	h := cfg.Harnesses["w"]
	if len(h.Argv) != 2 || h.Argv[1] != "{{package.dir}}/watch.sh" {
		t.Fatalf("argv = %q, want the template left for spawn to render", h.Argv)
	}
}

func TestPackageDirWithoutAPinFailsTheLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness.toml")
	writeConfig(t, path, "[harness.w]\nharness = \"command\"\nargv = [\"bash\", \"{{package.dir}}/watch.sh\"]\n")
	if _, err := Load(path); err == nil {
		t.Fatal("a hand-written harness with a required {{package.dir}} loaded, want it refused")
	} else if !strings.Contains(err.Error(), "package.dir") || !strings.Contains(err.Error(), "\"source\"") {
		t.Fatalf("load error = %v, want it to name {{package.dir}} and \"source\"", err)
	}
}

func TestOptionalPackageDirLoadsWithoutAPin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "harness.toml")
	writeConfig(t, path, "[harness.w]\nharness = \"command\"\nargv = [\"bash\", \"{{package.dir?}}/watch.sh\"]\n")
	if _, err := Load(path); err != nil {
		t.Fatalf("load: %v", err)
	}
}

package core

// The package.dir argv path: allowed in the closed argv set, and refused at
// load when the harness is defined by nothing but its own table, because every
// render of a required reference would then fail.
//
// Governing: ADR-0044, SPEC-0017 REQ-7 "Template Context", SPEC-0026 REQ-7.

import (
	"strings"
	"testing"
)

func TestCheckCommandArgvAllowsPackageDir(t *testing.T) {
	if err := CheckCommandArgv([]string{"bash", "{{package.dir}}/watch.sh"}); err != nil {
		t.Fatalf("argv with {{package.dir}} refused: %v", err)
	}
	err := CheckCommandArgv([]string{"bash", "{{package.nope}}/watch.sh"})
	if err == nil || !strings.Contains(err.Error(), "package.dir") {
		t.Fatalf("unknown path error = %v, want it to list package.dir as available", err)
	}
}

func TestCheckCommandTemplateContextPackageDirNeedsAPin(t *testing.T) {
	argv := []string{"bash", "{{package.dir}}/watch.sh"}
	err := CheckCommandTemplateContext(argv, "", false, false, false)
	if err == nil || !strings.Contains(err.Error(), "\"source\"") {
		t.Fatalf("required {{package.dir}} on a harness with no package = %v, want it refused naming \"source\"", err)
	}
	if err := CheckCommandTemplateContext(argv, "", false, false, true); err != nil {
		t.Fatalf("required {{package.dir}} on a package-sourced harness = %v, want it accepted", err)
	}
	// The optional form renders empty, so it is legal either way: a table
	// shared between a packaged and a hand-written harness stays loadable.
	for _, packaged := range []bool{false, true} {
		if err := CheckCommandTemplateContext([]string{"bash", "{{package.dir?}}/watch.sh"}, "", false, false, packaged); err != nil {
			t.Fatalf("optional {{package.dir?}} (packaged=%v) = %v, want it accepted", packaged, err)
		}
	}
}

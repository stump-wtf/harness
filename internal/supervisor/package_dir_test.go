package supervisor

// The reported gap, tested where it lives: a command harness an installed
// package defines must hand its bundled script to the child as a path inside
// the pin, so the script runs after an upgrade instead of exiting 127.
//
// Governing: ADR-0044; SPEC-0017 REQ-7 "Template Context", REQ-11 "Rendering";
// SPEC-0026 REQ-7.

import (
	"path/filepath"
	"testing"

	"github.com/stump-wtf/harness/internal/adapter"
	"github.com/stump-wtf/harness/internal/agentpkg"
	"github.com/stump-wtf/harness/internal/core"
)

func TestCommandArgvRendersPackageDirToThePin(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	src := agentpkg.Source{Stable: "stump-wtf", Package: "watcher", SHA: "0123456789abcdef0123456789abcdef01234567"}
	reg := adapter.NewRegistryWithDefaults()
	h := core.Harness{
		Name:          "watcher",
		Adapter:       core.AdapterCommand,
		Argv:          []string{"bash", "{{package.dir}}/watch.sh"},
		PackageSource: src.String(),
	}
	cmd, args := mustExecArgvReg(t, h, "/some/workdir", reg)
	if cmd != "bash" {
		t.Fatalf("argv[0] = %q, want bash", cmd)
	}
	want := filepath.Join(agentpkg.PinDir(src), "watch.sh")
	if len(args) != 1 || args[0] != want {
		t.Fatalf("child received %q, want the bundled script at %q", args, want)
	}
}

// Absent, not empty: a harness no package defines has no package.dir, so a
// required reference fails the render and nothing is exec'd with a literal
// placeholder in its argv.
func TestCommandArgvWithoutAPinDoesNotRenderPackageDir(t *testing.T) {
	reg := adapter.NewRegistryWithDefaults()
	h := core.Harness{
		Name:    "watcher",
		Adapter: core.AdapterCommand,
		Argv:    []string{"bash", "{{package.dir}}/watch.sh"},
	}
	if _, err := execArgvWithRegistry(h, "/some/workdir", RunEnv{}, reg); err == nil {
		t.Fatal("spawn rendered {{package.dir}} for a harness with no package, want the start refused")
	}
}

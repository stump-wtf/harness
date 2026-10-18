package agentpkg

// InstalledPackageDir is the run context's one answer to "where are this
// package's files": the pin directory named by the harness's own source, never
// a path the manifest chose.
//
// Governing: ADR-0044; SPEC-0026 REQ-7; SPEC-0017 REQ-7 "Template Context".

import (
	"path/filepath"
	"testing"
)

func TestInstalledPackageDirNamesThePin(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", filepath.Join(t.TempDir(), "state"))
	src := Source{Stable: "stump-wtf", Package: "watcher", SHA: "0123456789abcdef0123456789abcdef01234567"}
	dir, ok := InstalledPackageDir(src.String())
	if !ok {
		t.Fatal("InstalledPackageDir reported no directory for a valid source")
	}
	if want := PinDir(src); dir != want {
		t.Errorf("dir = %q, want the pin directory %q", dir, want)
	}
}

func TestInstalledPackageDirAbsentWithoutAPin(t *testing.T) {
	for _, src := range []string{"", "not-a-source", "stump-wtf/watcher@deadbeef"} {
		if dir, ok := InstalledPackageDir(src); ok {
			t.Errorf("InstalledPackageDir(%q) = %q, want absent", src, dir)
		}
	}
}

package buildinfo

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Governing: issue #1024. The Makefile's go gates must not see the
// Makefile's own build flags in GOFLAGS: a file-scope `GOFLAGS :=`
// assignment is exported to every recipe whenever the caller exports
// GOFLAGS, and go aborts parsing the leaked -ldflags before a single test
// runs. The `goflags-check` recipe pins that inside the gate; this test
// pins the guard itself so its removal or breakage is caught by CI too.
func TestGoflagsCheckGuard(t *testing.T) {
	if _, err := exec.LookPath("make"); err != nil {
		t.Skip("make not available")
	}
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(wd, "..", "..")

	run := func(env ...string) (string, error) {
		cmd := exec.Command("make", "-C", root, "goflags-check")
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	t.Run("caller's own GOFLAGS passes", func(t *testing.T) {
		out, err := run("GOFLAGS=-p=2")
		if err != nil {
			t.Fatalf("goflags-check rejected a caller's own GOFLAGS: %v\n%s", err, out)
		}
	})

	t.Run("leaked build flags fail", func(t *testing.T) {
		out, err := run(`GOFLAGS=-trimpath -ldflags "-X example.com/x.Version=v1"`)
		if err == nil {
			t.Fatalf("goflags-check passed with -ldflags in GOFLAGS:\n%s", out)
		}
		if !strings.Contains(out, "must not reach the go gate") {
			t.Fatalf("expected the guard's message, got:\n%s", out)
		}
	})
}

// The lint rules the CLI goldens do not each carry a fixture for: an empty
// path, a missing skill root, a symlink, a bad directory name, a stray file
// in packages/, an empty packages/, and the promotion table.
//
// Governing: ADR-0044, SPEC-0026 REQ-3, REQ-5, REQ-12.
//
// @joestump-agent 10/09/2026 - Added for harness#933.
package stablelint

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/agentpkg"
)

func writeStable(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func errorIDs(r Report) string {
	var ids []string
	for _, is := range r.Errors {
		ids = append(ids, is.ID)
	}
	return strings.Join(ids, ",")
}

const okManifest = "[package]\nname = \"p\"\nlicense = \"MIT\"\n[harness]\nharness = \"claude-code\"\nprompt_file = \"prompt.md\"\n"

func TestLintEmptyPromptFile(t *testing.T) {
	root := writeStable(t, map[string]string{
		"packages/p/package.toml": okManifest,
		"packages/p/README.md":    "# p\n",
		"packages/p/prompt.md":    "",
	})
	reports, err := Check(root)
	if err != nil {
		t.Fatal(err)
	}
	// The load test is skipped: config load would only restate the path.
	if got := errorIDs(reports[0]); got != IDEmptyFile {
		t.Errorf("errors %q, want %q", got, IDEmptyFile)
	}
}

func TestLintMissingSkillPath(t *testing.T) {
	root := writeStable(t, map[string]string{
		"packages/p/package.toml": okManifest + "skill_paths = [\"skills\"]\n",
		"packages/p/README.md":    "# p\n",
		"packages/p/prompt.md":    "Review it.\n",
	})
	reports, err := Lint(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := errorIDs(reports[0]); got != IDMissingFile {
		t.Errorf("errors %q, want %q", got, IDMissingFile)
	}
}

func TestLintSymlinkAndBadName(t *testing.T) {
	root := writeStable(t, map[string]string{
		"packages/p/package.toml":     okManifest,
		"packages/p/README.md":        "# p\n",
		"packages/p/prompt.md":        "Review it.\n",
		"packages/Bad_Name/README.md": "# bad\n",
		"packages/NOTES.txt":          "not a package\n",
	})
	if err := os.Symlink("/etc/hosts", filepath.Join(root, "packages", "p", "hosts")); err != nil {
		t.Fatal(err)
	}
	reports, err := Lint(root)
	if err != nil {
		t.Fatal(err)
	}
	// NOTES.txt is not a package; discovery skips plain files.
	if len(reports) != 2 {
		t.Fatalf("got %d reports, want 2: %+v", len(reports), reports)
	}
	if got := errorIDs(reports[0]); got != IDBadName+","+IDManifest {
		t.Errorf("Bad_Name errors %q", got)
	}
	if got := errorIDs(reports[1]); got != IDSymlink {
		t.Errorf("p errors %q, want %q", got, IDSymlink)
	}
}

func TestLintEmptyPackagesDir(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "packages"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Lint(root); !errors.Is(err, ErrNotStable) {
		t.Fatalf("err = %v, want ErrNotStable", err)
	}
}

// TestPromotedLowFindingsAreErrors pins the promotion table: a low finding
// is a warning unless its id is promoted, and both convention findings are.
func TestPromotedLowFindingsAreErrors(t *testing.T) {
	for _, id := range []string{IDNoReadme, IDNoLicense} {
		r := Report{Errors: []Issue{}, Warnings: []Issue{}}
		r.add(agentpkg.SeverityLow, Issue{ID: id})
		if len(r.Errors) != 1 || len(r.Warnings) != 0 {
			t.Errorf("%s: low finding not promoted to an error: %+v", id, r)
		}
	}
	r := Report{Errors: []Issue{}, Warnings: []Issue{}}
	r.add(agentpkg.SeverityLow, Issue{ID: "scan.imperative.prose"})
	if len(r.Errors) != 0 || len(r.Warnings) != 1 {
		t.Errorf("an unpromoted low finding must stay a warning: %+v", r)
	}
}

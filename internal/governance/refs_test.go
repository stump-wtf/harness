package governance

import (
	"bufio"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

var (
	adrFile   = regexp.MustCompile(`^adr-(\d{4})-.*\.md$`)
	specTitle = regexp.MustCompile(`^# SPEC-(\d{4}):`)
	citation  = regexp.MustCompile(`\b(ADR|SPEC)-(\d{4})\b`)
)

// repoRoot walks up from the test's directory to the module root.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}

// known collects the ADR numbers from docs/adrs file names and the spec
// numbers from each spec's title line — a spec is cited by the number in its
// title, not by its directory name.
func known(root string) (map[string]bool, error) {
	out := map[string]bool{}
	adrs, err := os.ReadDir(filepath.Join(root, "docs", "adrs"))
	if err != nil {
		return nil, err
	}
	for _, e := range adrs {
		if m := adrFile.FindStringSubmatch(e.Name()); m != nil {
			out["ADR-"+m[1]] = true
		}
	}
	specs, err := filepath.Glob(filepath.Join(root, "docs", "openspec", "specs", "*", "spec.md"))
	if err != nil {
		return nil, err
	}
	for _, p := range specs {
		f, err := os.Open(p)
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if m := specTitle.FindStringSubmatch(sc.Text()); m != nil {
				out["SPEC-"+m[1]] = true
				break
			}
		}
		_ = f.Close()
	}
	return out, nil
}

// dangling returns file:line: REF for every ADR-NNNN / SPEC-NNNN cited in a
// Go file under root that docs does not contain. This package's own files are
// skipped: they name numbers as examples, not citations.
func dangling(root string, docs map[string]bool) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Worktrees and dependency trees are other checkouts, not this one.
			switch d.Name() {
			case ".git", ".claude", "node_modules", "vendor":
				return filepath.SkipDir
			}
			if rel, _ := filepath.Rel(root, path); rel == filepath.Join("internal", "governance") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		for i, line := range strings.Split(string(b), "\n") {
			for _, m := range citation.FindAllStringSubmatch(line, -1) {
				if ref := m[1] + "-" + m[2]; !docs[ref] {
					out = append(out, rel+":"+strconv.Itoa(i+1)+": "+ref)
				}
			}
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

// TestGoCitationsResolve fails for any ADR or spec a Go file cites that has
// no document behind it.
func TestGoCitationsResolve(t *testing.T) {
	root := repoRoot(t)
	docs, err := known(root)
	if err != nil {
		t.Fatal(err)
	}
	// An empty set would make every citation dangle, but a partly-blind scan
	// (the ADR half found, the spec half not) would pass silently.
	var adrs, specs int
	for ref := range docs {
		if strings.HasPrefix(ref, "ADR-") {
			adrs++
		} else {
			specs++
		}
	}
	if adrs == 0 || specs == 0 {
		t.Fatalf("found %d ADRs and %d specs; the docs layout moved and this check is blind", adrs, specs)
	}
	bad, err := dangling(root, docs)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range bad {
		t.Errorf("cites a document that does not exist: %s", d)
	}
}

// TestDanglingReportsUnknownCitations runs the scan over a fixture tree, so
// the check above is known to fire rather than trusted for its silence.
func TestDanglingReportsUnknownCitations(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("docs/adrs/adr-0001-first.md", "# ADR-0001\n")
	write("docs/openspec/specs/thing/spec.md", "---\n---\n\n# SPEC-0002: Thing\n")
	write("pkg/a.go", "package pkg\n\n// Governing: ADR-0001, SPEC-0002 REQ-1\n// Governing: ADR-0003, SPEC-0004\n")
	write(".claude/worktrees/x/b.go", "package x // SPEC-0099\n")

	docs, err := known(root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := dangling(root, docs)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join("pkg", "a.go") + ":4: ADR-0003",
		filepath.Join("pkg", "a.go") + ":4: SPEC-0004",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("dangling =\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}

package fake

// TreePaths Tests
//
// Governing tests: SPEC-0025 REQ-18 — the fake answers a watched-directory
// read from the files set at a sha, or at the sha a branch name points at.

import (
	"errors"
	"slices"
	"testing"

	"github.com/stump-wtf/harness/internal/forge"
)

func TestTreePaths(t *testing.T) {
	f := New()
	f.SetBranch(repo, "main", "base")
	f.SetFile(repo, "base", "docs/adrs/adr-0050-a.md", []byte("a"))
	f.SetFile(repo, "base", "docs/adrs/adr-0051-b.md", []byte("b"))
	f.SetFile(repo, "base", "docs/guides/index.md", []byte("i"))
	f.SetFile(repo, "c1", "migrations/0007_x.sql", []byte("m"))

	cases := []struct {
		ref      string
		prefixes []string
		want     []string
	}{
		{"base", []string{"docs/adrs/"}, []string{"docs/adrs/adr-0050-a.md", "docs/adrs/adr-0051-b.md"}},
		{"main", []string{"docs/adrs"}, []string{"docs/adrs/adr-0050-a.md", "docs/adrs/adr-0051-b.md"}},
		{"main", []string{"docs/adrs", "migrations"}, []string{"docs/adrs/adr-0050-a.md", "docs/adrs/adr-0051-b.md"}},
		{"c1", nil, []string{"migrations/0007_x.sql"}},
		{"main", []string{"docs"}, []string{"docs/adrs/adr-0050-a.md", "docs/adrs/adr-0051-b.md", "docs/guides/index.md"}},
		{"main", []string{"docs/adrsx"}, nil},
	}
	for _, c := range cases {
		got, err := f.TreePaths(ctx, repo, c.ref, c.prefixes)
		if err != nil {
			t.Fatalf("TreePaths(%s, %v): %v", c.ref, c.prefixes, err)
		}
		if !slices.Equal(got, c.want) {
			t.Errorf("TreePaths(%s, %v) = %v, want %v", c.ref, c.prefixes, got, c.want)
		}
	}
}

func TestTreePathsUnknownRef(t *testing.T) {
	f := New()
	if _, err := f.TreePaths(ctx, repo, "nope", nil); !errors.Is(err, forge.ErrNotFound) {
		t.Fatalf("err = %v, want forge.ErrNotFound", err)
	}
}

// Retrieval-index and retrieval-count tests.
//
// Governing: SPEC-0007 REQ "Embedded Retrieval Index", REQ
// "Retrieval-Count Retirement", REQ "Database Operation Standards", REQ
// "Error Handling Standards".
//
// @joestump-agent 09/27/2026 - Added for harness#78.
package skillserve

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testEntry builds an index entry for a skill in one repo.
func testEntry(repo, slug, description, symptoms string, applies ...string) Entry {
	return Entry{
		Repo:        repo,
		Slug:        slug,
		RelPath:     "skills/" + slug + "/SKILL.md",
		Name:        slug,
		Description: description,
		Symptoms:    []string{symptoms},
		Tags:        []string{"ci"},
		AppliesTo:   applies,
	}
}

func openTestIndex(t *testing.T) *Index {
	t.Helper()
	idx, err := OpenIndex(filepath.Join(t.TempDir(), "index.db"))
	if err != nil {
		t.Fatalf("open index: %v", err)
	}
	t.Cleanup(func() { _ = idx.Close() })
	return idx
}

func TestSearchLiteralSymptomAndStemming(t *testing.T) {
	idx := openTestIndex(t)
	if err := idx.ReindexRepo("go-stack", []Entry{
		testEntry("go-stack", "pin-go", "Pin the Go toolchain in CI.",
			"go: cannot find main module", ".gitea/workflows/*"),
		testEntry("other", "unrelated", "Nothing about modules here.", "totally different failure"),
	}); err != nil {
		t.Fatalf("reindex: %v", err)
	}

	// Literal symptom text matches (SPEC-0007 REQ "Embedded Retrieval
	// Index": a query containing a literal string from symptoms finds it).
	hits, err := idx.Search("cannot find main module", nil, 5, nil)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].Repo != "go-stack" || hits[0].Slug != "pin-go" {
		t.Fatalf("want go-stack/pin-go, got %+v", hits)
	}

	// Porter stemming: "modules" matches the symptom's "module".
	hits, err = idx.Search("modules", nil, 5, nil)
	if err != nil {
		t.Fatalf("stemmed search: %v", err)
	}
	found := false
	for _, h := range hits {
		if h.Slug == "pin-go" {
			found = true
		}
	}
	if !found {
		t.Fatalf("want stemmed match for pin-go, got %+v", hits)
	}
}

func TestIndexRecordsRepoAndNoBodies(t *testing.T) {
	idx := openTestIndex(t)
	if err := idx.ReindexRepo("go-stack", []Entry{testEntry("go-stack", "pin-go", "Pin the toolchain.", "failure")}); err != nil {
		t.Fatal(err)
	}
	hits, err := idx.Search("toolchain", nil, 5, nil)
	if err != nil || len(hits) != 1 {
		t.Fatalf("search: %v %v", hits, err)
	}
	if hits[0].Repo != "go-stack" {
		t.Fatalf("index must record the source repo, got %+v", hits[0])
	}
	if strings.Contains(hits[0].Description, "Steps") || len(hits) > 5 {
		t.Fatalf("search carries identifiers and descriptions only: %+v", hits[0])
	}
}

func TestReindexReplacesRepoRows(t *testing.T) {
	idx := openTestIndex(t)
	if err := idx.ReindexRepo("go-stack", []Entry{testEntry("go-stack", "old", "Old skill.", "alpha")}); err != nil {
		t.Fatal(err)
	}
	if err := idx.ReindexRepo("go-stack", []Entry{testEntry("go-stack", "new", "New skill.", "beta")}); err != nil {
		t.Fatal(err)
	}
	hits, err := idx.Search("beta", nil, 5, nil)
	if err != nil || len(hits) != 1 || hits[0].Slug != "new" {
		t.Fatalf("want new only, got %v %v", hits, err)
	}
	hits, err = idx.Search("alpha", nil, 5, nil)
	if err != nil || len(hits) != 0 {
		t.Fatalf("old skill must leave the index, got %v %v", hits, err)
	}
}

func TestReindexAtomicOnMidTransactionFailure(t *testing.T) {
	idx := openTestIndex(t)
	if err := idx.ReindexRepo("go-stack", []Entry{testEntry("go-stack", "good", "Existing skill.", "existing failure")}); err != nil {
		t.Fatal(err)
	}
	// A NOT NULL violation on the second row fails the transaction partway:
	// the previous index contents must remain queryable and no partial state
	// visible (SPEC-0007 REQ "Database Operation Standards").
	broken := []Entry{
		testEntry("go-stack", "fine", "Fine skill.", "fine failure"),
		testEntry("go-stack", "", "Broken: empty slug violates NOT NULL.", "broken failure"),
	}
	if err := idx.ReindexRepo("go-stack", broken); err == nil {
		t.Fatal("want an error from the mid-transaction failure")
	}
	hits, err := idx.Search("existing", nil, 5, nil)
	if err != nil || len(hits) != 1 || hits[0].Slug != "good" {
		t.Fatalf("previous index must stay queryable, got %v %v", hits, err)
	}
	hits, err = idx.Search("fine failure", nil, 5, nil)
	if err != nil || len(hits) != 0 {
		t.Fatalf("no partial state may be visible, got %v %v", hits, err)
	}
}

func TestIndexIsRebuildable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "index.db")
	entries := []Entry{
		testEntry("go-stack", "pin-go", "Pin the Go toolchain in CI.", "cannot find main module", ".gitea/workflows/*"),
		testEntry("go-stack", "pin-ci", "Pin the CI runner image.", "runner version mismatch", ".gitea/workflows/*"),
	}

	rank := func() []SearchHit {
		idx, err := OpenIndex(path)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer idx.Close()
		if err := idx.ReindexRepo("go-stack", entries); err != nil {
			t.Fatalf("reindex: %v", err)
		}
		hits, err := idx.Search("toolchain pin", nil, 5, nil)
		if err != nil {
			t.Fatalf("search: %v", err)
		}
		return hits
	}

	first := rank()
	// Delete the rebuildable cache and rebuild from the same entries.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	second := rank()
	if len(first) != len(second) || len(first) == 0 {
		t.Fatalf("rebuild must restore equivalent results, got %v then %v", first, second)
	}
	for i := range first {
		if first[i].Slug != second[i].Slug || first[i].Score != second[i].Score {
			t.Fatalf("ranking drift after rebuild: %v vs %v", first, second)
		}
	}
}

func TestSearchAppliesToBoosts(t *testing.T) {
	idx := openTestIndex(t)
	entries := []Entry{
		testEntry("go-stack", "generic", "Pin things in CI generally.", "pin failure"),
		testEntry("go-stack", "workflow", "Pin workflow files in CI.", "pin failure", ".gitea/workflows/*"),
	}
	if err := idx.ReindexRepo("go-stack", entries); err != nil {
		t.Fatal(err)
	}
	hits, err := idx.Search("pin failure", []string{".gitea/workflows/ci.yml"}, 5, nil)
	if err != nil || len(hits) != 2 {
		t.Fatalf("search: %v %v", hits, err)
	}
	if hits[0].Slug != "workflow" {
		t.Fatalf("applies_to match must rank first, got %+v", hits)
	}
}

func TestSearchNeutralizesQuerySyntax(t *testing.T) {
	idx := openTestIndex(t)
	if err := idx.ReindexRepo("go-stack", []Entry{testEntry("go-stack", "pin-go", "Pin the toolchain.", "cannot find main module")}); err != nil {
		t.Fatal(err)
	}
	// FTS5 query operators in agent-supplied terms must not change the
	// query's structure (SPEC-0007 REQ "Database Operation Standards"): the
	// operators become plain quoted tokens, so the query degrades to a
	// narrow AND instead of a set difference or column filter.
	hits, err := idx.Search(`module" NOT (nefarious OR phrase) *`, nil, 5, nil)
	if err != nil {
		t.Fatalf("query syntax must be neutralized, got %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("NOT/OR must not act as operators, got %+v", hits)
	}
	if empty, err := idx.Search("", nil, 5, nil); err != nil || len(empty) != 0 {
		t.Fatalf("empty query returns nothing without error: %v %v", empty, err)
	}
}

func TestCountsStoreEligibility(t *testing.T) {
	path := filepath.Join(t.TempDir(), "counts.db")
	cs, err := OpenCounts(path)
	if err != nil {
		t.Fatalf("open counts: %v", err)
	}
	defer cs.Close()

	// Lost counts fail safe: a store that had to be created makes nothing
	// eligible (SPEC-0007 REQ "Retrieval-Count Retirement").
	if !cs.WasMissing() {
		t.Fatal("fresh store must report wasMissing")
	}
	merged := time.Now().Add(-90 * 24 * time.Hour)
	if ok, err := cs.EligibleForRetirement("go-stack", "pin-go", merged, time.Now(), 30, 60); err != nil || ok {
		t.Fatalf("fresh store must never be eligible, got %v %v", ok, err)
	}

	// A pre-existing store stops failing safe.
	if err := cs.Close(); err != nil {
		t.Fatal(err)
	}
	cs, err = OpenCounts(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer cs.Close()
	if cs.WasMissing() {
		t.Fatal("existing store must not report wasMissing")
	}

	now := time.Now()
	// In grace: not eligible even with zero retrievals.
	recent := now.Add(-10 * 24 * time.Hour)
	if ok, _ := cs.EligibleForRetirement("go-stack", "new", recent, now, 30, 60); ok {
		t.Fatal("in-grace skill must not be eligible")
	}
	// Past grace, zero retrievals in the window: eligible.
	if ok, err := cs.EligibleForRetirement("go-stack", "old", merged, now, 30, 60); err != nil || !ok {
		t.Fatalf("want eligible, got %v %v", ok, err)
	}
	// Past grace, retrieved inside the window: not eligible.
	if err := cs.Record("go-stack", "used", "reduit/agent", "42", now.Add(-24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if ok, _ := cs.EligibleForRetirement("go-stack", "used", merged, now, 30, 60); ok {
		t.Fatal("retrieved skill must not be eligible")
	}
	if n, err := cs.CountSince("go-stack", "used", now.Add(-2*24*time.Hour)); err != nil || n != 1 {
		t.Fatalf("count since: %d %v", n, err)
	}
}

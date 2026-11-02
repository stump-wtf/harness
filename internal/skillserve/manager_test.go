// Manager tests: the daemon-side reindex triggers and gate behavior.
//
// Governing: SPEC-0007 REQ "Default-Branch Gate", REQ "Retrieval-Count
// Retirement", REQ "Error Handling Standards".
//
// @joestump-agent 09/27/2026 - Added for harness#78.
package skillserve

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

func reposOf(name, path string) []core.SkillRepo {
	return []core.SkillRepo{{Name: name, Remote: "unused", Path: path, ServeTo: []string{"*"}, Public: true}}
}

func newTestManager(t *testing.T, clone string) *Manager {
	t.Helper()
	dir := t.TempDir()
	m := NewManager(reposOf("go-stack", "skills"), filepath.Join(dir, "index.db"), filepath.Join(dir, "counts.db"), 30, 60, 0)
	m.dirFor = func(string) string { return clone }
	t.Cleanup(m.Stop)
	return m
}

func TestManagerIndexesOnStart(t *testing.T) {
	_, clone := newServingClone(t)
	m := newTestManager(t, clone)

	if err := m.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	hits, err := m.Search(Caller{}, "cannot find main module", nil, 5)
	if err != nil || len(hits) != 1 || hits[0].Repo != "go-stack" {
		t.Fatalf("start must index the serving clone, got %v %v", hits, err)
	}
	sts := m.Statuses()
	if len(sts) != 1 || sts[0].State != string(CloneOK) || sts[0].Skills != 1 {
		t.Fatalf("want ok status with 1 skill, got %+v", sts)
	}
}

func TestManagerKeepsPreviousIndexOnGateFailure(t *testing.T) {
	_, clone := newServingClone(t)
	m := newTestManager(t, clone)
	if err := m.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// Detached HEAD: reindex refused, the previous index keeps serving.
	gitRun(t, clone, "checkout", "-q", "--detach", "HEAD")
	m.ReindexRepos([]string{"go-stack"})
	hits, err := m.Search(Caller{}, "cannot find main module", nil, 5)
	if err != nil || len(hits) != 1 {
		t.Fatalf("previous index must keep serving, got %v %v", hits, err)
	}
	sts := m.Statuses()
	if sts[0].State != string(CloneDetached) || sts[0].Detail == "" {
		t.Fatalf("doctor must see the detached clone, got %+v", sts)
	}

	// Dirty tree, same story.
	gitRun(t, clone, "checkout", "-q", "main")
	if err := os.WriteFile(filepath.Join(clone, "skills", "pin-go", "SKILL.md"), []byte("half-written"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.ReindexRepos([]string{"go-stack"})
	hits, err = m.Search(Caller{}, "cannot find main module", nil, 5)
	if err != nil || len(hits) != 1 {
		t.Fatalf("dirty clone must keep the previous index, got %v %v", hits, err)
	}
	if sts := m.Statuses(); sts[0].State != string(CloneDirty) {
		t.Fatalf("doctor must see the dirty clone, got %+v", sts)
	}
}

func TestManagerReindexesOnSyncReport(t *testing.T) {
	origin, clone := newServingClone(t)
	m := newTestManager(t, clone)
	if err := m.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	// A merged skill lands on the origin default branch and sync promotes it.
	commitSkillRepo(t, origin, map[string]string{
		"skills/pin-go/SKILL.md": testSkill("pin-go", "active"),
		"skills/pin-ci/SKILL.md": testSkill("pin-ci", "active"),
	})
	if _, err := SyncRepo("go-stack", origin, clone); err != nil {
		t.Fatalf("sync: %v", err)
	}
	m.ReindexRepos([]string{"go-stack"})

	hits, err := m.Search(Caller{}, "cannot find main module", nil, 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("fast-forward report must promote the merged skill, got %v", hits)
	}
	if sts := m.Statuses(); sts[0].Skills != 2 {
		t.Fatalf("want 2 indexed skills, got %+v", sts)
	}
}

func TestManagerReconcilePicksUpNewClone(t *testing.T) {
	_, clone := newServingClone(t)
	m := newTestManager(t, clone)

	// The clone does not exist when the manager starts (daemon booted before
	// the first sync). The reconcile pass must index it once it appears.
	m.dirFor = func(string) string { return filepath.Join(filepath.Dir(clone), "absent") }
	if err := m.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if sts := m.Statuses(); sts[0].State != string(CloneMissing) {
		t.Fatalf("want missing before the clone exists, got %+v", sts)
	}
	m.dirFor = func(string) string { return clone }
	m.reconcile()

	hits, err := m.Search(Caller{}, "cannot find main module", nil, 5)
	if err != nil || len(hits) != 1 {
		t.Fatalf("reconcile must index the appeared clone, got %v %v", hits, err)
	}
}

func TestManagerRetrievalCountsAndEligibility(t *testing.T) {
	_, clone := newServingClone(t)
	m := newTestManager(t, clone)
	// Pre-create the counts store so this test exercises the eligibility
	// logic rather than the lost-counts fail-safe (covered separately).
	countsPath := filepath.Join(t.TempDir(), "counts.db")
	if cs, err := OpenCounts(countsPath); err != nil {
		t.Fatalf("pre-create counts: %v", err)
	} else {
		_ = cs.Close()
	}
	m.countsPath = countsPath
	if err := m.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}

	if err := m.RecordRetrieval("go-stack", "pin-go", "reduit/agent", "42", time.Now()); err != nil {
		t.Fatalf("record: %v", err)
	}
	// In grace: never eligible (SPEC-0007 REQ "Retrieval-Count Retirement").
	merged := time.Now().Add(-10 * 24 * time.Hour)
	if ok, err := m.EligibleForRetirement("go-stack", "pin-go", merged, time.Now()); err != nil || ok {
		t.Fatalf("in-grace skill must not be eligible, got %v %v", ok, err)
	}
	// Past grace but retrieved inside the window: not eligible.
	merged = time.Now().Add(-90 * 24 * time.Hour)
	if ok, err := m.EligibleForRetirement("go-stack", "pin-go", merged, time.Now()); err != nil || ok {
		t.Fatalf("recently retrieved skill must not be eligible, got %v %v", ok, err)
	}
	// Past grace, no retrievals: eligible.
	if ok, err := m.EligibleForRetirement("go-stack", "pin-ci", merged, time.Now()); err != nil || !ok {
		t.Fatalf("disused skill past grace must be eligible, got %v %v", ok, err)
	}
}

func TestManagerWithoutReposCreatesNoIndex(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(nil, filepath.Join(dir, "index.db"), filepath.Join(dir, "counts.db"), 30, 60, 0)
	defer m.Stop()
	if err := m.Start(); err != nil {
		t.Fatalf("start with no repos: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "index.db")); !os.IsNotExist(err) {
		t.Fatal("no skill repo declared: no index file may be created (SPEC-0007 REQ \"Skill Repos\")")
	}
	if _, err := m.Search(Caller{}, "anything", nil, 3); err == nil {
		t.Fatal("search without repos must report the index unavailable")
	}
}

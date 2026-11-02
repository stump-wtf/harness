// Scoping, retrieval and rendering tests for the search_skills and get_skill
// surface: which repos a caller may search, what one skill render returns, and
// what the default-branch gate refuses to serve.
//
// Governing: SPEC-0007 REQ "Search And Retrieval Tools", REQ "Skill Repos",
// REQ "Default-Branch Gate", REQ "Error Handling Standards"; ADR-0030.
//
// @joestump-agent 11/02/2026 - Added for harness#79.
package skillserve

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/skillstore"
)

// twoRepoManager builds a started manager over two serving clones: go-stack,
// served to everyone, and reduit-skills, served only to reduit/*. Both hold
// one active skill matching "cannot find main module".
func twoRepoManager(t *testing.T, summaryMax int) (*Manager, map[string]string) {
	t.Helper()
	dirs := map[string]string{}
	for _, name := range []string{"go-stack", "reduit-skills"} {
		_, clone := newServingClone(t)
		dirs[name] = clone
	}
	tmp := t.TempDir()
	m := NewManager([]core.SkillRepo{
		{Name: "go-stack", Remote: "unused", Path: "skills", ServeTo: []string{"*"}, Public: true},
		{Name: "reduit-skills", Remote: "unused", Path: "skills", ServeTo: []string{"reduit/*"}, Public: true},
	}, filepath.Join(tmp, "index.db"), filepath.Join(tmp, "counts.db"), 30, 60, summaryMax)
	m.dirFor = func(name string) string { return dirs[name] }
	t.Cleanup(m.Stop)
	if err := m.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	return m, dirs
}

func TestSearchIsScopedToTheCaller(t *testing.T) {
	m, _ := twoRepoManager(t, 0)

	// A harness of another project gets nothing from the reduit repo.
	hits, err := m.Search(Caller{Harness: "spotter/agent"}, "cannot find main module", nil, 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	if len(hits) != 1 || hits[0].Repo != "go-stack" {
		t.Fatalf("spotter/agent must not read reduit-skills, got %+v", hits)
	}

	// The served harness sees both.
	hits, err = m.Search(Caller{Harness: "reduit/agent"}, "cannot find main module", nil, 5)
	if err != nil {
		t.Fatalf("search served: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("reduit/agent must see both repos, got %+v", hits)
	}

	// An unattributed caller sees only the bare-star repo.
	hits, err = m.Search(Caller{}, "cannot find main module", nil, 5)
	if err != nil {
		t.Fatalf("search unattributed: %v", err)
	}
	if len(hits) != 1 || hits[0].Repo != "go-stack" {
		t.Fatalf("unattributed caller sees unscoped repos only, got %+v", hits)
	}
}

func TestSearchScopeIsEmptyNotAnError(t *testing.T) {
	tmp := t.TempDir()
	m := NewManager([]core.SkillRepo{
		{Name: "reduit-skills", Remote: "unused", Path: "skills", ServeTo: []string{"reduit/*"}, Public: true},
	}, filepath.Join(tmp, "index.db"), filepath.Join(tmp, "counts.db"), 30, 60, 0)
	_, clone := newServingClone(t)
	m.dirFor = func(string) string { return clone }
	t.Cleanup(m.Stop)
	if err := m.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	hits, err := m.Search(Caller{Harness: "spotter/agent"}, "cannot find main module", nil, 5)
	if err != nil {
		t.Fatalf("being scoped out is not an error: %v", err)
	}
	if len(hits) != 0 {
		t.Fatalf("want no hits, got %+v", hits)
	}
}

func TestSearchResultsAreBodyFree(t *testing.T) {
	m, _ := twoRepoManager(t, 0)
	hits, err := m.Search(Caller{Harness: "reduit/agent"}, "cannot find main module", nil, 5)
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	raw, err := json.Marshal(hits)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"When to use", "Steps", "Evidence", "body"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("search results must carry no body text, found %q in %s", forbidden, raw)
		}
	}
}

func TestSearchLimitDefaultAndCeiling(t *testing.T) {
	idx := openTestIndex(t)
	var entries []Entry
	for i := 0; i < 8; i++ {
		slug := string(rune('a'+i)) + "-module-failure"
		entries = append(entries, testEntry("go-stack", slug, "A module failure in the Go toolchain.", "cannot find main module variant"))
	}
	if err := idx.ReindexRepo("go-stack", entries); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if hits, err := idx.Search("module failure", nil, 0, nil); err != nil || len(hits) != DefaultSearchLimit {
		t.Fatalf("no limit means the default of %d, got %d %v", DefaultSearchLimit, len(hits), err)
	}
	if hits, err := idx.Search("module failure", nil, 99, nil); err != nil || len(hits) != MaxSearchLimit {
		t.Fatalf("the ceiling is %d, got %d %v", MaxSearchLimit, len(hits), err)
	}
}

func TestSearchRepoScopeIsEnforcedInSQL(t *testing.T) {
	idx := openTestIndex(t)
	if err := idx.ReindexRepo("go-stack", []Entry{
		testEntry("go-stack", "pin-go", "Pin the Go toolchain in CI.", "cannot find main module"),
	}); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	if err := idx.ReindexRepo("reduit-skills", []Entry{
		testEntry("reduit-skills", "pin-go", "Pin the Go toolchain in CI.", "cannot find main module"),
	}); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	hits, err := idx.Search("cannot find main module", nil, 5, []string{"go-stack"})
	if err != nil {
		t.Fatalf("scoped search: %v", err)
	}
	if len(hits) != 1 || hits[0].Repo != "go-stack" {
		t.Fatalf("a scoped search must not read the other repo, got %+v", hits)
	}
	if all, err := idx.Search("cannot find main module", nil, 5, nil); err != nil || len(all) != 2 {
		t.Fatalf("no scope reads both repos, got %v %v", all, err)
	}
	if none, err := idx.Search("cannot find main module", nil, 5, []string{}); err != nil || len(none) != 2 {
		t.Fatalf("an empty scope means every repo, got %v %v", none, err)
	}
}

func TestGetSkillSummaryRender(t *testing.T) {
	m, _ := twoRepoManager(t, 0)
	skill, err := m.Get(Caller{Harness: "reduit/agent"}, "go-stack/pin-go", "")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if skill.Render != RenderSummary || skill.Truncated {
		t.Fatalf("want an untruncated summary, got %+v", skill)
	}
	for _, want := range SummarySections {
		if !strings.Contains(skill.Content, "## "+want) {
			t.Fatalf("summary must keep %q, got %q", want, skill.Content)
		}
	}
	for _, notWanted := range []string{"## Failure modes", "## Evidence", "name: pin-go"} {
		if strings.Contains(skill.Content, notWanted) {
			t.Fatalf("summary must not carry %q, got %q", notWanted, skill.Content)
		}
	}
	if len(skill.Content) > core.DefaultSummaryMaxChars {
		t.Fatalf("summary exceeds the default cap: %d", len(skill.Content))
	}
}

func TestGetSkillSummaryIsCapped(t *testing.T) {
	m, dirs := twoRepoManager(t, 200)
	commitSkillRepo(t, dirs["go-stack"], map[string]string{"skills/long-skill/SKILL.md": longSkill("long-skill")})

	skill, err := m.Get(Caller{}, "harness://skills/go-stack/long-skill", RenderSummary)
	if err != nil {
		t.Fatalf("get by resource URI: %v", err)
	}
	if len(skill.Content) > 200 {
		t.Fatalf("summary must fit summary_max_chars, got %d chars", len(skill.Content))
	}
	if !skill.Truncated {
		t.Fatal("a capped summary must say it was capped")
	}
	if !strings.Contains(skill.Content, truncationMarker) {
		t.Fatalf("a capped summary must carry the marker, got %q", skill.Content)
	}
}

// longSkill renders a valid active skill whose Steps section runs past any
// plausible summary_max_chars.
func longSkill(slug string) string {
	var steps strings.Builder
	for i := 0; i < 60; i++ {
		steps.WriteString("- Step ninety-something: verify the assumption holds under the recorded failure.\n")
	}
	return "---\n" +
		"name: " + slug + "\n" +
		"description: A long skill used to prove the summary cap bites.\n" +
		"status: active\n" +
		"tags:\n  - ci\n" +
		"symptoms:\n  - \"go: cannot find main module\"\n" +
		"---\n" +
		"## When to use\n\nA long CI run fails.\n\n" +
		"## Steps\n\n" + steps.String() +
		"\n## Invariants\n\n- Never disagree.\n\n" +
		"## Do not\n\n- Do not bump blindly.\n"
}

func TestGetSkillFullRenderIsTheFile(t *testing.T) {
	m, _ := twoRepoManager(t, 0)
	skill, err := m.Get(Caller{}, "go-stack/pin-go", RenderFull)
	if err != nil {
		t.Fatalf("get full: %v", err)
	}
	if !strings.HasPrefix(skill.Content, "---\nname: pin-go") {
		t.Fatalf("full render is the whole file, got %q", skill.Content)
	}
	if !strings.Contains(skill.Content, "## Evidence") {
		t.Fatalf("full render must include every section, got %q", skill.Content)
	}
	if skill.Truncated {
		t.Fatal("a full render is never truncated")
	}
}

func TestGetSkillRefusesWhatTheCallerMayNotRead(t *testing.T) {
	m, dirs := twoRepoManager(t, 0)

	if _, err := m.Get(Caller{Harness: "spotter/agent"}, "reduit-skills/pin-go", ""); !errors.Is(err, ErrNotServed) {
		t.Fatalf("want ErrNotServed, got %v", err)
	}
	if _, err := m.Get(Caller{}, "no-such-repo/pin-go", ""); !errors.Is(err, ErrSkillRepoNotFound) {
		t.Fatalf("want ErrSkillRepoNotFound, got %v", err)
	}
	if _, err := m.Get(Caller{}, "go-stack/nope", ""); !errors.Is(err, skillstore.ErrSkillNotFound) {
		t.Fatalf("want ErrSkillNotFound, got %v", err)
	}
	if _, err := m.Get(Caller{}, "go-stack/pin-go", "yaml"); !errors.Is(err, ErrBadRender) {
		t.Fatalf("want ErrBadRender, got %v", err)
	}
	for _, id := range []string{"", "go-stack", "pin-go", "go-stack/", "/pin-go", "go-stack/../secrets", "go-stack/.hidden", "go-stack/a/b"} {
		if _, err := m.Get(Caller{}, id, ""); !errors.Is(err, ErrBadSkillID) {
			t.Fatalf("id %q must be refused as malformed, got %v", id, err)
		}
	}

	// A retired skill is not active, so it is not served.
	commitSkillRepo(t, dirs["go-stack"], map[string]string{"skills/old-skill/SKILL.md": testSkill("old-skill", "retired")})
	if _, err := m.Get(Caller{}, "go-stack/old-skill", ""); !errors.Is(err, skillstore.ErrSkillNotFound) {
		t.Fatalf("a retired skill is not retrievable, got %v", err)
	}
}

func TestGetSkillFollowsTheDefaultBranchGate(t *testing.T) {
	m, dirs := twoRepoManager(t, 0)
	gitRun(t, dirs["go-stack"], "checkout", "-q", "--detach", "HEAD")
	m.reindexRepo("go-stack")

	if _, err := m.Get(Caller{}, "go-stack/pin-go", ""); !errors.Is(err, ErrIndexUnavailable) {
		t.Fatalf("a detached clone must not serve get_skill, got %v", err)
	}
}

func TestManagerSkillsIsScopedToTheCaller(t *testing.T) {
	m, _ := twoRepoManager(t, 0)

	all, err := m.Skills(Caller{Harness: "reduit/agent"})
	if err != nil {
		t.Fatalf("skills: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("the served caller sees both repos, got %+v", all)
	}
	scoped, err := m.Skills(Caller{})
	if err != nil {
		t.Fatalf("skills unattributed: %v", err)
	}
	if len(scoped) != 1 || scoped[0].Repo != "go-stack" || scoped[0].Slug != "pin-go" {
		t.Fatalf("an unattributed caller sees the star repo only, got %+v", scoped)
	}
}

func TestParseSkillIDAcceptsResourceURIs(t *testing.T) {
	for _, id := range []string{"go-stack/pin-go", "harness://skills/go-stack/pin-go"} {
		repo, slug, err := ParseSkillID(id)
		if err != nil {
			t.Fatalf("parse %q: %v", id, err)
		}
		if repo != "go-stack" || slug != "pin-go" {
			t.Fatalf("parse %q gave %q/%q", id, repo, slug)
		}
	}
}

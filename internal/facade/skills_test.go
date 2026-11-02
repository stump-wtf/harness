// Skill tool tests: registration, per-caller scoping as the facade applies it,
// retrieval counting, resource addressing, and the structured error codes.
//
// Governing: SPEC-0007 REQ "Search And Retrieval Tools", REQ "Skill Repos",
// REQ "Retrieval-Count Retirement"; SPEC-0005 REQ "Capability Scoping",
// REQ "Caller Identity"; ADR-0030, ADR-0035.
//
// @joestump-agent 11/02/2026 - Added for harness#79.
package facade

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/skillserve"
	"github.com/stump-wtf/harness/internal/skillstore"
)

// retrieval is one counted row the fake recorded.
type retrieval struct {
	repo, slug, harness, run string
	at                       time.Time
}

// fakeSkills is the serving surface the handlers talk to, so scoping and
// counting are tested without a serving clone.
type fakeSkills struct {
	hits     []skillserve.SearchHit
	refs     []skillserve.SkillRef
	skills   map[string]skillserve.SkillContent
	err      error
	countErr error

	searches []skillserve.Caller
	counts   []retrieval
}

func (f *fakeSkills) Search(caller skillserve.Caller, query string, paths []string, limit int) ([]skillserve.SearchHit, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.searches = append(f.searches, caller)
	if limit <= 0 {
		limit = skillserve.DefaultSearchLimit
	}
	if limit > skillserve.MaxSearchLimit {
		limit = skillserve.MaxSearchLimit
	}
	if limit < len(f.hits) {
		return f.hits[:limit], nil
	}
	return f.hits, nil
}

func (f *fakeSkills) Get(caller skillserve.Caller, id, render string) (skillserve.SkillContent, error) {
	if f.err != nil {
		return skillserve.SkillContent{}, f.err
	}
	skill, ok := f.skills[id]
	if !ok {
		return skillserve.SkillContent{}, skillstore.ErrSkillNotFound
	}
	return skill, nil
}

func (f *fakeSkills) Skills(caller skillserve.Caller) ([]skillserve.SkillRef, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.refs, nil
}

func (f *fakeSkills) RecordRetrieval(repo, slug, harness, run string, at time.Time) error {
	if f.countErr != nil {
		return f.countErr
	}
	f.counts = append(f.counts, retrieval{repo, slug, harness, run, at})
	return nil
}

var _ SkillSource = (*fakeSkills)(nil)

func TestSkillToolsAreReadClass(t *testing.T) {
	for _, tool := range SkillTools {
		if tool.Class != ClassRead {
			t.Errorf("tool %q has class %q, want read", tool.Name, tool.Class)
		}
	}
	names := map[string]bool{}
	for _, tool := range SkillTools {
		names[tool.Name] = true
	}
	if !names["search_skills"] || !names["get_skill"] {
		t.Fatalf("want search_skills and get_skill, got %v", SkillTools)
	}
}

func TestSkillToolDescriptionsCarryTheUsageGuidance(t *testing.T) {
	// SPEC-0007 REQ "Search And Retrieval Tools": the descriptions must steer
	// callers to search while planning and while reviewing a concrete draft or
	// diff, and to try several phrasings including literal error text.
	var search, get string
	for _, tool := range SkillTools {
		switch tool.Name {
		case "search_skills":
			search = tool.Description
		case "get_skill":
			get = tool.Description
		}
	}
	for _, want := range []string{"planning", "diff", "phrasings", "error text"} {
		if !strings.Contains(search, want) {
			t.Errorf("search_skills description omits %q: %s", want, search)
		}
	}
	for _, want := range []string{"summary", "full"} {
		if !strings.Contains(get, want) {
			t.Errorf("get_skill description omits %q: %s", want, get)
		}
	}
}

func TestSkillToolsRegisteredOnlyWithASkillRepo(t *testing.T) {
	// SPEC-0007 REQ "Skill Repos": with no skill repo declared the facade does
	// not register the skill tools.
	for _, tool := range AllTools(false) {
		if tool.Name == "search_skills" || tool.Name == "get_skill" {
			t.Fatalf("skill tool %q must not be registered with no skill repo", tool.Name)
		}
	}
	found := map[string]bool{}
	for _, tool := range AllTools(true) {
		found[tool.Name] = true
	}
	if !found["search_skills"] || !found["get_skill"] {
		t.Fatalf("declared skill repo must register both skill tools, got %v", found)
	}
}

func TestCheckToolSkillAccess(t *testing.T) {
	h := core.Harness{Name: "agent"}
	if err := CheckTool(h, "search_skills", false); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("want ErrUnknownTool with no skill repo, got %v", err)
	}
	// A read-class tool is available whatever mcp_allow says.
	for _, allow := range [][]string{nil, {"read"}, {"write"}} {
		if err := CheckTool(core.Harness{Name: "agent", MCPAllow: allow}, "get_skill", true); err != nil {
			t.Fatalf("mcp_allow %v must not block a read tool: %v", allow, err)
		}
	}
}

func TestHandleSearchSkillsCountsEveryHit(t *testing.T) {
	f := &fakeSkills{hits: []skillserve.SearchHit{
		{Repo: "go-stack", Slug: "pin-go", Description: "Pin the Go toolchain."},
		{Repo: "reduit-skills", Slug: "pin-ci", Description: "Pin the CI image."},
	}}
	at := time.Unix(1700000000, 0)
	res := HandleSearchSkills(f, skillserve.Caller{Harness: "reduit/agent", Run: "42"}, "toolchain", nil, 0, at)
	if res.Error != "" || res.Code != "" {
		t.Fatalf("want no error, got %+v", res)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("want 2 hits, got %+v", res.Hits)
	}
	if len(f.counts) != 2 {
		t.Fatalf("every returned skill is one retrieval, got %+v", f.counts)
	}
	if f.counts[0] != (retrieval{"go-stack", "pin-go", "reduit/agent", "42", at}) {
		t.Fatalf("count row is %v", f.counts[0])
	}
}

func TestHandleSearchSkillsCapsTheLimit(t *testing.T) {
	var hits []skillserve.SearchHit
	for i := 0; i < 8; i++ {
		hits = append(hits, skillserve.SearchHit{Repo: "go-stack", Slug: string(rune('a' + i)), Description: "d"})
	}
	f := &fakeSkills{hits: hits}
	res := HandleSearchSkills(f, skillserve.Caller{Harness: "reduit/agent"}, "d", nil, 99, time.Unix(1, 0))
	if len(res.Hits) != skillserve.MaxSearchLimit {
		t.Fatalf("the ceiling is %d hits, got %d", skillserve.MaxSearchLimit, len(res.Hits))
	}
	if len(f.counts) != skillserve.MaxSearchLimit {
		t.Fatalf("only returned skills are counted, got %d", len(f.counts))
	}
}

func TestHandleSearchSkillsCarriesNoBodies(t *testing.T) {
	f := &fakeSkills{hits: []skillserve.SearchHit{{Repo: "go-stack", Slug: "pin-go", Description: "d"}}}
	res := HandleSearchSkills(f, skillserve.Caller{}, "d", nil, 3, time.Unix(1, 0))
	// SearchHit has no body field at all: what the tool can return is the
	// identifiers and the description.
	for _, h := range res.Hits {
		if h.Repo == "" || h.Slug == "" {
			t.Fatalf("hits must carry repo-qualified identifiers, got %+v", h)
		}
	}
}

func TestHandleSearchSkillsThreadsTheCaller(t *testing.T) {
	f := &fakeSkills{}
	HandleSearchSkills(f, skillserve.Caller{Harness: "spotter/agent", Run: "7"}, "x", []string{"ci.yml"}, 3, time.Unix(1, 0))
	if len(f.searches) != 1 || f.searches[0].Harness != "spotter/agent" || f.searches[0].Run != "7" {
		t.Fatalf("the attributed caller must reach the serving layer, got %+v", f.searches)
	}
}

func TestHandleSearchSkillsErrorCodes(t *testing.T) {
	f := &fakeSkills{err: skillserve.ErrIndexUnavailable}
	res := HandleSearchSkills(f, skillserve.Caller{}, "x", nil, 3, time.Unix(1, 0))
	if res.Code != "index_unavailable" {
		t.Fatalf("no skill repo declared answers index_unavailable, got %+v", res)
	}
	if len(f.counts) != 0 {
		t.Fatalf("a failed search counts nothing, got %+v", f.counts)
	}
}

func TestHandleSearchSkillsReportsCountFailureWithoutLosingHits(t *testing.T) {
	f := &fakeSkills{
		hits:     []skillserve.SearchHit{{Repo: "go-stack", Slug: "pin-go", Description: "d"}},
		countErr: errors.New("counts store not open"),
	}
	res := HandleSearchSkills(f, skillserve.Caller{}, "d", nil, 3, time.Unix(1, 0))
	if len(res.Hits) != 1 {
		t.Fatalf("a counting failure must not lose the answer, got %+v", res)
	}
	if res.CountError == "" {
		t.Fatal("a counting failure must be visible: retirement reads these counts")
	}
}

func TestHandleGetSkillServesAndCountsOnce(t *testing.T) {
	f := &fakeSkills{skills: map[string]skillserve.SkillContent{
		"go-stack/pin-go": {Repo: "go-stack", Slug: "pin-go", Render: skillserve.RenderSummary, Content: "## Steps\n"},
	}}
	at := time.Unix(1700000001, 0)
	res := HandleGetSkill(f, skillserve.Caller{Harness: "reduit/agent", Run: "9"}, "go-stack/pin-go", "", at)
	if res.Error != "" || res.Skill == nil {
		t.Fatalf("want a skill, got %+v", res)
	}
	if len(f.counts) != 1 || f.counts[0].slug != "pin-go" || f.counts[0].harness != "reduit/agent" || f.counts[0].run != "9" {
		t.Fatalf("want one counted row, got %+v", f.counts)
	}
}

func TestHandleGetSkillErrorCodes(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"repo not declared", skillserve.ErrSkillRepoNotFound, "skill_repo_not_found"},
		{"repo not served", skillserve.ErrNotServed, "not_permitted"},
		{"skill missing", skillstore.ErrSkillNotFound, "skill_not_found"},
		{"malformed id", skillserve.ErrBadSkillID, "bad_request"},
		{"unknown render", skillserve.ErrBadRender, "bad_request"},
		{"index down", skillserve.ErrIndexUnavailable, "index_unavailable"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeSkills{err: tc.err}
			res := HandleGetSkill(f, skillserve.Caller{}, "go-stack/pin-go", "", time.Unix(1, 0))
			if res.Code != tc.want {
				t.Fatalf("want %s, got %+v", tc.want, res)
			}
			if res.Skill != nil || len(f.counts) != 0 {
				t.Fatalf("a refused retrieval must not count, got %+v %+v", res.Skill, f.counts)
			}
		})
	}
}

func TestSkillResourcesAddressEveryActiveSkill(t *testing.T) {
	f := &fakeSkills{refs: []skillserve.SkillRef{
		{Repo: "go-stack", Slug: "pin-ci", Description: "Pin CI"},
		{Repo: "go-stack", Slug: "pin-go", Description: "Pin Go"},
	}}
	res, err := SkillResources(f, skillserve.Caller{})
	if err != nil {
		t.Fatalf("resources: %v", err)
	}
	if len(res) != 2 {
		t.Fatalf("want 2 resources, got %+v", res)
	}
	if res[0].URI != "harness://skills/go-stack/pin-ci" && res[0].URI != "harness://skills/go-stack/pin-go" {
		t.Fatalf("resource URI must be harness://skills/<repo>/<slug>, got %s", res[0].URI)
	}
	if SkillResourceURI("go-stack", "pin-go") != "harness://skills/go-stack/pin-go" {
		t.Fatalf("SkillResourceURI gave %q", SkillResourceURI("go-stack", "pin-go"))
	}
}

func TestHandleReadSkillResourceServesTheFile(t *testing.T) {
	f := &fakeSkills{skills: map[string]skillserve.SkillContent{
		"go-stack/pin-go": {Repo: "go-stack", Slug: "pin-go", Render: skillserve.RenderFull, Content: "---\nname: pin-go\n---\n"},
	}}
	res := HandleReadSkillResource(f, skillserve.Caller{Harness: "reduit/agent", Run: "3"}, "harness://skills/go-stack/pin-go", time.Unix(1700000002, 0))
	if res.Error != "" || res.Skill == nil || res.Skill.Render != skillserve.RenderFull {
		t.Fatalf("a skill resource reads the whole file, got %+v", res)
	}
	if len(f.counts) != 1 {
		t.Fatalf("reading a resource is one retrieval, got %+v", f.counts)
	}

	unknown := HandleReadSkillResource(f, skillserve.Caller{}, "harness://trajectories/go-stack/s1", time.Unix(1, 0))
	if unknown.Code != "unknown_resource" || unknown.Error == "" || unknown.Skill != nil {
		t.Fatalf("a URI outside the scheme is an unknown resource, got %+v", unknown)
	}
}

// get_skill: fetch and render one skill from a serving clone for one caller.
// Nothing is projected into a harness's directory: the daemon reads its own
// read-only serving clone and returns the text (SPEC-0007 REQ "Skill Repos",
// ADR-0030).
//
// Governing: SPEC-0007 REQ "Search And Retrieval Tools", REQ "Skill Artifact",
// REQ "Skill Repos", REQ "Error Handling Standards"; ADR-0030.
package skillserve

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/skillstore"
)

// Renders accepted by get_skill (SPEC-0007 REQ "Search And Retrieval Tools").
const (
	RenderSummary = "summary"
	RenderFull    = "full"
)

// ErrBadRender marks a render the tool does not know.
var ErrBadRender = errors.New("skillserve: unknown render")

// SkillContent is one skill as get_skill returns it.
type SkillContent struct {
	Repo    string `json:"repo"`
	Slug    string `json:"slug"`
	Render  string `json:"render"`
	Content string `json:"content"`
	// Truncated reports a summary cut at summary_max_chars; a full render is
	// never truncated.
	Truncated bool `json:"truncated,omitempty"`
}

// ParseSkillID splits a skill identifier, which is "<repo>/<slug>" and may
// also arrive as the harness://skills/<repo>/<slug> resource URI.
func ParseSkillID(id string) (repo, slug string, err error) {
	id = strings.TrimSpace(id)
	id = strings.TrimPrefix(id, "harness://skills/")
	repo, slug, ok := strings.Cut(id, "/")
	if !ok || repo == "" || slug == "" {
		return "", "", fmt.Errorf("%w %q: want <repo>/<slug>", ErrBadSkillID, id)
	}
	if err := checkSegment(slug); err != nil {
		return "", "", fmt.Errorf("%w %q: %v", ErrBadSkillID, id, err)
	}
	if err := checkSegment(repo); err != nil {
		return "", "", fmt.Errorf("%w %q: %v", ErrBadSkillID, id, err)
	}
	return repo, slug, nil
}

// checkSegment rejects anything that is not a single path element, so a skill
// id cannot address a file outside the repo's configured skill path.
func checkSegment(seg string) error {
	if seg == "." || seg == ".." || strings.ContainsAny(seg, "/\\") || strings.HasPrefix(seg, ".") {
		return fmt.Errorf("%q is not a path element", seg)
	}
	return nil
}

// Get returns one skill for one caller: the summary render capped at
// summary_max_chars, or with render "full" the whole file. The repo must be
// declared, serve the caller, and hold the skill as active.
func (m *Manager) Get(caller Caller, id, render string) (SkillContent, error) {
	repoName, slug, err := ParseSkillID(id)
	if err != nil {
		return SkillContent{}, err
	}
	switch render {
	case "", RenderSummary, RenderFull:
	default:
		return SkillContent{}, fmt.Errorf("%w %q: want %q or %q", ErrBadRender, render, RenderSummary, RenderFull)
	}

	m.mu.Lock()
	var rc core.SkillRepo
	var dir string
	state := CloneState("")
	found := false
	for i := range m.repos {
		if m.repos[i].Name != repoName {
			continue
		}
		rc = m.repos[i]
		dir = m.dirFor(rc.Name)
		state = m.states[rc.Name].State
		found = true
		break
	}
	summaryMax := m.summaryMax
	m.mu.Unlock()
	if !found {
		return SkillContent{}, fmt.Errorf("%w: %s", ErrSkillRepoNotFound, repoName)
	}
	if !ServesTo(rc.ServeToOrDefault(), caller) {
		return SkillContent{}, fmt.Errorf("%w: %s", ErrNotServed, repoName)
	}

	// get_skill reads through the same gate the index was built behind: a
	// clone on a proposal branch or with a dirty tree must not serve content
	// that never passed it (SPEC-0007 REQ "Default-Branch Gate").
	if state != CloneOK {
		return SkillContent{}, fmt.Errorf("%w: %s is %s", ErrIndexUnavailable, repoName, state)
	}
	rel := filepath.ToSlash(filepath.Join(rc.Path, slug, "SKILL.md"))
	if _, err := gitRO(dir, "cat-file", "-e", "HEAD:"+rel); err != nil {
		return SkillContent{}, fmt.Errorf("%w: %s/%s: %v", skillstore.ErrSkillNotFound, repoName, slug, err)
	}
	content, err := gitRO(dir, "show", "HEAD:"+rel)
	if err != nil {
		return SkillContent{}, fmt.Errorf("%w: %s/%s: %v", skillstore.ErrSkillNotFound, repoName, slug, err)
	}
	fm, err := ParseSkillFile(content)
	if err != nil {
		return SkillContent{}, fmt.Errorf("skillserve: %s/%s: %v", repoName, slug, err)
	}
	if fm.Status != StatusActive {
		return SkillContent{}, fmt.Errorf("%w: %s/%s is not active", skillstore.ErrSkillNotFound, repoName, slug)
	}

	out := SkillContent{Repo: repoName, Slug: slug}
	if render == RenderFull {
		out.Render = RenderFull
		out.Content = content
		return out, nil
	}
	full := Summary(skillBody(content), 0)
	out.Render = RenderSummary
	out.Content = capChars(full, summaryMax)
	out.Truncated = len(out.Content) < len(full)
	return out, nil
}

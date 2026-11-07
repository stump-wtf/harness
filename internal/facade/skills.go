// Skill search and retrieval on the facade: the search_skills and get_skill
// tools, and the harness://skills/<repo>/<slug> resource each active skill is
// exposed as (SPEC-0007 REQ "Search And Retrieval Tools").
//
// Both are read-class, so under SPEC-0005 REQ "Capability Scoping" every
// harness sees them; what differs per caller is which skill repos a search may
// touch and whether one skill is readable, which skillserve scopes by the
// repo's serve_to and the caller SPEC-0005 attributes. Serving these over a
// live endpoint arrives with the gateway; the handlers take the attributed
// caller as input, which is what makes the scoping testable now.
//
// Skills are never written into a harness's directory: a caller searches, then
// reads what it needs (ADR-0030).
//
// Governing: SPEC-0007 REQ "Search And Retrieval Tools", REQ "Skill Repos";
// SPEC-0005 REQ "Capability Scoping", REQ "Caller Identity"; ADR-0030,
// ADR-0035.
package facade

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/stump-wtf/harness/internal/skillserve"
	"github.com/stump-wtf/harness/internal/skillstore"
)

// SkillResourceScheme is the URI scheme of the per-skill MCP resource.
const SkillResourceScheme = "harness://skills/"

// SkillSource is the skill-serving surface these tools need:
// *skillserve.Manager satisfies it, and tests supply a fake so the facade's
// scoping and counting are testable without a serving clone.
type SkillSource interface {
	Search(caller skillserve.Caller, query string, paths []string, limit int) ([]skillserve.SearchHit, error)
	Get(caller skillserve.Caller, id, render string) (skillserve.SkillContent, error)
	Skills(caller skillserve.Caller) ([]skillserve.SkillRef, error)
	RecordRetrieval(repo, slug, harness, run string, at time.Time) error
}

var _ SkillSource = (*skillserve.Manager)(nil)

// SkillTools are the two read-class skill tools. They are registered only when
// at least one skill repo is declared (SPEC-0007 REQ "Skill Repos").
//
// Their descriptions carry the usage guidance SPEC-0007 requires of them:
// search while planning and again against a concrete draft or diff, and try
// several phrasings, because retrieval is lexical and an error string copied
// verbatim is the strongest query there is.
var SkillTools = []Tool{
	{
		Name:  "search_skills",
		Class: ClassRead,
		Description: "Search the declared skill repos. Call it while planning, and again while reviewing a " +
			"concrete draft or diff. Pass a query plus the optional paths you are touching; try several " +
			"phrasings, including any literal error text. Returns up to 5 repo-qualified skill ids with " +
			"descriptions only, never bodies. Read a hit with get_skill.",
	},
	{
		Name:  "get_skill",
		Class: ClassRead,
		Description: "Read one skill by <repo>/<slug> id (or its harness://skills/<repo>/<slug> URI). The " +
			"default render is the summary (When to use, Steps, Invariants, Do not); render \"full\" returns " +
			"the whole file.",
	},
}

// SkillResourceURI is the MCP resource URI of one active skill.
func SkillResourceURI(repo, slug string) string {
	return SkillResourceScheme + repo + "/" + slug
}

// Resource is one MCP resource offered by the facade.
type Resource struct {
	URI         string `json:"uri"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// SkillResources lists the per-skill resources visible to one caller: every
// active skill in a repo whose serve_to admits it (SPEC-0007 REQ "Search And
// Retrieval Tools").
func SkillResources(src SkillSource, caller skillserve.Caller) ([]Resource, error) {
	refs, err := src.Skills(caller)
	if err != nil {
		return nil, err
	}
	out := make([]Resource, 0, len(refs))
	for _, r := range refs {
		out = append(out, Resource{
			URI:         SkillResourceURI(r.Repo, r.Slug),
			Name:        r.Repo + "/" + r.Slug,
			Description: r.Description,
		})
	}
	return out, nil
}

// SearchSkillsResult is the result of the search_skills facade tool. Hits
// carry identifiers and descriptions only. CountError reports that the hits
// were returned but their retrieval counts failed to record, which affects a
// later retirement decision and so must not be silent (SPEC-0007 REQ
// "Retrieval-Count Retirement").
type SearchSkillsResult struct {
	Hits       []skillserve.SearchHit `json:"hits,omitempty"`
	CountError string                 `json:"countError,omitempty"`
	Error      string                 `json:"error,omitempty"`
	Code       string                 `json:"errorCode,omitempty"`
}

// GetSkillResult is the result of the get_skill facade tool.
type GetSkillResult struct {
	Skill      *skillserve.SkillContent `json:"skill,omitempty"`
	CountError string                   `json:"countError,omitempty"`
	Error      string                   `json:"error,omitempty"`
	Code       string                   `json:"errorCode,omitempty"`
}

// HandleSearchSkills executes search_skills for one attributed caller. limit
// of 0 means the default; the ceiling is skillserve.MaxSearchLimit either way.
// Each returned hit is one retrieval, counted per (repo, skill, harness, run).
func HandleSearchSkills(src SkillSource, caller skillserve.Caller, query string, paths []string, limit int, at time.Time) SearchSkillsResult {
	hits, err := src.Search(caller, query, paths, limit)
	if err != nil {
		return SearchSkillsResult{Error: err.Error(), Code: errorCode(err)}
	}
	out := SearchSkillsResult{Hits: hits}
	if at.IsZero() {
		at = time.Now()
	}
	if err := recordRetrievals(src, caller, hits, at); err != nil {
		out.CountError = err.Error()
	}
	return out
}

// HandleGetSkill executes get_skill for one attributed caller. render is ""
// (the summary), "summary" or "full"; id is <repo>/<slug> or the skill's
// harness://skills URI. The retrieval is counted once.
func HandleGetSkill(src SkillSource, caller skillserve.Caller, id, render string, at time.Time) GetSkillResult {
	skill, err := src.Get(caller, id, render)
	if err != nil {
		return GetSkillResult{Error: err.Error(), Code: errorCode(err)}
	}
	out := GetSkillResult{Skill: &skill}
	if at.IsZero() {
		at = time.Now()
	}
	if err := src.RecordRetrieval(skill.Repo, skill.Slug, caller.Harness, caller.Run, at); err != nil {
		out.CountError = err.Error()
	}
	return out
}

// HandleReadSkillResource serves one harness://skills/<repo>/<slug> resource:
// the whole skill file, counted as the retrieval it is. A URI outside the
// scheme is not a skill resource at all.
func HandleReadSkillResource(src SkillSource, caller skillserve.Caller, uri string, at time.Time) GetSkillResult {
	if !strings.HasPrefix(uri, SkillResourceScheme) {
		return GetSkillResult{
			Error: fmt.Errorf("%w: %s", ErrUnknownResource, uri).Error(),
			Code:  "unknown_resource",
		}
	}
	return HandleGetSkill(src, caller, strings.TrimPrefix(uri, SkillResourceScheme), skillserve.RenderFull, at)
}

// recordRetrievals counts every hit as one retrieval. Counting is daemon
// state: a failure leaves the search answer intact, so it is reported rather
// than fatal.
func recordRetrievals(src SkillSource, caller skillserve.Caller, hits []skillserve.SearchHit, at time.Time) error {
	var firstErr error
	for _, h := range hits {
		if err := src.RecordRetrieval(h.Repo, h.Slug, caller.Harness, caller.Run, at); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// skillErrorCode maps the skillserve and skillstore sentinels onto stable
// error codes for a tool response (SPEC-0007 REQ "Error Handling Standards").
// It returns ok=false for an error that is not one of them.
func skillErrorCode(err error) (code string, ok bool) {
	switch {
	case errors.Is(err, skillserve.ErrIndexUnavailable):
		return "index_unavailable", true
	case errors.Is(err, skillserve.ErrSkillRepoNotFound):
		return "skill_repo_not_found", true
	case errors.Is(err, skillserve.ErrNotServed):
		return "not_permitted", true
	case errors.Is(err, skillstore.ErrSkillNotFound):
		return "skill_not_found", true
	case errors.Is(err, skillserve.ErrBadSkillID), errors.Is(err, skillserve.ErrBadRender):
		return "bad_request", true
	}
	return "", false
}

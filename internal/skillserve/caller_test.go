// serve_to selector tests: which caller may search which skill repo.
//
// Governing: SPEC-0007 REQ "Search And Retrieval Tools", REQ "Skill Repos";
// SPEC-0005 REQ "Caller Identity".
//
// @joestump-agent 11/02/2026 - Added for harness#79.
package skillserve

import (
	"testing"

	"github.com/stump-wtf/harness/internal/core"
)

func TestServesToSelectorTable(t *testing.T) {
	tests := []struct {
		name    string
		serveTo []string
		caller  Caller
		want    bool
	}{
		{"bare star serves a named harness", []string{"*"}, Caller{Harness: "reduit/agent"}, true},
		{"bare star serves the unattributed caller", []string{"*"}, Caller{}, true},
		{"star without slashes is the bare star", []string{" * "}, Caller{}, true},
		{"project glob serves its own harness", []string{"reduit/*"}, Caller{Harness: "reduit/agent"}, true},
		{"project glob serves another project", []string{"reduit/*"}, Caller{Harness: "spotter/agent"}, false},
		{"star does not cross a slash", []string{"reduit/*"}, Caller{Harness: "reduit/nested/agent"}, false},
		{"glob serves no unattributed caller", []string{"reduit/*"}, Caller{}, false},
		{"exact name serves only that harness", []string{"reduit/agent"}, Caller{Harness: "reduit/reviewer"}, false},
		{"exact name serves that harness", []string{"reduit/agent"}, Caller{Harness: "reduit/agent"}, true},
		{"one matching selector of several serves", []string{"spotter/*", "reduit/*"}, Caller{Harness: "reduit/agent"}, true},
		{"empty selector list serves nobody", nil, Caller{Harness: "reduit/agent"}, false},
		{"bad glob is refused, not a wildcard", []string{"[oops"}, Caller{Harness: "reduit/agent"}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ServesTo(tc.serveTo, tc.caller); got != tc.want {
				t.Fatalf("ServesTo(%q, %+v) = %v, want %v", tc.serveTo, tc.caller, got, tc.want)
			}
		})
	}
}

func TestServeToOrDefaultServesEveryone(t *testing.T) {
	r := core.SkillRepo{Name: "unconfigured"}
	if !ServesTo(r.ServeToOrDefault(), Caller{}) {
		t.Fatal("an unset serve_to must default to the bare star")
	}
}

func TestServedNamesFollowsDeclarationOrder(t *testing.T) {
	m := NewManager([]core.SkillRepo{
		{Name: "go-stack", ServeTo: []string{"*"}},
		{Name: "reduit-skills", ServeTo: []string{"reduit/*"}},
		{Name: "secrets-handling", ServeTo: []string{"auditor/*"}},
	}, "", "", 30, 60, 0)
	if got := m.servedNames(Caller{Harness: "reduit/agent"}); len(got) != 2 || got[0] != "go-stack" || got[1] != "reduit-skills" {
		t.Fatalf("reduit/agent should see the star repo and its own, got %v", got)
	}
	if got := m.servedNames(Caller{}); len(got) != 1 || got[0] != "go-stack" {
		t.Fatalf("an unattributed caller sees only the bare-star repo, got %v", got)
	}
}

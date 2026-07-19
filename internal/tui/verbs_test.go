package tui

import (
	"testing"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

func sampleHarnesses() []protocol.HarnessInfo {
	return []protocol.HarnessInfo{
		{Name: "crush-signal", State: "running"},
		{Name: "claude-src", State: "running"},
		{Name: "reduit-agent", State: "degraded", Flapping: true},
		{Name: "docs-glow", State: "failed", LastExitCode: 1},
		{Name: "backup-watch", State: "stopped"},
	}
}

func sampleProfiles() []protocol.ProfileInfo {
	return []protocol.ProfileInfo{
		{Name: "signal-ops", Description: "signal", Harnesses: []string{"crush-signal", "claude-src"}, Active: true},
		{Name: "reduit", Description: "reduit", Harnesses: []string{"reduit-agent"}},
	}
}

// TestPaletteVerbPlusTarget is the SPEC-0001 scenario "Verb plus target":
// typing "rest redu" offers "restart reduit-agent" as the top match, and that
// entry carries the restart verb + reduit-agent target so Enter executes it.
func TestPaletteVerbPlusTarget(t *testing.T) {
	cmds := BuildCommands(CLIVerbs(), sampleHarnesses(), sampleProfiles())
	got := FilterCommands(cmds, "rest redu")
	if len(got) == 0 {
		t.Fatal("no matches for \"rest redu\"")
	}
	top := got[0]
	if top.Display != "restart reduit-agent" {
		t.Fatalf("top match = %q, want %q", top.Display, "restart reduit-agent")
	}
	if top.Verb != "restart" || top.Target != "reduit-agent" {
		t.Fatalf("top match verb/target = %q/%q, want restart/reduit-agent", top.Verb, top.Target)
	}
}

// TestPaletteMirrorsCLI verifies the palette verb set mirrors the CLI 1:1 — every
// CLI verb the run() dispatcher handles has a palette Verb (SPEC-0001 REQ
// "Command Palette": "never drift").
func TestPaletteMirrorsCLI(t *testing.T) {
	cliVerbs := map[string]bool{
		"attach": true, "start": true, "stop": true, "restart": true,
		"describe": true, "logs": true, "profile": true, "reload": true,
		"list": true, "profiles": true, "daemon-info": true,
		"new": true, "edit": true, "delete": true,
	}
	have := map[string]bool{}
	for _, v := range CLIVerbs() {
		have[v.Name] = true
	}
	for name := range cliVerbs {
		if !have[name] {
			t.Errorf("palette is missing CLI verb %q", name)
		}
	}
}

// TestFilterEmptyReturnsAll verifies an empty query returns everything in order.
func TestFilterEmptyReturnsAll(t *testing.T) {
	cmds := BuildCommands(CLIVerbs(), sampleHarnesses(), sampleProfiles())
	if got := FilterCommands(cmds, ""); len(got) != len(cmds) {
		t.Fatalf("empty query returned %d, want %d", len(got), len(cmds))
	}
}

// TestFuzzyScoreRanking verifies a tighter match ranks ahead of a looser one.
func TestFuzzyScoreRanking(t *testing.T) {
	cmds := []Command{
		{Verb: "start", Target: "backup-watch", Display: "start backup-watch"},
		{Verb: "restart", Target: "reduit-agent", Display: "restart reduit-agent"},
	}
	got := FilterCommands(cmds, "start")
	if len(got) == 0 || got[0].Display != "start backup-watch" {
		t.Fatalf("expected 'start backup-watch' first, got %+v", got)
	}
}

package tui

// Governing: SPEC-0001 REQ "Command Palette" — Ctrl-k / : fuzzy over verbs AND
// harness names, mirroring the scriptable CLI verbs 1:1 so the palette and CLI
// never drift; ADR-0002 (control ops mirror the CLI). The verb table here is the
// single source the palette expands into concrete commands, and it is the same
// set the CLI's run() dispatches (cmd/harness/main.go).

import (
	"sort"
	"strings"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// TargetKind says what a verb operates on: nothing, a harness, or a profile.
type TargetKind int

const (
	// TargetNone: the verb takes no argument (list, reload, daemon-info, new).
	TargetNone TargetKind = iota
	// TargetHarness: the verb targets one harness (start, stop, attach, …).
	TargetHarness
	// TargetProfile: the verb targets one profile (use-profile).
	TargetProfile
)

// Verb is one palette/CLI verb. Name is the canonical token that also appears in
// the CLI (cmd/harness usage); Target says whether it expands over harness or
// profile names.
type Verb struct {
	Name    string
	Aliases []string
	Target  TargetKind
	Desc    string
}

// CLIVerbs is the palette's verb table, mirroring the CLI 1:1 (SPEC-0001 REQ
// "Command Palette"). Keep this in lockstep with cmd/harness/main.go's run().
func CLIVerbs() []Verb {
	return []Verb{
		{Name: "attach", Target: TargetHarness, Desc: "attach to a harness terminal"},
		{Name: "start", Target: TargetHarness, Desc: "start a harness"},
		{Name: "stop", Target: TargetHarness, Desc: "stop a harness"},
		{Name: "restart", Target: TargetHarness, Desc: "restart a harness"},
		{Name: "describe", Target: TargetHarness, Desc: "show a harness in detail"},
		{Name: "logs", Target: TargetHarness, Desc: "tail a harness log"},
		{Name: "edit", Target: TargetHarness, Desc: "edit a harness"},
		{Name: "delete", Target: TargetHarness, Desc: "delete a harness"},
		{Name: "profile", Aliases: []string{"use-profile"}, Target: TargetProfile, Desc: "activate a profile"},
		{Name: "new", Target: TargetNone, Desc: "create a new harness"},
		{Name: "list", Target: TargetNone, Desc: "list harnesses"},
		{Name: "profiles", Target: TargetNone, Desc: "list profiles"},
		{Name: "reload", Target: TargetNone, Desc: "reload the daemon config"},
		{Name: "daemon-info", Target: TargetNone, Desc: "show daemon status"},
	}
}

// Command is one concrete palette entry: a verb bound to a specific target (or
// none). Display is what the user sees and fuzzy-matches against — e.g.
// "restart reduit-agent".
type Command struct {
	Verb    string
	Target  string
	Display string
	Desc    string
}

// BuildCommands expands the verb table over the live harness and profile names
// into the full flat command list the palette fuzzy-filters (SPEC-0001).
func BuildCommands(verbs []Verb, harnesses []protocol.HarnessInfo, profiles []protocol.ProfileInfo) []Command {
	var out []Command
	for _, v := range verbs {
		switch v.Target {
		case TargetNone:
			out = append(out, Command{Verb: v.Name, Display: v.Name, Desc: v.Desc})
		case TargetHarness:
			for _, h := range harnesses {
				out = append(out, Command{Verb: v.Name, Target: h.Name, Display: v.Name + " " + h.Name, Desc: v.Desc})
			}
		case TargetProfile:
			for _, p := range profiles {
				out = append(out, Command{Verb: v.Name, Target: p.Name, Display: v.Name + " " + p.Name, Desc: v.Desc})
			}
		}
	}
	return out
}

// scored pairs a command with its fuzzy score for ranking.
type scored struct {
	cmd   Command
	score int
	order int
}

// FilterCommands returns the commands matching query, best first. An empty query
// returns everything in table order. Matching is subsequence fuzzy over the
// display string with spaces ignored, so "rest redu" matches
// "restart reduit-agent" (SPEC-0001 scenario "Verb plus target").
func FilterCommands(cmds []Command, query string) []Command {
	q := normalize(query)
	if q == "" {
		out := make([]Command, len(cmds))
		copy(out, cmds)
		return out
	}
	var hits []scored
	for i, c := range cmds {
		if score, ok := fuzzyScore(q, normalize(c.Display)); ok {
			hits = append(hits, scored{cmd: c, score: score, order: i})
		}
	}
	sort.SliceStable(hits, func(a, b int) bool {
		if hits[a].score != hits[b].score {
			return hits[a].score < hits[b].score
		}
		return hits[a].order < hits[b].order
	})
	out := make([]Command, len(hits))
	for i, h := range hits {
		out[i] = h.cmd
	}
	return out
}

// normalize lowercases and strips spaces so multi-word queries fuzzy-match
// across the verb/target boundary.
func normalize(s string) string {
	return strings.ReplaceAll(strings.ToLower(s), " ", "")
}

// fuzzyScore reports whether query is an ordered subsequence of target and, if
// so, a score where LOWER is a tighter (better) match. The score is the span of
// the match (last matched index − first) plus its start offset, so compact
// early matches rank first.
func fuzzyScore(query, target string) (int, bool) {
	if query == "" {
		return 0, true
	}
	qi := 0
	first, last := -1, -1
	for ti := 0; ti < len(target) && qi < len(query); ti++ {
		if target[ti] == query[qi] {
			if first < 0 {
				first = ti
			}
			last = ti
			qi++
		}
	}
	if qi != len(query) {
		return 0, false
	}
	return (last - first) + first, true
}

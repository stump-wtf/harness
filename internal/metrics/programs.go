package metrics

// Per-Program Tool Calls
//
// agent-trace names the executables inside a shell tool call
// (classify.Event.Programs), so the observer can say which programs the fleet's
// agents actually run, and which of them keep failing — a stale `tea` login or
// a missing `make` target shows up as one series with a rising error count
// instead of a run nobody can explain (issue #913).
//
// The label is the only unbounded one harness exports, so it is bounded here,
// at the point a count is taken rather than at scrape time: a name that is not
// a plain lowercase executable name, and every name past the cap, are folded
// into program="other" before they reach the registry. Folding happens at
// count time for both reasons: the map that holds the counts is what would grow
// without bound, and a name that could carry an env assignment or a secret is
// then never stored anywhere a scrape could print it.
//
// Governing: SPEC-0013 REQ-3 (the family), REQ-5 (cardinality), REQ-6 (absence).
//
// @joestump-agent 10/31/2026 - Added for stump.wtf/harness#913.

import (
	"regexp"
	"strings"
)

const (
	// ProgramOther is the label for every name this file refuses to use as a
	// label of its own: one failing the grammar, and every name past the cap.
	ProgramOther = "other"
	// ProgramCap is the most distinct program names one harness label keeps.
	// The observer itself hands over at most 32 names per call, so the cap is
	// reached by distinct programs across a daemon's life, which is the growth
	// that matters: an hour of a build harness can name a hundred scripts.
	ProgramCap = 64
	// outcomeOK is a program call whose result was not an error; the other
	// outcome reuses outcomeError, which every *_total family here already
	// spells the same way.
	outcomeOK = "ok"
)

// programNameRe is the label grammar from issue #913: a lowercase executable
// name of at most 32 runes. agent-trace accepts uppercase too, so names are
// folded to lowercase before the match; anything still failing it is a name
// with something attached (an env assignment, an argument, a path, shell
// punctuation), and it is counted as other rather than recorded verbatim.
var programNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._+-]{0,31}$`)

// ProgramLabel maps one agent-trace program name to its label value.
func ProgramLabel(name string) string {
	n := strings.ToLower(name)
	if !programNameRe.MatchString(n) {
		return ProgramOther
	}
	return n
}

// countPrograms records one shell call's programs against this harness label.
// The slice is a lower bound (agent-trace names only what a static read of the
// command can name), so a call that names nothing simply counts nothing.
func (s *series) countPrograms(programs []string, isErr bool) {
	if len(programs) == 0 {
		return
	}
	if s.programs == nil {
		s.programs = make(map[string][2]uint64, len(programs))
	}
	out := 0
	if isErr {
		out = 1
	}
	for _, p := range programs {
		lbl := ProgramLabel(p)
		if lbl != ProgramOther && !s.takesProgram(lbl) {
			lbl = ProgramOther
		}
		c := s.programs[lbl]
		c[out]++
		s.programs[lbl] = c
	}
}

// takesProgram reports whether this harness label can keep a count for name:
// yes when it already has one, and otherwise only while the cap has room.
func (s *series) takesProgram(name string) bool {
	if _, ok := s.programs[name]; ok {
		return true
	}
	return s.namedPrograms() < ProgramCap
}

func (s *series) namedPrograms() int {
	n := 0
	for name := range s.programs {
		if name != ProgramOther {
			n++
		}
	}
	return n
}

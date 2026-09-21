package metrics

// Harness Label Cardinality
//
// `harness` is an operator-chosen label value. It is single digits per host in
// practice, but ephemeral scratchpad harnesses (ADR-0017) mint a fresh name
// per run, and "in practice" is not a bound. SPEC-0013 REQ-5 caps distinct
// values and folds the overflow into harness="__other__".
//
// A name keeps its label for as long as it stays declared, so a series never
// jumps between its own name and __other__ while the harness lives. A slot is
// released when the harness disappears from the Manager (removed from config,
// a project brought down, a scratchpad finished), which is what keeps a
// long-lived daemon that has run hundreds of scratchpads from starving every
// harness declared after the fiftieth. Released names lose their series, the
// same staleness any removed target shows; a harness that was overflowing takes
// the free slot the next time it is seen, starting its own series from zero
// while what it already contributed stays in __other__.
//
// A harness literally named "__other__" is never granted a slot of its own: it
// would be indistinguishable from the overflow, so it is folded into it.
//
// Governing: SPEC-0013 REQ-5.
//
// @joestump-agent 09/21/2026 - Added for harness#356.

// DefaultMaxHarnesses is SPEC-0013 REQ-5's default cap on distinct harness
// label values.
const DefaultMaxHarnesses = 50

// OverflowLabel is the harness label value every harness past the cap shares.
const OverflowLabel = "__other__"

// labeler assigns harness label values under the cap. Not safe for concurrent
// use; Metrics guards it with its mutex.
type labeler struct {
	max   int
	slots map[string]struct{} // names holding a label of their own
}

func newLabeler(max int) *labeler {
	return &labeler{max: max, slots: make(map[string]struct{})}
}

// label returns name's label value, granting it a slot if one is free.
func (l *labeler) label(name string) string {
	if _, ok := l.slots[name]; ok {
		return name
	}
	if name == OverflowLabel || len(l.slots) >= l.max {
		return OverflowLabel
	}
	l.slots[name] = struct{}{}
	return name
}

// retain releases the slot of every name not in declared and returns the
// released names, so the caller can drop their series.
func (l *labeler) retain(declared map[string]bool) []string {
	var gone []string
	for name := range l.slots {
		if !declared[name] {
			delete(l.slots, name)
			gone = append(gone, name)
		}
	}
	return gone
}

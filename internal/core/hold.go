package core

// Hold Reasons
//
// A harness the daemon keeps down on purpose (not a crash, not an operator
// stop) is held, and it is held for one or more reasons: its operating hours
// are closed, its provider's quota is exhausted (a park), or a budget is
// spent. A hold is a set of those reasons, not a flag, so a park that expires
// out of hours leaves the harness held for hours, and the harness starts again
// only when its last reason clears. The supervisor keeps the set; the harness
// projection carries it as hold_reasons, in canonical order.
//
// Governing: ADR-0027, SPEC-0021 REQ-14 "Release and hold reasons", REQ-16
// "Listing surfaces"; design.md § "Holds become a reason set". It amends
// ADR-0019 / SPEC-0012 REQ "Gate Enforcement", whose `held` is now "held for
// at least one reason".
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#468.

import "strings"

// HoldReason is one reason a harness is held down. The zero value is no
// reason and is ignored by every HoldSet method.
type HoldReason uint8

const (
	// HoldDrain: an operator took the harness out of service with
	// `harness drain`. Unlike the other reasons it stops nothing that is
	// already running: it is a gate on starting, checked first in
	// admission, and it is cleared only by `harness undrain`. It is
	// derived runtime state like the rest of the set, never persisted:
	// a daemon restart resumes every harness, which is what a packaging
	// script that drained-then-restarted wants. It amends stump.wtf/
	// harness#828.
	HoldDrain HoldReason = iota + 1
	// HoldHours: the harness is out of its operating hours (SPEC-0012).
	HoldHours
	// HoldQuota: the harness, or its quota group, is parked on an exhausted
	// provider quota until a reset instant (SPEC-0021 REQ-12).
	HoldQuota
	// HoldBudget: a daily budget the harness is subject to is spent
	// (SPEC-0021 REQ-5, REQ-10).
	HoldBudget
)

// holdOrder is the canonical order of the reasons, which is the order
// admission evaluates them in (SPEC-0021 REQ-4): hours, then quota, then
// budget. A reason another spec defines (SPEC-0020's model hold) takes its
// place in this list where REQ-4 evaluates it, between hours and quota, and
// needs no other change: the release rule only ever asks whether the set is
// empty.
var holdOrder = [...]HoldReason{HoldDrain, HoldHours, HoldQuota, HoldBudget}

// String is the reason's wire name, as hold_reasons carries it.
func (r HoldReason) String() string {
	switch r {
	case HoldDrain:
		return "drain"
	case HoldHours:
		return "hours"
	case HoldQuota:
		return "quota"
	case HoldBudget:
		return "budget"
	}
	return ""
}

// Valid reports whether r is a known reason.
func (r HoldReason) Valid() bool { return r.String() != "" }

// HoldSet is the set of reasons a harness is held for. The zero value is the
// empty set: not held.
type HoldSet uint16

// HoldSetOf returns the set holding rs. Unknown reasons are dropped.
func HoldSetOf(rs ...HoldReason) HoldSet {
	var s HoldSet
	for _, r := range rs {
		s = s.With(r)
	}
	return s
}

func (r HoldReason) bit() HoldSet {
	if !r.Valid() {
		return 0
	}
	return 1 << r
}

// Has reports whether s holds r.
func (s HoldSet) Has(r HoldReason) bool {
	b := r.bit()
	return b != 0 && s&b != 0
}

// With returns s plus r.
func (s HoldSet) With(r HoldReason) HoldSet { return s | r.bit() }

// Without returns s minus r.
func (s HoldSet) Without(r HoldReason) HoldSet { return s &^ r.bit() }

// Empty reports whether s holds no reason: the harness is not held.
func (s HoldSet) Empty() bool { return s == 0 }

// Reasons returns the reasons in s in canonical order; nil when empty.
func (s HoldSet) Reasons() []HoldReason {
	var out []HoldReason
	for _, r := range holdOrder {
		if s.Has(r) {
			out = append(out, r)
		}
	}
	return out
}

// Strings returns the wire names of the reasons in s, in canonical order:
// the projection's hold_reasons. nil when empty, so an omitempty field is
// absent for a harness that is not held.
func (s HoldSet) Strings() []string {
	var out []string
	for _, r := range s.Reasons() {
		out = append(out, r.String())
	}
	return out
}

// String renders s for a log line: "hours,quota", or "none" when empty.
func (s HoldSet) String() string {
	if s.Empty() {
		return "none"
	}
	return strings.Join(s.Strings(), ",")
}

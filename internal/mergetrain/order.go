package mergetrain

// Train Order
//
// The order the train lands eligible pull requests in: earliest qualifying
// approval first, then lowest PR number. It is first-come, first-served on the
// only signal that means "a reviewer is done", and it cannot be gamed by the
// author (ADR-0032, option C1). Pure and deterministic: the result depends on
// the set of PRs alone, never on the order they were listed in.
//
// Governing: SPEC-0025 REQ-3.

import (
	"slices"
	"time"
)

// Order returns the eligible PRs in the order the train will merge them.
// It does not mutate prs.
func Order(prs []PullRequest) []PullRequest {
	type keyed struct {
		pr       PullRequest
		approved time.Time
	}
	ks := make([]keyed, 0, len(prs))
	for _, pr := range prs {
		if ok, _ := Eligible(pr); !ok {
			continue
		}
		first, _ := firstQualifyingApproval(pr)
		ks = append(ks, keyed{pr: pr, approved: first.SubmittedAt})
	}
	slices.SortFunc(ks, func(a, b keyed) int {
		if c := a.approved.Compare(b.approved); c != 0 {
			return c
		}
		return a.pr.Number - b.pr.Number
	})
	out := make([]PullRequest, len(ks))
	for i, k := range ks {
		out[i] = k.pr
	}
	return out
}

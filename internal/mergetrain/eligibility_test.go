package mergetrain

// Eligibility Tests
//
// Governing tests: SPEC-0025 REQ-2 — one case per rule, the reason order, and
// the three ways an approval can fail to count (stale head, self-approval,
// overridden by a later REQUEST_CHANGES on the same head).

import (
	"testing"
	"time"
)

const (
	head  = "1111111111111111111111111111111111111111"
	stale = "0000000000000000000000000000000000000000"
)

var t0 = time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)

// ready returns a PR that passes every rule; each case breaks one thing.
func ready() PullRequest {
	return PullRequest{
		Number:    7,
		Author:    "joestump",
		HeadSHA:   head,
		CIState:   "success",
		Mergeable: true,
		Reviews: []Review{
			{Author: "joestump-agent", State: StateApproved, CommitID: head, SubmittedAt: t0},
		},
	}
}

func TestEligible(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*PullRequest)
		ok     bool
		reason string
	}{
		{"every condition met", func(*PullRequest) {}, true, ""},
		{"draft", func(p *PullRequest) { p.Draft = true }, false, ReasonDraft},
		{"ci pending", func(p *PullRequest) { p.CIState = "pending" }, false, ReasonCINotGreen},
		{"ci failure", func(p *PullRequest) { p.CIState = "failure" }, false, ReasonCINotGreen},
		{"ci empty", func(p *PullRequest) { p.CIState = "" }, false, ReasonCINotGreen},
		{"not mergeable", func(p *PullRequest) { p.Mergeable = false }, false, ReasonNotMergeable},
		{"no reviews", func(p *PullRequest) { p.Reviews = nil }, false, ReasonNoApproval},
		{"approval on a stale sha", func(p *PullRequest) { p.Reviews[0].CommitID = stale }, false, ReasonNoApproval},
		{"self-approval only", func(p *PullRequest) { p.Reviews[0].Author = p.Author }, false, ReasonNoApproval},
		{"comment is not an approval", func(p *PullRequest) { p.Reviews[0].State = "COMMENT" }, false, ReasonNoApproval},
		{
			"approved, then changes requested on the same sha",
			func(p *PullRequest) {
				p.Reviews = append(p.Reviews, Review{Author: "reviewer2", State: StateRequestChanges, CommitID: head, SubmittedAt: t0.Add(time.Minute)})
			},
			false, ReasonChangesRequested,
		},
		{
			"changes requested on a stale sha do not block",
			func(p *PullRequest) {
				p.Reviews = append(p.Reviews, Review{Author: "reviewer2", State: StateRequestChanges, CommitID: stale, SubmittedAt: t0.Add(-time.Hour)})
			},
			true, "",
		},
		{
			"first failing rule wins: draft before ci",
			func(p *PullRequest) { p.Draft, p.CIState, p.Mergeable, p.Reviews = true, "failure", false, nil },
			false, ReasonDraft,
		},
		{
			"first failing rule wins: ci before mergeable",
			func(p *PullRequest) { p.CIState, p.Mergeable, p.Reviews = "failure", false, nil },
			false, ReasonCINotGreen,
		},
		{
			"first failing rule wins: mergeable before approval",
			func(p *PullRequest) { p.Mergeable, p.Reviews = false, nil },
			false, ReasonNotMergeable,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := ready()
			tc.mutate(&pr)
			ok, reason := Eligible(pr)
			if ok != tc.ok || reason != tc.reason {
				t.Fatalf("Eligible = (%v, %q), want (%v, %q)", ok, reason, tc.ok, tc.reason)
			}
		})
	}
}

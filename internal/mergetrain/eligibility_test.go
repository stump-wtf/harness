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

// yes and no are what a forge reports about an approval's officiality; a nil
// Official, which the fixtures leave by default, means the forge did not say.
var (
	yes = true
	no  = false
)

// ready returns a PR that passes every rule; each case breaks one thing.
func ready() PullRequest {
	return PullRequest{
		Number:    7,
		Author:    "joestump",
		HeadSHA:   head,
		BaseRef:   "main",
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
		{"stacked on another branch", func(p *PullRequest) { p.BaseRef = "feat/owner-columns" }, false, wrongBaseReason("main")},
		{"base unknown", func(p *PullRequest) { p.BaseRef = "" }, false, wrongBaseReason("main")},
		{"draft", func(p *PullRequest) { p.Draft = true }, false, ReasonDraft},
		{"ci pending", func(p *PullRequest) { p.CIState = "pending" }, false, ReasonCINotGreen},
		{"ci failure", func(p *PullRequest) { p.CIState = "failure" }, false, ReasonCINotGreen},
		{"ci empty", func(p *PullRequest) { p.CIState = "" }, false, ReasonCINotGreen},
		{"not mergeable", func(p *PullRequest) { p.Mergeable = false }, false, ReasonNotMergeable},
		{"no reviews", func(p *PullRequest) { p.Reviews = nil }, false, ReasonNoApproval},
		{"approval on a stale sha", func(p *PullRequest) { p.Reviews[0].CommitID = stale }, false, ReasonNoApproval},
		{"self-approval only", func(p *PullRequest) { p.Reviews[0].Author = p.Author }, false, ReasonNoApproval},
		{"comment is not an approval", func(p *PullRequest) { p.Reviews[0].State = "COMMENT" }, false, ReasonNoApproval},
		{"approval the forge no longer counts", func(p *PullRequest) { p.Reviews[0].Official = &no }, false, ReasonNoApproval},
		{"approval reported official", func(p *PullRequest) { p.Reviews[0].Official = &yes }, true, ""},
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
			"first failing rule wins: base before draft",
			func(p *PullRequest) { p.BaseRef, p.Draft = "feat/x", true },
			false, wrongBaseReason("main"),
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
			ok, reason := Eligible(pr, "main")
			if ok != tc.ok || reason != tc.reason {
				t.Fatalf("Eligible = (%v, %q), want (%v, %q)", ok, reason, tc.ok, tc.reason)
			}
		})
	}
}

// An approval a review re-request left behind is the one ineligibility the
// reason alone cannot explain: the review is on the head, by the right person,
// and counts for nothing (#977).
func TestUnofficialApproval(t *testing.T) {
	cases := []struct {
		name   string
		review Review
		want   bool
	}{
		{"de-officialized on the head", Review{Author: "joestump-agent", State: StateApproved, CommitID: head, Official: &no}, true},
		{"official", Review{Author: "joestump-agent", State: StateApproved, CommitID: head, Official: &yes}, false},
		{"officiality unreported", Review{Author: "joestump-agent", State: StateApproved, CommitID: head}, false},
		{"by the author", Review{Author: "joestump", State: StateApproved, CommitID: head, Official: &no}, false},
		{"on a stale head", Review{Author: "joestump-agent", State: StateApproved, CommitID: stale, Official: &no}, false},
		{"a comment, not an approval", Review{Author: "joestump-agent", State: "COMMENT", CommitID: head, Official: &no}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := ready()
			pr.Reviews = []Review{tc.review}
			got, ok := UnofficialApproval(pr)
			if ok != tc.want {
				t.Fatalf("UnofficialApproval ok = %v, want %v (%+v)", ok, tc.want, got)
			}
			if ok && got != tc.review {
				t.Fatalf("UnofficialApproval = %+v, want %+v", got, tc.review)
			}
		})
	}
}

// Two unofficial approvals on one head: the note a driver logs names the one a
// reviewer should act on first.
func TestUnofficialApprovalIsTheEarliest(t *testing.T) {
	pr := ready()
	pr.Reviews = []Review{
		{Author: "late", State: StateApproved, CommitID: head, Official: &no, SubmittedAt: t0.Add(time.Hour)},
		{Author: "early", State: StateApproved, CommitID: head, Official: &no, SubmittedAt: t0},
	}
	got, ok := UnofficialApproval(pr)
	if !ok || got.Author != "early" {
		t.Fatalf("UnofficialApproval = %+v (%v), want the earliest", got, ok)
	}
}

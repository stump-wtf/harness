package mergetrain

// Order Tests
//
// Governing tests: SPEC-0025 REQ-3 — ineligible PRs dropped, earlier approval
// first, PR number breaks ties, input order never matters, the argument is not
// mutated, and empty in gives empty (non-nil) out.

import (
	"math/rand/v2"
	"reflect"
	"slices"
	"testing"
	"time"
)

// approvedAt returns an eligible PR whose only qualifying approval is at t.
func approvedAt(n int, t time.Time) PullRequest {
	pr := ready()
	pr.Number = n
	pr.Reviews = []Review{{Author: "joestump-agent", State: StateApproved, CommitID: pr.HeadSHA, SubmittedAt: t}}
	return pr
}

func numbers(prs []PullRequest) []int {
	out := make([]int, len(prs))
	for i, pr := range prs {
		out[i] = pr.Number
	}
	return out
}

func TestOrderDropsIneligible(t *testing.T) {
	draft := approvedAt(1, t0)
	draft.Draft = true
	red := approvedAt(2, t0)
	red.CIState = "failure"
	got := Order([]PullRequest{draft, approvedAt(3, t0), red})
	if !slices.Equal(numbers(got), []int{3}) {
		t.Fatalf("Order = %v, want [3]", numbers(got))
	}
}

func TestOrderEarlierApprovalFirst(t *testing.T) {
	got := Order([]PullRequest{approvedAt(1, t0.Add(time.Minute)), approvedAt(2, t0)})
	if !slices.Equal(numbers(got), []int{2, 1}) {
		t.Fatalf("Order = %v, want [2 1]", numbers(got))
	}
}

func TestOrderTieBreaksOnNumber(t *testing.T) {
	got := Order([]PullRequest{approvedAt(9, t0), approvedAt(4, t0)})
	if !slices.Equal(numbers(got), []int{4, 9}) {
		t.Fatalf("Order = %v, want [4 9]", numbers(got))
	}
}

func TestOrderUsesEarliestQualifyingApproval(t *testing.T) {
	// #1's earliest review is a stale-head approval, which must not count; its
	// qualifying approval is later than #2's.
	one := approvedAt(1, t0.Add(2*time.Minute))
	one.Reviews = append(one.Reviews, Review{Author: "x", State: StateApproved, CommitID: stale, SubmittedAt: t0.Add(-time.Hour)})
	// #3 has two qualifying approvals; the earlier one is its key.
	three := approvedAt(3, t0.Add(3*time.Minute))
	three.Reviews = append(three.Reviews, Review{Author: "y", State: StateApproved, CommitID: three.HeadSHA, SubmittedAt: t0.Add(-time.Minute)})
	got := Order([]PullRequest{one, approvedAt(2, t0), three})
	if !slices.Equal(numbers(got), []int{3, 2, 1}) {
		t.Fatalf("Order = %v, want [3 2 1]", numbers(got))
	}
}

func TestOrderIgnoresInputOrder(t *testing.T) {
	in := []PullRequest{
		approvedAt(5, t0), approvedAt(3, t0), approvedAt(8, t0.Add(time.Second)),
		approvedAt(1, t0.Add(time.Hour)), approvedAt(2, t0.Add(-time.Hour)), approvedAt(7, t0),
	}
	want := numbers(Order(in))
	rng := rand.New(rand.NewPCG(1, 2))
	for i := range 10 {
		shuffled := slices.Clone(in)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		if got := numbers(Order(shuffled)); !slices.Equal(got, want) {
			t.Fatalf("shuffle %d: Order = %v, want %v", i, got, want)
		}
	}
	if !slices.Equal(want, []int{2, 3, 5, 7, 8, 1}) {
		t.Fatalf("Order = %v, want [2 3 5 7 8 1]", want)
	}
}

func TestOrderDoesNotMutate(t *testing.T) {
	in := []PullRequest{approvedAt(2, t0.Add(time.Minute)), approvedAt(1, t0)}
	before := make([]PullRequest, len(in))
	for i, pr := range in {
		before[i] = pr
		before[i].Reviews = slices.Clone(pr.Reviews)
	}
	_ = Order(in)
	if !reflect.DeepEqual(in, before) {
		t.Fatalf("Order mutated its argument:\n got %+v\nwant %+v", in, before)
	}
}

func TestOrderEmpty(t *testing.T) {
	for _, in := range [][]PullRequest{nil, {}} {
		got := Order(in)
		if got == nil || len(got) != 0 {
			t.Fatalf("Order(%v) = %#v, want empty non-nil", in, got)
		}
	}
}

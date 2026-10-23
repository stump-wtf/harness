package forge

// Pull Request Types
//
// What a forge reports about an open pull request, reduced to what the merge
// train needs to decide eligibility and order. The structs live here rather
// than in internal/mergetrain because mergetrain calls the Forge interface,
// which returns them; declaring them there would make the two packages import
// each other. internal/mergetrain re-exports them as aliases, so callers still
// write mergetrain.PullRequest.
//
// Governing: ADR-0032 (merge train), SPEC-0025 design.md "Package layout".

import "time"

// Review is one review left on a pull request.
type Review struct {
	Author      string
	State       string // "APPROVED", "REQUEST_CHANGES", "COMMENT"
	CommitID    string // head SHA the review was left on
	SubmittedAt time.Time
	// Official reports whether the forge counts this review toward the
	// approvals its merge gate requires. nil means the forge did not say,
	// which is not evidence either way and so never blocks a merge: a review
	// the forge reports as not official cannot satisfy an approval
	// requirement, and Gitea clears that flag when review is requested from
	// the reviewer who left it (ADR-0050).
	Official *bool
}

// PullRequest is an open pull request as the train sees it.
type PullRequest struct {
	Number    int
	Author    string
	HeadSHA   string
	BaseRef   string // the branch this PR targets; "" when the forge does not say
	Reviews   []Review
	CIState   string // "success", "pending", "failure", "error"
	Draft     bool
	Mergeable bool
}

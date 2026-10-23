package mergetrain

// Eligibility
//
// Whether a pull request may enter the train. Pure: it reads only the
// PullRequest it is given and the train's base branch. The rules are evaluated
// in a fixed order and the first failure is the reason, so the same PR always
// logs the same reason.
//
// Governing: SPEC-0025 REQ-2.

// Ineligibility reasons, exactly as they appear in log lines.
const (
	ReasonDraft            = "draft"
	ReasonCINotGreen       = "ci not green"
	ReasonNotMergeable     = "not mergeable"
	ReasonNoApproval       = "no approval on current head"
	ReasonChangesRequested = "changes requested"
)

// wrongBaseReason is the ineligibility reason for a PR whose base branch is
// not the train's base branch — a PR stacked on another PR's branch (#870).
// It names the train's branch so the log line reads whole.
func wrongBaseReason(base string) string { return "base is not " + base }

// Eligible reports whether pr may enter the train whose base branch is base,
// and why not when it may not. reason is a short lowercase phrase for a log
// line, and is "" when ok is true.
func Eligible(pr PullRequest, base string) (ok bool, reason string) {
	switch {
	case pr.BaseRef != base:
		// A PR stacked on another branch is merged by the forge into that
		// branch, never into base; the train would then fail to verify and
		// halt. A PR the forge's payload leaves without a base never enters
		// either — the train does not guess (#870).
		return false, wrongBaseReason(base)
	case pr.Draft:
		return false, ReasonDraft
	case pr.CIState != CISuccess:
		return false, ReasonCINotGreen
	case !pr.Mergeable:
		return false, ReasonNotMergeable
	}
	if _, approved := firstQualifyingApproval(pr); !approved {
		return false, ReasonNoApproval
	}
	for _, r := range pr.Reviews {
		if r.State == StateRequestChanges && r.CommitID == pr.HeadSHA {
			return false, ReasonChangesRequested
		}
	}
	return true, ""
}

// qualifies reports whether r is a qualifying approval of pr: APPROVED, by
// someone other than the author, on the current head, and not one the forge
// reports as counting toward nothing. An approval whose officiality the forge
// never reported still qualifies: silence is not evidence, and the 405
// approvals refusal is the backstop for it (ADR-0050).
func qualifies(pr PullRequest, r Review) bool {
	return r.State == StateApproved && r.Author != pr.Author && r.CommitID == pr.HeadSHA &&
		(r.Official == nil || *r.Official)
}

// UnofficialApproval returns the earliest approval of pr that is left on its
// current head by someone other than the author but which the forge reports as
// not counting, and false when the PR holds no such approval. It is the state
// a review re-request leaves behind (Gitea clears the official flag of the
// requested reviewer's earlier reviews), and it is worth naming in a log line
// because its cure is a fresh approval rather than another verification
// (#977).
func UnofficialApproval(pr PullRequest) (Review, bool) {
	var first Review
	found := false
	for _, r := range pr.Reviews {
		if r.State != StateApproved || r.Author == pr.Author || r.CommitID != pr.HeadSHA {
			continue
		}
		if r.Official != nil && !*r.Official && (!found || r.SubmittedAt.Before(first.SubmittedAt)) {
			first, found = r, true
		}
	}
	return first, found
}

// firstQualifyingApproval returns the earliest qualifying approval of pr, by
// SubmittedAt, and false when there is none.
func firstQualifyingApproval(pr PullRequest) (Review, bool) {
	var first Review
	found := false
	for _, r := range pr.Reviews {
		if !qualifies(pr, r) {
			continue
		}
		if !found || r.SubmittedAt.Before(first.SubmittedAt) {
			first, found = r, true
		}
	}
	return first, found
}

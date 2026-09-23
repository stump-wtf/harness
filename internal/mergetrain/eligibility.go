package mergetrain

// Eligibility
//
// Whether a pull request may enter the train. Pure: it reads only the
// PullRequest it is given. The rules are evaluated in a fixed order and the
// first failure is the reason, so the same PR always logs the same reason.
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

// Eligible reports whether pr may enter the train, and why not when it may not.
// reason is a short lowercase phrase for a log line, and is "" when ok is true.
func Eligible(pr PullRequest) (ok bool, reason string) {
	switch {
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
// someone other than the author, on the current head.
func qualifies(pr PullRequest, r Review) bool {
	return r.State == StateApproved && r.Author != pr.Author && r.CommitID == pr.HeadSHA
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

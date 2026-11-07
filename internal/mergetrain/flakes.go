package mergetrain

// Classifying A Red Train
//
// A red train normally means the tree is bad: the PR's own CI was green, so
// what broke is either the interaction with what landed since, or the runner.
// The two are told apart by the forge's per-job detail (SPEC-0025 REQ-18,
// ADR-0056): a failed job whose log the forge answers "job not started" never
// reached a runner, so the tree is not on trial. The rule is deliberately
// narrow, because a wrong verdict costs a re-run of CI on a genuinely broken
// tree — and, unlike the stall it replaces, that is cheap: one retry, then
// today's behaviour.
//
// Every failed job on the commit has to carry that signature for the rule to
// fire. A job with a log, a job whose conclusion is anything but `failure`
// (`cancelled`, `timed_out`, an empty one), a commit the forge reports no jobs
// for, and a forge with no per-job detail at all all answer "this is a real
// failure", which is the answer that keeps the train's behaviour unchanged.
//
// Governing: SPEC-0025 REQ-18, ADR-0056; issue #1016.

import (
	"context"
	"strings"

	"github.com/stump-wtf/harness/internal/forge"
)

// ruleNeverStarted names the one rule that can call a train's failure
// infrastructure: every failed job on the commit has no log at all.
const ruleNeverStarted = "never-started"

// infraFailure returns the rule that classified this red attempt as not the
// tree's fault, or "" when the failure stands. It is asked once per red
// attempt, and only when the retry is configured on.
func infraFailure(ctx context.Context, f forge.Forge, repo, sha string) string {
	if sha == "" {
		return ""
	}
	r, ok := f.(forge.JobReporter)
	if !ok {
		return ""
	}
	jobs, err := r.CommitJobs(ctx, repo, sha)
	if err != nil || len(jobs) == 0 {
		return ""
	}
	failed := 0
	for _, j := range jobs {
		if !jobFailed(j.Conclusion) {
			continue
		}
		failed++
		if !j.NeverStarted {
			return ""
		}
	}
	if failed == 0 {
		return ""
	}
	return ruleNeverStarted
}

// jobFailed says whether a job's own conclusion counts as a failure to
// classify. Only `failure` does: a cancelled or timed-out job says nothing
// about runners, and an unknown word is read as a real failure.
func jobFailed(conclusion string) bool {
	return strings.EqualFold(conclusion, "failure")
}

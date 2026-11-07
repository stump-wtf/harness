package forge

import "context"

// Per-Job CI Detail
//
// CombinedStatus answers one question about a commit — success, pending,
// failure, error — and a failure there says nothing about whether the tree
// caused it. A job that never reached a runner fails the same way a failing
// test does. JobReporter is the optional capability that tells them apart:
// it is optional because not every forge can be asked, and everything built
// on it has to treat a failure as the tree's when it cannot.
//
// Governing: SPEC-0025 REQ-18, ADR-0056.

// Job is one CI job the forge reports for a commit.
type Job struct {
	// Run is the name of the workflow run the job belongs to.
	Run string
	// Name is the job's name within that run.
	Name string
	// Conclusion is the job's own verdict, as the forge words it: "success",
	// "failure", "skipped", "cancelled", or empty while it is still running.
	Conclusion string
	// NeverStarted is true when the forge has no log for the job at all: no
	// runner ever attached to it, so nothing the tested tree did can explain
	// the failure. Its zero value is a job that ran, so a forge that does not
	// fill the field is read as reporting a real failure.
	NeverStarted bool
}

// JobReporter is the optional forge capability that reports how each CI job on
// a commit ended. A forge that does not implement it can say only the combined
// state.
type JobReporter interface {
	CommitJobs(ctx context.Context, repo, sha string) ([]Job, error)
}

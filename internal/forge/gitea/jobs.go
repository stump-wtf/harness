package gitea

// Per-Job CI Detail
//
// CommitJobs answers forge.JobReporter: for one commit, every job of every
// Actions run on it, and for each failed job whether Gitea has a log for it at
// all. That last question is the whole point. A job whose runner never
// attaches fails with the combined status reading exactly like a failing
// test, and its log endpoint answers "job not started" instead of a log — the
// only difference the API shows (issue #1016). Any other answer, including a
// fetch that fails for a reason nobody predicted, is read as a job that ran,
// so a train is never retried on a guess.
//
// The calls are bounded to one page each: a commit carries a handful of runs,
// a run a handful of jobs, and only the failed jobs' logs are probed.
//
// Governing: SPEC-0025 REQ-18, ADR-0056.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/stump-wtf/harness/internal/forge"
)

// notStartedMarker is the message Gitea answers a never-started job's log with.
const notStartedMarker = "job not started"

type apiActionRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	HeadSHA    string `json:"head_sha"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type apiActionRuns struct {
	TotalCount int            `json:"total_count"`
	Runs       []apiActionRun `json:"workflow_runs"`
}

type apiActionJob struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type apiActionJobs struct {
	TotalCount int            `json:"total_count"`
	Jobs       []apiActionJob `json:"jobs"`
}

// CommitJobs lists the jobs of every Actions run whose head is sha.
func (c *Client) CommitJobs(ctx context.Context, repo, sha string) ([]forge.Job, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	var runs apiActionRuns
	query := rp + "/actions/runs?per_page=50&head_sha=" + url.PathEscape(sha)
	if err := c.do(ctx, "CommitJobs", repo, http.MethodGet, query, nil, &runs); err != nil {
		return nil, err
	}
	var out []forge.Job
	for _, r := range runs.Runs {
		// The filter is the server's; confirm it, so a forge that ignores the
		// parameter cannot blame another commit's jobs on this one.
		if r.HeadSHA != "" && r.HeadSHA != sha {
			continue
		}
		var jobs apiActionJobs
		path := fmt.Sprintf("%s/actions/runs/%d/jobs?per_page=100", rp, r.ID)
		if err := c.do(ctx, "CommitJobs", repo, http.MethodGet, path, nil, &jobs); err != nil {
			return nil, err
		}
		for _, j := range jobs.Jobs {
			job := forge.Job{Run: r.Name, Name: j.Name, Conclusion: j.Conclusion}
			if job.Conclusion == "failure" {
				job.NeverStarted = c.jobNeverStarted(ctx, repo, rp, j.ID)
			}
			out = append(out, job)
		}
	}
	return out, nil
}

// jobNeverStarted reports whether the failed job's log is the "job not
// started" answer rather than a log. A log that exists, and a log that cannot
// be read for any other reason, both answer false.
func (c *Client) jobNeverStarted(ctx context.Context, repo, rp string, id int64) bool {
	path := rp + "/actions/jobs/" + strconv.FormatInt(id, 10) + "/logs"
	err := c.do(ctx, "CommitJobs", repo, http.MethodGet, path, nil, nil)
	var se *forge.StatusError
	if !errors.As(err, &se) {
		return false
	}
	return strings.Contains(strings.ToLower(se.Body), notStartedMarker)
}

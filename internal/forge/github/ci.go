package github

// The CI Gate
//
// CombinedStatus answers the train's one question about a commit — success,
// pending, failure, error — and on GitHub that answer is not one endpoint.
// Actions results are check runs, and the combined-status endpoint reports a
// commit with no commit statuses as success whether or not anything ran on it
// (issue #906). Reading only combined status would therefore call a commit that
// never reached a runner green, which is the one mistake a merge train may not
// make. So this reads both, and pending wins: a check run that is queued or in
// progress is pending, and a commit with neither check runs nor statuses is
// pending rather than green.
//
// CommitJobs is the same care one level down (SPEC-0025 REQ-18, ADR-0056): a
// job that never reached a runner is reported as never started, so the train
// can tell an infrastructure flake from a failing test.
//
// Governing: SPEC-0025 REQ-5, REQ-18; ADR-0056, ADR-0061.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/stump-wtf/harness/internal/forge"
)

type apiCheckRun struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

type apiCheckRuns struct {
	Total     int           `json:"total_count"`
	CheckRuns []apiCheckRun `json:"check_runs"`
}

type apiCheckRunJobs struct {
	Total int              `json:"total_count"`
	Jobs  []apiCheckRunJob `json:"jobs"`
}

type apiCheckRunJob struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	// Steps is empty for a job no runner ever attached to.
	Steps []struct {
		Name string `json:"name"`
	} `json:"steps"`
}

type apiCommitStatus struct {
	State string `json:"state"`
}

type apiCommitStatuses struct {
	State    string            `json:"state"`
	Statuses []apiCommitStatus `json:"statuses"`
}

// checkRunPages bounds how far the check-run listing is followed. A commit
// carries a handful of check runs; a forge that ignores the pagination
// parameters then cannot loop forever.
const checkRunPages = 4

// CombinedStatus returns the state of everything the forge reports about sha:
// its check runs and its commit statuses. "pending" covers a commit nothing has
// run on yet.
func (c *Client) CombinedStatus(ctx context.Context, repo, sha string) (string, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return "", err
	}
	runs, err := c.checkRuns(ctx, repo, rp, sha)
	if err != nil {
		return "", err
	}
	var st apiCommitStatuses
	if err := c.do(ctx, "CombinedStatus", repo, http.MethodGet,
		rp+"/commits/"+url.PathEscape(sha)+"/status", nil, &st); err != nil {
		return "", err
	}
	return combine(runs, st), nil
}

// combine folds check runs and commit statuses into one state. Anything still
// running is pending; anything refused is failure; a commit nothing ran on is
// pending, never success.
func combine(runs []apiCheckRun, st apiCommitStatuses) string {
	sawAnything, settled, bad := false, true, false
	for _, r := range runs {
		sawAnything = true
		switch r.Status {
		case "queued", "requested", "waiting", "pending", "in_progress":
			return "pending"
		}
		switch r.Conclusion {
		case "failure", "timed_out", "action_required", "stale", "startup_failure", "cancelled":
			bad = true
		case "success", "neutral", "skipped":
		default:
			// Completed with a conclusion nobody predicted: not a verdict.
			settled = false
		}
	}
	if len(st.Statuses) > 0 {
		sawAnything = true
		for _, s := range st.Statuses {
			switch s.State {
			case "pending":
				return "pending"
			case "failure", "error":
				bad = true
			case "success":
			default:
				settled = false
			}
		}
	} else if st.State != "" && st.State != "success" {
		// A combined state with no per-context rows still says something.
		sawAnything = true
		switch st.State {
		case "pending":
			return "pending"
		case "failure", "error":
			bad = true
		}
	}
	switch {
	case !sawAnything:
		return "pending"
	case bad:
		return "failure"
	case !settled:
		return "pending"
	default:
		return "success"
	}
}

func (c *Client) checkRuns(ctx context.Context, repo, rp, sha string) ([]apiCheckRun, error) {
	var out []apiCheckRun
	for page := 1; page <= checkRunPages; page++ {
		var batch apiCheckRuns
		q := "?per_page=" + strconv.Itoa(perPage) + "&page=" + strconv.Itoa(page) + "&check_status=all"
		if err := c.do(ctx, "CombinedStatus", repo, http.MethodGet,
			rp+"/commits/"+url.PathEscape(sha)+"/check-runs"+q, nil, &batch); err != nil {
			return nil, err
		}
		out = append(out, batch.CheckRuns...)
		if len(batch.CheckRuns) < perPage {
			return out, nil
		}
	}
	return out, nil
}

// CommitJobs lists every job of every check run on sha, and for each failed one
// whether a runner ever picked it up. GitHub reports a job that never started as
// a check run whose job list is empty: no runner attached, so no job object was
// ever created. That empty list is what NeverStarted means here, and anything
// else — including a job list that cannot be read — is read as a job that ran,
// so a train is never retried on a guess.
func (c *Client) CommitJobs(ctx context.Context, repo, sha string) ([]forge.Job, error) {
	rp, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	runs, err := c.checkRuns(ctx, repo, rp, sha)
	if err != nil {
		return nil, err
	}
	var out []forge.Job
	for _, r := range runs {
		if r.Conclusion != "failure" {
			out = append(out, forge.Job{Run: r.Name, Name: r.Name, Conclusion: r.Conclusion})
			continue
		}
		jobs, err := c.checkRunJobs(ctx, repo, rp, r.ID)
		if err != nil {
			continue // unreadable: read it as a job that ran
		}
		if len(jobs) == 0 {
			out = append(out, forge.Job{Run: r.Name, Name: r.Name, Conclusion: r.Conclusion, NeverStarted: true})
			continue
		}
		for _, j := range jobs {
			out = append(out, forge.Job{Run: r.Name, Name: j.Name, Conclusion: j.Conclusion,
				NeverStarted: j.Conclusion == "failure" && len(j.Steps) == 0})
		}
	}
	return out, nil
}

func (c *Client) checkRunJobs(ctx context.Context, repo, rp string, id int64) ([]apiCheckRunJob, error) {
	var out apiCheckRunJobs
	path := fmt.Sprintf("%s/check-runs/%d/jobs?per_page=%d", rp, id, perPage)
	if err := c.do(ctx, "CommitJobs", repo, http.MethodGet, path, nil, &out); err != nil {
		var se *forge.StatusError
		if errors.As(err, &se) && se.Status == http.StatusNotFound {
			return nil, nil
		}
		return nil, err
	}
	return out.Jobs, nil
}

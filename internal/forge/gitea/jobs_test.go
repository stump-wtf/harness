package gitea

// Per-Job CI Detail Tests
//
// Governing tests: SPEC-0025 REQ-18, ADR-0056 — the runner signature comes from
// the job's own log endpoint: an error whose body says "job not started" is a
// job no runner ever attached to, a served log (or a log that cannot be read
// for any other reason) is a job that ran. A run whose head is not the asked-for
// commit is never blamed on it.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/stump-wtf/harness/internal/forge"
)

const trainSHA = "1111111111111111111111111111111111111111"

// jobsFixture answers job 1's log with `status` and `body`: the never-started
// answer is an error whose body says so, and any other failure is just the log
// being unreadable.
func jobsFixture(t *testing.T, status int, body any) (*server, *Client) {
	t.Helper()
	s, c := newServer(t)
	base := "/api/v1/repos/" + repo
	s.route("GET "+base+"/actions/runs", 200, map[string]any{"total_count": 2, "workflow_runs": []any{
		map[string]any{"id": 20255, "name": "ci", "head_sha": trainSHA, "status": "completed", "conclusion": "failure"},
		map[string]any{"id": 20256, "name": "ci", "head_sha": "2222222222222222222222222222222222222222", "status": "completed", "conclusion": "failure"},
	}})
	s.route("GET "+base+"/actions/runs/20255/jobs", 200, map[string]any{"total_count": 3, "jobs": []any{
		map[string]any{"id": 1, "name": "secrets", "status": "completed", "conclusion": "failure"},
		map[string]any{"id": 2, "name": "lint + test", "status": "completed", "conclusion": "failure"},
		map[string]any{"id": 3, "name": "pages", "status": "completed", "conclusion": "skipped"},
	}})
	s.route("GET "+base+"/actions/jobs/1/logs", status, body)
	s.route("GET "+base+"/actions/jobs/2/logs", http.StatusOK, "=== RUN TestThing\n--- FAIL: TestThing\n")
	s.route("GET "+base+"/actions/jobs/3/logs", http.StatusNotFound, map[string]any{"message": "not found"})
	return s, c
}

func TestCommitJobsReadsTheRunnerSignature(t *testing.T) {
	s, c := jobsFixture(t, http.StatusBadRequest, map[string]any{"message": "job not started"})
	jobs, err := c.CommitJobs(ctx, repo, trainSHA)
	if err != nil {
		t.Fatal(err)
	}
	want := []forge.Job{
		{Run: "ci", Name: "secrets", Conclusion: "failure", NeverStarted: true},
		{Run: "ci", Name: "lint + test", Conclusion: "failure"},
		{Run: "ci", Name: "pages", Conclusion: "skipped"},
	}
	if len(jobs) != len(want) {
		t.Fatalf("jobs = %+v, want %d", jobs, len(want))
	}
	for i, w := range want {
		if jobs[i] != w {
			t.Errorf("job %d = %+v, want %+v", i, jobs[i], w)
		}
	}
	// A success-job's log is never fetched, and only this commit's run is read.
	for _, r := range s.requests() {
		if r.Path == "/api/v1/repos/"+repo+"/actions/runs/20256/jobs" {
			t.Error("read the jobs of a run on a different commit")
		}
		if r.Auth != "token "+token {
			t.Errorf("%s %s: auth = %q", r.Method, r.Path, r.Auth)
		}
	}
}

// A log endpoint that fails for a reason nobody predicted is not evidence about
// runners: the job is read as one that ran, so the train keeps its red verdict.
func TestCommitJobsTreatsAnUnreadableLogAsAJobThatRan(t *testing.T) {
	s, c := jobsFixture(t, http.StatusInternalServerError, map[string]any{"message": "server error"})
	jobs, err := c.CommitJobs(ctx, repo, trainSHA)
	if err != nil {
		t.Fatal(err)
	}
	if jobs[0].NeverStarted {
		t.Errorf("a 500 from the log endpoint read as no runner: %+v", jobs[0])
	}
	if s.requests()[0].Method != http.MethodGet {
		t.Fatalf("first call = %+v, want a GET", s.requests()[0])
	}
}

func TestCommitJobsRunsQueryIsScopedToTheCommit(t *testing.T) {
	s, c := jobsFixture(t, http.StatusOK, "=== RUN TestThing\n")
	if _, err := c.CommitJobs(ctx, repo, trainSHA); err != nil {
		t.Fatal(err)
	}
	first := s.requests()[0]
	if first.Query == "" {
		t.Fatal("the runs listing carried no head_sha filter")
	}
	if !strings.Contains(first.Query, "head_sha="+trainSHA) {
		t.Errorf("query = %q, want it filtered to the commit", first.Query)
	}
}

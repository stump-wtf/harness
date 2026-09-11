package main

// Governing: SPEC-0008 REQ "Protocol Operations", REQ "Manual Trigger"; issue
// #120.

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

func intp(n int) *int { return &n }

// TestWaitExitCode pins what `trigger --wait` exits with for each outcome.
func TestWaitExitCode(t *testing.T) {
	cases := []struct {
		name string
		run  protocol.RunInfo
		want int
	}{
		{"success", protocol.RunInfo{Outcome: "success", ExitCode: intp(0)}, 0},
		{"failed with its own code", protocol.RunInfo{Outcome: "failed", ExitCode: intp(3)}, 3},
		{"spawn failure has no code", protocol.RunInfo{Outcome: "failed"}, 1},
		{"signalled", protocol.RunInfo{Outcome: "failed", ExitCode: intp(-1)}, 1},
		{"timed out", protocol.RunInfo{Outcome: "timed_out", ExitCode: intp(-1)}, exitTimedOut},
		{"skipped", protocol.RunInfo{Outcome: "skipped"}, exitSkipped},
		{"cancelled", protocol.RunInfo{Outcome: "cancelled", ExitCode: intp(-1)}, 1},
		{"interrupted", protocol.RunInfo{Outcome: "interrupted"}, 1},
		{"out of range code", protocol.RunInfo{Outcome: "failed", ExitCode: intp(300)}, 1},
	}
	for _, tc := range cases {
		if got := waitExitCode(tc.run); got != tc.want {
			t.Errorf("%s: waitExitCode = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestPrintJobsTable renders a job's cadence, next window and latest run.
func TestPrintJobsTable(t *testing.T) {
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	jobs := []protocol.JobInfo{
		{
			Name: "nightly", Schedule: "CRON_TZ=UTC 0 3 * * *", State: "stopped",
			NextRun: now.Add(15 * time.Hour).Format(time.RFC3339),
			LastRun: &protocol.RunInfo{RunID: 7, Outcome: "failed", StartedAt: now.Add(-9 * time.Hour).Format(time.RFC3339Nano),
				EndedAt: now.Add(-8 * time.Hour).Format(time.RFC3339Nano)},
			ConsecutiveFailures: 2,
		},
		{
			Name: "busy", Schedule: "@every 6h", State: "running",
			Running: &protocol.RunInfo{RunID: 3, Outcome: "running"},
		},
	}
	var buf bytes.Buffer
	if err := printJobsTable(&buf, jobs, now); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	// A non-terminal writer gets the default table width, so a long cell may
	// wrap; assert on tokens rather than on whole cells.
	for _, want := range []string{"nightly", "daily 03:00 UTC", "in 15h", "#7 failed", "8h", "busy", "running #3", "every 6h"} {
		if !strings.Contains(out, want) {
			t.Errorf("jobs table missing %q:\n%s", want, out)
		}
	}

	buf.Reset()
	if err := printJobsTable(&buf, nil, now); err != nil || !strings.Contains(buf.String(), "no scheduled harnesses") {
		t.Errorf("empty jobs table = %q, %v", buf.String(), err)
	}
}

// TestPrintRunsTable renders one row per record, with process-less records
// marked as such.
func TestPrintRunsTable(t *testing.T) {
	start := time.Date(2026, 9, 11, 3, 0, 0, 0, time.UTC)
	rd := protocol.RunsData{Name: "nightly", Runs: []protocol.RunInfo{
		{RunID: 3, Trigger: "schedule", Outcome: "missed", Windows: 4, StartedAt: start.Format(time.RFC3339Nano), EndedAt: start.Format(time.RFC3339Nano)},
		{RunID: 2, Trigger: "manual", Outcome: "timed_out", StartedAt: start.Format(time.RFC3339Nano),
			EndedAt: start.Add(45 * time.Minute).Format(time.RFC3339Nano), DurationMs: (45 * time.Minute).Milliseconds(), ExitCode: intp(-1)},
		{RunID: 1, Trigger: "catch_up", Outcome: "interrupted", StartedAt: start.Format(time.RFC3339Nano)},
	}}
	var buf bytes.Buffer
	if err := printRunsTable(&buf, rd); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"missed ×4", "timed_out", "45m", "-1", "catch_up", "interrupted", "unknown"} {
		if !strings.Contains(out, want) {
			t.Errorf("runs table missing %q:\n%s", want, out)
		}
	}
}

// TestTriggerLine says what the daemon did with each decision.
func TestTriggerLine(t *testing.T) {
	cases := map[string]protocol.TriggerData{
		"nightly: started run #4":     {Name: "nightly", Decision: protocol.TriggerStarted, Run: &protocol.RunInfo{RunID: 4}},
		"queued behind it":            {Name: "nightly", Decision: protocol.TriggerQueued},
		"skipped, recorded as run #5": {Name: "nightly", Decision: protocol.TriggerSkipped, Run: &protocol.RunInfo{RunID: 5}},
	}
	for want, td := range cases {
		if got := triggerLine(td); !strings.Contains(got, want) {
			t.Errorf("triggerLine(%+v) = %q, want it to contain %q", td, got, want)
		}
	}
}

// TestPickRun finds the followed run, and for a queued trigger the first
// manual run that started after it was queued.
func TestPickRun(t *testing.T) {
	runs := []protocol.RunInfo{ // newest first
		{RunID: 9, Trigger: "manual", Outcome: "skipped"},
		{RunID: 8, Trigger: "manual", Outcome: "running"},
		{RunID: 7, Trigger: "schedule", Outcome: "success"},
		{RunID: 6, Trigger: "manual", Outcome: "success"},
	}
	if r, ok := pickRun(runs, 7, 0); !ok || r.RunID != 7 {
		t.Errorf("pickRun by id = %+v, %v", r, ok)
	}
	if r, ok := pickRun(runs, 0, 6); !ok || r.RunID != 8 {
		t.Errorf("pickRun queued = %+v, %v; want run 8", r, ok)
	}
	if _, ok := pickRun(runs, 0, 9); ok {
		t.Error("pickRun found a run that started before the trigger")
	}
	if _, ok := pickRun(runs, 42, 0); ok {
		t.Error("pickRun found a run id that is not in the history")
	}
}

// TestPrintNewLogText prints only what a poll added, and reprints a log that
// was rewritten.
func TestPrintNewLogText(t *testing.T) {
	var buf bytes.Buffer
	printed := printNewLogText(&buf, "", "line one\n")
	printed = printNewLogText(&buf, printed, "line one\nline two\n")
	if buf.String() != "line one\nline two\n" {
		t.Errorf("incremental output = %q", buf.String())
	}
	buf.Reset()
	printNewLogText(&buf, printed, "different\n")
	if buf.String() != "different\n" {
		t.Errorf("rewritten log output = %q", buf.String())
	}
}

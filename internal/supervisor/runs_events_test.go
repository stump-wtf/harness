package supervisor

// Governing: SPEC-0008 REQ "Lifecycle Events", REQ "Manual Trigger", REQ
// "Protocol Operations"; issue #120.

import (
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// TestStartRunReportsItsDecision: StartRun says whether it started, queued or
// skipped the request, with the record it made.
func TestStartRunReportsItsDecision(t *testing.T) {
	env := newRunsEnv(t)
	h := sweep("busy", "sleep 30")
	h.OnOverlap = core.OverlapQueue
	m, _ := env.manager(t, sweepCfg(h), fastPolicy())

	d, ok := m.StartRun("busy", RunRequest{Trigger: TriggerManual})
	if !ok || d.Kind != DecisionStarted || d.Run.RunID != 1 || d.Run.Outcome != OutcomeRunning {
		t.Errorf("first StartRun = %+v, %v; want started run 1", d, ok)
	}
	d, _ = m.StartRun("busy", RunRequest{Trigger: TriggerManual})
	if d.Kind != DecisionQueued || d.Run.RunID != 0 {
		t.Errorf("second StartRun = %+v; want queued with no record", d)
	}
	d, _ = m.StartRun("busy", RunRequest{Trigger: TriggerManual})
	if d.Kind != DecisionSkipped || d.Run.RunID != 2 || d.Run.Outcome != OutcomeSkipped {
		t.Errorf("third StartRun = %+v; want skipped record 2", d)
	}
	if _, ok := m.StartRun("ghost", RunRequest{Trigger: TriggerManual}); ok {
		t.Error("StartRun of an unknown harness succeeded")
	}
	m.Stop("busy")
}

// TestRunEventsArePublished: a run's start and finish, a missed-window record,
// and a next-window change all reach the lifecycle bus.
func TestRunEventsArePublished(t *testing.T) {
	env := newRunsEnv(t)
	m, _ := env.manager(t, sweepCfg(sweep("ok", "exit 0")), fastPolicy())
	events, cancel := m.Events()
	defer cancel()

	next := func(kind EventKind) Event {
		t.Helper()
		deadline := time.After(5 * time.Second)
		for {
			select {
			case ev := <-events:
				if ev.Kind == kind && ev.Name == "ok" {
					return ev
				}
			case <-deadline:
				t.Fatalf("no %s event within 5s", kind)
			}
		}
	}

	m.StartRun("ok", RunRequest{Trigger: TriggerSchedule})
	started := next(EventRunStarted)
	if started.Run.RunID != 1 || started.Run.Trigger != TriggerSchedule || started.Run.Outcome != OutcomeRunning {
		t.Errorf("started event run = %+v", started.Run)
	}
	finished := next(EventRunFinished)
	if finished.Run.RunID != 1 || finished.Run.Outcome != OutcomeSuccess ||
		finished.Run.ExitCode == nil || *finished.Run.ExitCode != 0 || finished.Run.EndedAt == nil {
		t.Errorf("finished event run = %+v", finished.Run)
	}

	window := time.Date(2026, 9, 11, 3, 0, 0, 0, time.UTC)
	if err := m.RecordMissed("ok", window, window, 1, window.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if missed := next(EventRunFinished); missed.Run.Outcome != OutcomeMissed || missed.Run.RunID != 2 {
		t.Errorf("missed event run = %+v", missed.Run)
	}

	m.PublishScheduleChanged("ok", window.Add(24*time.Hour))
	if changed := next(EventScheduleChanged); !changed.NextRun.Equal(window.Add(24 * time.Hour)) {
		t.Errorf("schedule changed next = %v", changed.NextRun)
	}
}

// TestConsecutiveFailures counts failures back to the latest success, and
// ignores records that pass no verdict on the job.
func TestConsecutiveFailures(t *testing.T) {
	r := func(outcomes ...RunOutcome) []RunRecord {
		out := make([]RunRecord, len(outcomes))
		for i, o := range outcomes {
			out[i] = RunRecord{RunID: i + 1, Outcome: o}
		}
		return out
	}
	cases := []struct {
		name string
		runs []RunRecord
		want int
	}{
		{"no history", nil, 0},
		{"last run succeeded", r(OutcomeFailed, OutcomeSuccess), 0},
		{"two failures after a success", r(OutcomeSuccess, OutcomeFailed, OutcomeTimedOut), 2},
		{"never succeeded", r(OutcomeFailed, OutcomeFailed, OutcomeFailed), 3},
		{"skips and misses do not reset", r(OutcomeFailed, OutcomeSkipped, OutcomeMissed, OutcomeFailed), 2},
		{"operator actions do not count", r(OutcomeSuccess, OutcomeCancelled, OutcomeReplaced, OutcomeInterrupted, OutcomeRunning), 0},
	}
	for _, tc := range cases {
		if got := ConsecutiveFailures(tc.runs); got != tc.want {
			t.Errorf("%s: ConsecutiveFailures = %d, want %d", tc.name, got, tc.want)
		}
	}
}

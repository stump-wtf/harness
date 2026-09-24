package supervisor

// Command One-Shot Tests
//
// A scheduled or triggered `command` harness renders its argv[1:] templates at
// spawn, against the run it is spawning for. These tests drive a real Manager
// and exec the argv probe (command_test.go), so every assertion is about what
// the child process actually received, what the run record actually says, and
// what actually landed on disk — never about what execArgv returned.
//
// Governing: ADR-0023, SPEC-0017 REQ-3 "Command Harness Modes And
// Exclusions", REQ-7 "Template Context", REQ-11 "Rendering", REQ "Error
// Handling Standards".

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
	_ "time/tzdata" // run.date zones must resolve on a CI image without zoneinfo

	"github.com/stump-wtf/harness/internal/core"
)

// commandSweep is a probe command harness fired by schedule.
func commandSweep(t *testing.T, name string, args ...string) (core.Harness, string) {
	t.Helper()
	h, out := probeHarness(t, name, testBinary(t), args...)
	h.Schedule = "0 6 * * *"
	h.Timeout = core.DefaultRunTimeout
	h.OnOverlap = core.OverlapSkip
	h.KeepRuns = core.DefaultKeepRuns
	return h, out
}

// commandTriggered is a probe command harness fired by an event source.
func commandTriggered(t *testing.T, name string, args ...string) (core.Harness, string) {
	t.Helper()
	h, out := probeHarness(t, name, testBinary(t), args...)
	h.Triggers = []string{"webhook.ci"}
	h.Timeout = core.DefaultRunTimeout
	h.OnOverlap = core.OverlapQueue
	h.KeepRuns = core.DefaultKeepRuns
	return h, out
}

// TestScheduledCommandRunRendersRunContext is REQ-3's "A scheduled script
// with no prompt" and REQ-7's "Run context on a scheduled run": the firing
// produces a run record, and the process receives the run's own id and
// trigger, one argument per element.
func TestScheduledCommandRunRendersRunContext(t *testing.T) {
	e := newRunsEnv(t)
	h, out := commandSweep(t, "report", "--run", "{{run.id}}", "--via", "{{run.trigger}}", "--name={{harness.name}}")
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	m.StartRun("report", RunRequest{Trigger: TriggerSchedule, Window: time.Now()})
	rec := waitRuns(t, m, "report", "the scheduled run finishes", outcomesAre(OutcomeSuccess))[0]

	p := readProbe(t, out)
	want := []string{"--run", strconv.Itoa(rec.RunID), "--via", "schedule", "--name=report"}
	if !slices.Equal(p.Args, want) {
		t.Errorf("child received %q, want %q", p.Args, want)
	}
}

// TestRenderContextRunDateUsesScheduleZone: run.date is the schedule's
// CRON_TZ zone's calendar day, and run.started_at is the record's start in
// UTC. 20:00 UTC is already tomorrow in Tokyo and still today in Los Angeles,
// so a renderer using any single zone gets one of the two wrong.
func TestRenderContextRunDateUsesScheduleZone(t *testing.T) {
	started := time.Date(2026, 9, 24, 20, 0, 0, 0, time.UTC)
	run := RunEnv{RunID: 7, Trigger: TriggerSchedule, StartedAt: started}
	for schedule, want := range map[string]string{
		"CRON_TZ=Asia/Tokyo 0 6 * * *":      "2026-09-25",
		"TZ=America/Los_Angeles 0 6 * * *":  "2026-09-24",
		"0 6 * * *":                         started.In(time.Local).Format(time.DateOnly),
		"":                                  started.In(time.Local).Format(time.DateOnly),
		"CRON_TZ=Not/AZone this is garbage": started.In(time.Local).Format(time.DateOnly),
	} {
		ctx := renderContext(core.Harness{Name: "r", Schedule: schedule}, "/w", run, time.Now())
		if got, _ := ctx.Lookup(core.PathRunDate); got != want {
			t.Errorf("schedule %q: run.date = %q, want %q", schedule, got, want)
		}
		if got, _ := ctx.Lookup(core.PathRunStartedAt); got != "2026-09-24T20:00:00Z" {
			t.Errorf("run.started_at = %q, want the record's start in UTC", got)
		}
	}
	// Absent, not empty: a start with no record has no run.id, run.trigger
	// or run.source, so a required reference fails instead of passing "".
	ctx := renderContext(core.Harness{Name: "r"}, "/w", RunEnv{}, started)
	for _, p := range []string{core.PathRunID, core.PathRunTrigger, core.PathRunSource, core.PathModel} {
		if v, ok := ctx.Lookup(p); ok {
			t.Errorf("%s present (%q) on a start with no record", p, v)
		}
	}
	if v, _ := ctx.Lookup(core.PathHarnessWorkdir); v != "/w" {
		t.Errorf("harness.workdir = %q", v)
	}
}

// TestCommandArgvSpacesDoNotSplit is REQ-11's "Spaces do not split" and
// "Empty optional value", end to end: a value holding spaces, quotes and a
// newline reaches the child as one argument, and an absent optional value as
// one empty argument.
func TestCommandArgvSpacesDoNotSplit(t *testing.T) {
	e := newRunsEnv(t)
	h, out := commandSweep(t, "tool", "--msg={{model}}", "{{run.source?}}")
	h.Model = "fix \"it\" now\nplease $(id)"
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())

	m.StartRun("tool", RunRequest{Trigger: TriggerSchedule})
	waitRuns(t, m, "tool", "the run finishes", outcomesAre(OutcomeSuccess))

	p := readProbe(t, out)
	if want := []string{"--msg=" + h.Model, ""}; !slices.Equal(p.Args, want) {
		t.Errorf("child received %d args %q, want exactly %d: %q", len(p.Args), p.Args, len(want), want)
	}
}

// TestCommandRunTemplateUnresolvedIsSkipped is REQ-11's unresolved-value rule
// and "A render failure is never silent": a run whose required value is absent
// is recorded skipped with reason template_unresolved and the path's name,
// nothing is exec'd, the harness is not failed, and the counter's event fires.
// The same harness then runs when the value is present, which is what makes
// the probe's absence meaningful.
func TestCommandRunTemplateUnresolvedIsSkipped(t *testing.T) {
	e := newRunsEnv(t)
	h, out := commandTriggered(t, "ci", "{{run.source}}")
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())
	events, cancel := m.Events()
	defer cancel()

	m.StartRun("ci", RunRequest{Trigger: TriggerManual})
	rec := waitRuns(t, m, "ci", "the unresolved run is recorded", outcomesAre(OutcomeSkipped))[0]
	if rec.Reason != ReasonTemplateUnresolved || rec.MissingPath != core.PathRunSource {
		t.Errorf("record reason/path = %q/%q, want template_unresolved/run.source", rec.Reason, rec.MissingPath)
	}
	if rec.ExitCode != nil || rec.EndedAt == nil {
		t.Errorf("record = %+v, want an ended record with no exit code (nothing ran)", rec)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the probe ran for an unresolved run (stat: %v)", err)
	}
	if log := readText(t, filepath.Join(e.logs, "ci.log")); !strings.Contains(log, "reason=template_unresolved path=run.source") {
		t.Errorf("harness log does not record the skip with its path:\n%s", log)
	}
	if snap, _ := m.Snapshot("ci"); snap.State != core.StateStopped {
		t.Errorf("state after a template skip = %s, want stopped (no process ran, nothing failed)", snap.State)
	}
	gotEvent := false
	deadline := time.After(2 * time.Second)
	for !gotEvent {
		select {
		case ev := <-events:
			if ev.Kind == EventTemplateRenderFailed {
				if ev.Name != "ci" || ev.RenderFailure != RenderFailureUnresolved {
					t.Errorf("render failure event = %+v", ev)
				}
				gotEvent = true
			}
		case <-deadline:
			t.Fatal("no EventTemplateRenderFailed: the failure would not be counted")
		}
	}

	m.StartRun("ci", RunRequest{Trigger: TriggerWebhook, Source: "webhook.ci"})
	waitRuns(t, m, "ci", "the resolved run succeeds", outcomesAre(OutcomeSkipped, OutcomeSuccess))
	if p := readProbe(t, out); !slices.Equal(p.Args, []string{"webhook.ci"}) {
		t.Errorf("child received %q, want [webhook.ci]", p.Args)
	}
}

// TestCommandPlainStartUnresolvedFails: a start with no run record whose
// required value is absent (reachable only by a definition that bypassed the
// load-time rule) fails the start, logs the path, counts, and execs nothing.
func TestCommandPlainStartUnresolvedFails(t *testing.T) {
	e := newRunsEnv(t)
	h, out := probeHarness(t, "resident", testBinary(t), "{{run.id}}")
	m, _ := e.manager(t, sweepCfg(h), fastPolicy())
	events, cancel := m.Events()
	defer cancel()

	m.Start("resident")
	waitFor(t, 5*time.Second, "the start fails", func() bool {
		s, _ := m.Snapshot("resident")
		return s.State == core.StateFailed
	})
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the probe ran (stat: %v)", err)
	}
	log := readText(t, filepath.Join(e.logs, "resident.log"))
	if !strings.Contains(log, "start failed reason=template_unresolved path=run.id") {
		t.Errorf("harness log does not record the failed start with its path:\n%s", log)
	}
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-events:
			if ev.Kind == EventTemplateRenderFailed && ev.RenderFailure == RenderFailureUnresolved {
				return
			}
		case <-deadline:
			t.Fatal("no EventTemplateRenderFailed for a failed plain start")
		}
	}
}

// TestRenderedArgvIsNeverPersisted is REQ-11's "Nothing rendered is
// persisted". The elements mix literal text with values, so the rendered
// strings exist nowhere but in the child's argv: the positive control is the
// probe record, which proves they were rendered; then no file the daemon
// wrote — state.json, the run record, the harness log, the run log — may
// contain them.
func TestRenderedArgvIsNeverPersisted(t *testing.T) {
	e := newRunsEnv(t)
	h, out := commandSweep(t, "leak", "--tag=R{{run.id}}-{{harness.name}}Z", "--m=<{{model}}>")
	h.Model = "m0del-7Qx"
	m, closeM := e.manager(t, sweepCfg(h), fastPolicy())

	m.StartRun("leak", RunRequest{Trigger: TriggerSchedule})
	rec := waitRuns(t, m, "leak", "the run finishes", outcomesAre(OutcomeSuccess))[0]
	rendered := []string{"R" + strconv.Itoa(rec.RunID) + "-leakZ", "<m0del-7Qx>"}
	p := readProbe(t, out)
	for _, r := range rendered {
		if !slices.ContainsFunc(p.Args, func(a string) bool { return strings.Contains(a, r) }) {
			t.Fatalf("the child never received %q (%q), so the absence checks below prove nothing", r, p.Args)
		}
	}
	closeM() // flush state.json and close the logs

	checked := 0
	err := filepath.WalkDir(e.dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		checked++
		for _, r := range rendered {
			if strings.Contains(string(b), r) {
				t.Errorf("%s contains the rendered value %q", path, r)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{e.state, m.RunLogPath("leak", rec.RunID)} {
		if _, err := os.Stat(want); err != nil {
			t.Errorf("expected %s to exist and be checked: %v", want, err)
		}
	}
	if checked < 3 {
		t.Errorf("only %d files checked; want at least state.json, the harness log and the run log", checked)
	}
}

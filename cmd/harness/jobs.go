package main

// Scheduled Job Verbs
//
// `harness jobs`, `harness trigger` and `harness runs`: the operator's view of
// scheduled harnesses — what is armed and when it fires next, how the latest
// runs ended, a manual run — over the daemon's run history. `harness logs
// NAME --run N` (root.go, verbs.go) reads one run's log.
//
// The manual run verb is `trigger`, not the `run` issue #120 first named: `harness
// run` already starts a scratchpad (ADR-0017), and one verb meaning two things
// depending on whether its argument happens to be a scheduled harness is the
// kind of ambiguity a CLI never gets to take back.
//
// `trigger --wait` makes a job scriptable from outside: it streams the run's log
// and exits with the run's exit code, so `harness trigger nightly --wait &&
// deploy` means what it reads as.
//
// Governing: ADR-0002 (the CLI is the supported programmatic surface), ADR-0013,
// ADR-0017; SPEC-0008 REQ "Protocol Operations", REQ "Manual Trigger"; issue
// #120.
//
// @joestump-agent 09/11/2026 - Added for issue #120.

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"gitea.stump.rocks/stump.wtf/harness/internal/client"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/schedfmt"
)

const (
	// waitPoll is how often `trigger --wait` re-reads the run and its log.
	waitPoll = time.Second
	// waitLogLines asks for the whole run log on each --wait poll.
	waitLogLines = 1 << 20
	// waitHistoryLimit is how much history --wait searches for its run — well
	// past any keep_runs a person sets.
	waitHistoryLimit = 1000

	// Exit statuses for `trigger --wait` when the run did not simply exit
	// with a code of its own.
	exitTimedOut = 124 // timeout(1)'s convention
	exitSkipped  = 75  // EX_TEMPFAIL: the job is busy, try again later
)

// waitSleep is a seam over the --wait poll interval, so tests run in
// milliseconds.
var waitSleep = time.Sleep

// exitCodeError ends the process with code once the command has already said
// everything it needed to (main.go).
type exitCodeError struct{ code int }

func (e exitCodeError) Error() string { return fmt.Sprintf("exit status %d", e.code) }

// newTriggerCmd builds `harness trigger NAME [--wait]`.
func newTriggerCmd(g *globalOpts) *cobra.Command {
	var wait bool
	cmd := &cobra.Command{
		Use:           "trigger",
		Short:         "run a scheduled harness now",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := bindName("trigger", nameRequired, args)
			if err != nil {
				return err
			}
			o := g.opts()
			o.name, o.wait = name, wait
			return run("trigger", o)
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "stream the run's log and exit with its exit code")
	return cmd
}

// newRunsCmd builds `harness runs NAME [--limit N]`.
func newRunsCmd(g *globalOpts) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:           "runs",
		Short:         "show a scheduled harness's run history",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := bindName("runs", nameRequired, args)
			if err != nil {
				return err
			}
			if limit < 1 {
				return fmt.Errorf("--limit must be at least 1")
			}
			o := g.opts()
			o.name, o.limit = name, limit
			return run("runs", o)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "number of runs to show, newest first")
	return cmd
}

// cmdJobs prints every scheduled harness.
func cmdJobs(c *client.Client, o verbOpts) error {
	jobs, err := c.Jobs()
	if err != nil {
		return err
	}
	if o.json {
		return printJSON(jobs)
	}
	return printJobsTable(os.Stdout, jobs, time.Now())
}

// printJobsTable renders the jobs listing against now.
func printJobsTable(w io.Writer, jobs []protocol.JobInfo, now time.Time) error {
	if len(jobs) == 0 {
		_, err := fmt.Fprintln(w, "no scheduled harnesses (give a prompt harness a schedule in harness.toml)")
		return err
	}
	t := NewTable(w, "NAME", "STATE", "SCHEDULE", "NEXT", "LAST RUN", "FAILS")
	for _, j := range jobs {
		t.Row(
			j.Name,
			t.stateCell(j.State, j.Schedule),
			schedfmt.LabelOrRaw(j.Schedule),
			jobNextCell(j, now),
			jobLastCell(j, now),
			strconv.Itoa(j.ConsecutiveFailures),
		)
	}
	return t.Flush()
}

// jobNextCell is the run in flight, or the countdown to the next window.
func jobNextCell(j protocol.JobInfo, now time.Time) string {
	if j.Running != nil {
		return fmt.Sprintf("running #%d", j.Running.RunID)
	}
	next, err := time.Parse(time.RFC3339, j.NextRun)
	if err != nil {
		return "—"
	}
	return schedfmt.NextInAt(next, now)
}

// jobLastCell is the newest finished record: "#12 success 3h ago".
func jobLastCell(j protocol.JobInfo, now time.Time) string {
	if j.LastRun == nil {
		return "—"
	}
	r := *j.LastRun
	return strings.TrimSpace(fmt.Sprintf("#%d %s %s", r.RunID, r.Outcome, runAgo(r, now)))
}

// runAgo is how long ago a run ended (or, still open, started).
func runAgo(r protocol.RunInfo, now time.Time) string {
	at := r.EndedAt
	if at == "" {
		at = r.StartedAt
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return ""
	}
	return schedfmt.ShortDuration(max(now.Sub(t), 0)) + " ago"
}

// cmdRuns prints a harness's run history.
func cmdRuns(c *client.Client, o verbOpts) error {
	rd, err := c.Runs(o.name, o.limit)
	if err != nil {
		return err
	}
	if o.json {
		return printJSON(rd)
	}
	return printRunsTable(os.Stdout, rd)
}

// printRunsTable renders a run history, newest first.
func printRunsTable(w io.Writer, rd protocol.RunsData) error {
	if len(rd.Runs) == 0 {
		_, err := fmt.Fprintf(w, "%s has no runs yet\n", rd.Name)
		return err
	}
	t := NewTable(w, "RUN", "TRIGGER", "OUTCOME", "STARTED", "DURATION", "EXIT")
	for _, r := range rd.Runs {
		t.Row(strconv.Itoa(r.RunID), r.Trigger, runOutcomeCell(r), runStartedCell(r), runDurationCell(r), runExitCell(r))
	}
	return t.Flush()
}

// runOutcomeCell is the outcome, with the window count a missed record covers
// ("missed ×4") — kept short so it fits one table column.
func runOutcomeCell(r protocol.RunInfo) string {
	if r.Outcome == "missed" && r.Windows > 1 {
		return fmt.Sprintf("missed ×%d", r.Windows)
	}
	return r.Outcome
}

func runStartedCell(r protocol.RunInfo) string {
	t, err := time.Parse(time.RFC3339Nano, r.StartedAt)
	if err != nil {
		return r.StartedAt
	}
	return t.Local().Format("Jan _2 15:04")
}

func runDurationCell(r protocol.RunInfo) string {
	switch {
	case r.Outcome == "running":
		return "running"
	case r.EndedAt == "":
		return "unknown"
	case r.DurationMs > 0:
		return schedfmt.ShortDuration(time.Duration(r.DurationMs) * time.Millisecond)
	}
	return "—"
}

func runExitCell(r protocol.RunInfo) string {
	if r.ExitCode == nil {
		return "—"
	}
	return strconv.Itoa(*r.ExitCode)
}

// cmdTrigger starts a manual run and, with --wait, follows it to the end.
func cmdTrigger(c *client.Client, o verbOpts) error {
	before := 0
	if o.wait {
		// A queued trigger has no run id until it starts, so note the newest
		// id now to recognise the run when it does.
		rd, err := c.Runs(o.name, 1)
		if err != nil {
			return err
		}
		if len(rd.Runs) > 0 {
			before = rd.Runs[0].RunID
		}
	}
	td, err := c.Trigger(o.name)
	if err != nil {
		return err
	}
	if !o.wait {
		if o.json {
			return printJSON(td)
		}
		fmt.Println(triggerLine(td))
		return nil
	}

	if !o.json {
		fmt.Fprintln(os.Stderr, triggerLine(td))
	}
	if td.Decision == protocol.TriggerSkipped {
		if o.json {
			if err := printJSON(td); err != nil {
				return err
			}
		}
		return exitCodeError{code: exitSkipped}
	}
	id := 0
	if td.Run != nil {
		id = td.Run.RunID
	}
	final, err := waitForRun(c, o, id, before, os.Stdout)
	if err != nil {
		return err
	}
	if o.json {
		if err := printJSON(final); err != nil {
			return err
		}
	} else {
		fmt.Fprintln(os.Stderr, finishLine(o.name, final))
	}
	if code := waitExitCode(final); code != 0 {
		return exitCodeError{code: code}
	}
	return nil
}

// triggerLine says what the daemon did with a trigger.
func triggerLine(td protocol.TriggerData) string {
	switch td.Decision {
	case protocol.TriggerStarted:
		if td.Run != nil {
			return fmt.Sprintf("%s: started run #%d", td.Name, td.Run.RunID)
		}
		return fmt.Sprintf("%s: started", td.Name)
	case protocol.TriggerQueued:
		return fmt.Sprintf("%s: a run is in flight; this one is queued behind it (on_overlap = queue)", td.Name)
	}
	if td.Run != nil {
		return fmt.Sprintf("%s: a run is in flight; skipped, recorded as run #%d", td.Name, td.Run.RunID)
	}
	return fmt.Sprintf("%s: skipped", td.Name)
}

// finishLine reports how a followed run ended.
func finishLine(name string, r protocol.RunInfo) string {
	line := fmt.Sprintf("%s: run #%d %s", name, r.RunID, r.Outcome)
	if r.ExitCode != nil {
		line += fmt.Sprintf(" (exit %d)", *r.ExitCode)
	}
	if r.DurationMs > 0 {
		line += " after " + schedfmt.ShortDuration(time.Duration(r.DurationMs)*time.Millisecond)
	}
	return line
}

// errRunGone is returned when the followed run falls out of history mid-wait.
var errRunGone = errors.New("the run is no longer in the harness's history")

// waitForRun follows run id — or, for a queued trigger (id 0), the first manual
// run newer than before — until it ends, streaming its log to w unless --json.
func waitForRun(c *client.Client, o verbOpts, id, before int, w io.Writer) (protocol.RunInfo, error) {
	printed := ""
	for {
		rd, err := c.Runs(o.name, waitHistoryLimit)
		if err != nil {
			return protocol.RunInfo{}, err
		}
		rec, found := pickRun(rd.Runs, id, before)
		if id > 0 && !found {
			return protocol.RunInfo{}, fmt.Errorf("run #%d: %w", id, errRunGone)
		}
		if found {
			id = rec.RunID
			if !o.json && rec.HasLog {
				if ld, err := c.RunLogs(o.name, id, waitLogLines); err == nil {
					printed = printNewLogText(w, printed, ld.Text)
				}
			}
			if rec.Outcome != "running" {
				return rec, nil
			}
		}
		waitSleep(waitPoll)
	}
}

// pickRun finds run id in a newest-first history, or — with id 0 — the oldest
// manual run newer than before that actually started (a skipped record is a
// different trigger).
func pickRun(runs []protocol.RunInfo, id, before int) (protocol.RunInfo, bool) {
	var match protocol.RunInfo
	found := false
	for _, r := range runs {
		switch {
		case id > 0:
			if r.RunID == id {
				return r, true
			}
		case r.RunID > before && r.Trigger == "manual" && r.Outcome != "skipped":
			match, found = r, true // newest first, so the last match is the oldest
		}
	}
	return match, found
}

// printNewLogText prints what text adds to what was already printed, made
// inert first (#146). A log that no longer extends the previous text is
// reprinted whole.
func printNewLogText(w io.Writer, printed, text string) string {
	cur := inertLogText(text)
	if strings.HasPrefix(cur, printed) {
		fmt.Fprint(w, cur[len(printed):])
	} else {
		fmt.Fprint(w, cur)
	}
	return cur
}

// waitExitCode maps a finished run to `trigger --wait`'s exit status, so a job
// scripts the way its own command would: the run's exit code when it exited
// with one, 124 for a timeout, 75 for a skip, and 1 for any other failure.
func waitExitCode(r protocol.RunInfo) int {
	switch r.Outcome {
	case "success":
		return 0
	case "timed_out":
		return exitTimedOut
	case "skipped":
		return exitSkipped
	}
	if r.ExitCode != nil && *r.ExitCode > 0 && *r.ExitCode < 256 {
		return *r.ExitCode
	}
	return 1
}

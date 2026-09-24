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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/stump-wtf/harness/internal/buildinfo"
	"github.com/stump-wtf/harness/internal/client"
	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/ledger"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/runquery"
	"github.com/stump-wtf/harness/internal/schedfmt"
	"github.com/stump-wtf/harness/internal/supervisor"
	"github.com/stump-wtf/harness/internal/trigger"
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

// newTriggerCmd builds `harness trigger NAME [--wait] [--event FILE]`.
func newTriggerCmd(g *globalOpts) *cobra.Command {
	var (
		wait      bool
		eventFile string
	)
	cmd := &cobra.Command{
		Use:           "trigger",
		Short:         "run a triggered harness now",
		Args:          cobra.MaximumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := bindName("trigger", nameRequired, args)
			if err != nil {
				return err
			}
			o := g.opts()
			o.name, o.wait, o.eventFile = name, wait, eventFile
			return run("trigger", o)
		},
	}
	cmd.Flags().BoolVar(&wait, "wait", false, "stream the run's log and exit with its exit code")
	// A past run's event file is a valid input by construction: it is the same
	// envelope schema, so replaying one is a copy rather than a translation.
	// That is what makes a failed webhook run debuggable offline — no sender,
	// no listener, no port.
	cmd.Flags().StringVar(&eventFile, "event", "",
		"replay an event envelope from FILE (e.g. a past run's <id>.event.json)")
	return cmd
}

// newRunsCmd builds `harness runs [NAME...]` (SPEC-0022 REQ-14): SPEC-0008's
// one-harness history when given a single NAME and no other filter, and a
// query across the run ledger otherwise.
func newRunsCmd(g *globalOpts) *cobra.Command {
	var (
		limit    int
		harness  []string
		since    string
		until    string
		outcomes []string
		triggers []string
		wide     bool
	)
	cmd := &cobra.Command{
		Use:           "runs [NAME...]",
		Short:         "show run history from the run ledger",
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			rq := runsRequest{names: append(slices.Clone(args), harness...), wide: wide, now: time.Now()}
			rq.legacy = len(args) == 1 && len(harness) == 0 && since == "" && until == "" &&
				len(outcomes) == 0 && len(triggers) == 0
			rq.limit = limit
			if !cmd.Flags().Changed("limit") {
				rq.limit = 50
				if rq.legacy {
					rq.limit = 20
				}
			}
			if rq.limit < 1 || rq.limit > runquery.MaxLimit {
				return fmt.Errorf("--limit must be between 1 and %d", runquery.MaxLimit)
			}
			if err := runquery.CheckValues("outcome", outcomes, runquery.Outcomes); err != nil {
				return err
			}
			if err := runquery.CheckValues("trigger", triggers, runquery.Triggers); err != nil {
				return err
			}
			rq.outcomes, rq.triggers = outcomes, triggers
			if !rq.legacy {
				// A query with no --since reads the last day (REQ-14).
				if since == "" {
					since = "24h"
				}
				t, err := runquery.ParseSince(since, rq.now)
				if err != nil {
					return err
				}
				rq.since = t
			}
			if until != "" {
				t, err := time.Parse(time.RFC3339Nano, until)
				if err != nil {
					return fmt.Errorf("--until %q: want an RFC 3339 instant", until)
				}
				rq.until = t
			}
			return cmdRunsQuery(g.opts(), rq, os.Stdout, os.Stderr)
		},
	}
	cmd.Flags().IntVar(&limit, "limit", 20, "number of runs to show, newest first (default 20 for one NAME, else 50)")
	cmd.Flags().StringArrayVar(&harness, "harness", nil, "a harness to include (repeatable)")
	cmd.Flags().StringVar(&since, "since", "", "runs started since a duration ago (7d, 36h) or an RFC 3339 instant (default 24h across harnesses)")
	cmd.Flags().StringVar(&until, "until", "", "runs started before an RFC 3339 instant")
	cmd.Flags().StringSliceVar(&outcomes, "outcome", nil, "only these outcomes, comma-separated (e.g. failed,timed_out)")
	cmd.Flags().StringSliceVar(&triggers, "trigger", nil, "only these triggers, comma-separated (e.g. webhook,channel)")
	cmd.Flags().BoolVar(&wide, "wide", false, "add MODEL, TOKENS, COST and TODO columns")
	return cmd
}

// runsRequest is a parsed `harness runs` invocation.
type runsRequest struct {
	names              []string
	legacy             bool // a single NAME and no other filter: SPEC-0008's view
	since, until       time.Time
	outcomes, triggers []string
	limit              int
	wide               bool
	now                time.Time
}

// cmdRunsQuery asks the daemon, and, when there is no daemon to ask, reads the
// ledger's day files itself (SPEC-0022 REQ-16). It exits 0 whenever the query
// ran, records or none, and fails only when neither could be read (REQ-14).
func cmdRunsQuery(o verbOpts, rq runsRequest, stdout, stderr io.Writer) error {
	rd, err := daemonRuns(o, rq)
	if err != nil {
		why, offline := unreachable(err)
		if !offline {
			return err
		}
		rd, err = offlineRuns(o, rq)
		if err != nil {
			return fmt.Errorf("daemon %s, and the run ledger could not be read: %w", why, err)
		}
		fmt.Fprintf(stderr, "harness: daemon %s; read the ledger from disk\n", why)
	}
	return printRuns(stdout, rd, rq, o.json)
}

// daemonRuns asks the daemon.
func daemonRuns(o verbOpts, rq runsRequest) (protocol.RunsData, error) {
	c, err := client.Dial(o.socket, buildinfo.Version, nil)
	if err != nil {
		return protocol.RunsData{}, err
	}
	defer c.Close()
	if rq.legacy {
		// SPEC-0008's request, exactly: an older daemon answers it too.
		return c.Runs(rq.names[0], rq.limit)
	}
	q := client.RunsQuery{Names: rq.names, Outcomes: rq.outcomes, Triggers: rq.triggers, Limit: rq.limit}
	if !rq.since.IsZero() {
		q.Since = rq.since.UTC().Format(time.RFC3339Nano)
	}
	if !rq.until.IsZero() {
		q.Until = rq.until.UTC().Format(time.RFC3339Nano)
	}
	return c.QueryRuns(q)
}

// unreachable reports a dial that found no daemon to answer: nothing bound to
// the socket, or something bound that never said hello.
func unreachable(err error) (why string, ok bool) {
	var silent *client.NoHandshakeError
	switch {
	case errors.As(err, &silent):
		return "not responding", true
	case errors.Is(err, syscall.ECONNREFUSED), errors.Is(err, os.ErrNotExist):
		return "not running", true
	}
	return "", false
}

// offlineRuns reads the ledger read-only. It reconciles, prunes and writes
// nothing, so a run a crashed daemon left open is shown as `running?`: the
// CLI cannot tell a live run from one nobody will ever close (REQ-16).
func offlineRuns(o verbOpts, rq runsRequest) (protocol.RunsData, error) {
	l, err := ledger.OpenReader(offlineLedgerDir(o))
	if err != nil {
		return protocol.RunsData{}, err
	}
	q := ledger.Query{Names: rq.names, Since: rq.since, Until: rq.until, Outcomes: rq.outcomes, Triggers: rq.triggers, Limit: rq.limit}
	recs, oldest, err := l.Query(q)
	if err != nil {
		return protocol.RunsData{}, err
	}
	rd := protocol.RunsData{Runs: runquery.Infos(recs), OldestSeq: oldest}
	if rq.legacy {
		rd.Name = rq.names[0]
	}
	for i := range rd.Runs {
		if rd.Runs[i].Outcome == string(supervisor.OutcomeRunning) {
			rd.Runs[i].Outcome = "running?"
		}
	}
	return rd, nil
}

// offlineLedgerDir is the ledger a default daemon writes: beside its
// state.json (SPEC-0022 REQ-1). A variable so a test can point it at a temp
// directory.
var offlineLedgerDir = func(verbOpts) string {
	return filepath.Join(filepath.Dir(supervisor.DefaultStatePath()), "ledger")
}

// printRuns renders runs. --json is SPEC-0008's {name, runs} object for the
// one-harness view, so a script written against it keeps working with only
// new fields added, and the REQ-4 record list, newest first, for a query
// (REQ-14).
func printRuns(w io.Writer, rd protocol.RunsData, rq runsRequest, asJSON bool) error {
	if asJSON {
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		if rq.legacy {
			return enc.Encode(rd)
		}
		return enc.Encode(rd.Runs)
	}
	return printRunsTable(w, rd, rq.wide)
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
			// A scheduled one-shot is never gated (ADR-0019 exclusions).
			t.stateCell(j.State, j.Schedule, false, false),
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

// printRunsTable renders a run history, newest first, in 80 columns:
// HARNESS, RUN, STARTED, TOOK, TRIGGER, OUTCOME and EXIT; wide adds MODEL,
// TOKENS, COST and TODO (SPEC-0022 REQ-14).
func printRunsTable(w io.Writer, rd protocol.RunsData, wide bool) error {
	if len(rd.Runs) == 0 {
		msg := "no runs match"
		if rd.Name != "" {
			msg = rd.Name + " has no runs yet"
		}
		_, err := fmt.Fprintln(w, msg)
		return err
	}
	t := NewTable(w, runsHeaders(wide)...)
	if wide {
		t.width = max(t.width, wideRunsWidth)
	}
	for _, r := range rd.Runs {
		h := r.Harness
		if h == "" {
			h = rd.Name
		}
		cells := []string{h, strconv.Itoa(r.RunID), runStartedCell(r), runDurationCell(r), r.Trigger, runOutcomeCell(r), runExitCell(r)}
		if wide {
			cells = append(cells, dashIfEmpty(r.Model), runTokensCell(r), runCostCell(r), dashIfEmpty(r.TodoID))
		}
		t.Row(cells...)
	}
	return t.Flush()
}

// runsHeaders is the runs table's columns.
func runsHeaders(wide bool) []string {
	h := []string{"HARNESS", "RUN", "STARTED", "TOOK", "TRIGGER", "OUTCOME", "EXIT"}
	if wide {
		h = append(h, "MODEL", "TOKENS", "COST", "TODO")
	}
	return h
}

// wideRunsWidth is the least budget --wide lays out in: it is for a wide
// terminal or a pipe, and eleven columns in 64 cells would help no one.
const wideRunsWidth = 140

func dashIfEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

func runTokensCell(r protocol.RunInfo) string {
	if r.Tokens == nil {
		return "—"
	}
	return strconv.FormatInt(r.Tokens.Input+r.Tokens.Output, 10)
}

// runCostCell is the cost with its source; an unknown cost is "?", never a
// zero it is not.
func runCostCell(r protocol.RunInfo) string {
	switch {
	case r.CostUSD != nil && r.CostSource == "recorded":
		return fmt.Sprintf("$%.2f", *r.CostUSD)
	case r.CostUSD != nil:
		return fmt.Sprintf("$%.2f %s", *r.CostUSD, r.CostSource)
	case r.CostSource != "":
		return "?"
	}
	return "—"
}

// runOutcomeCell is the outcome, with the window count a missed record covers
// ("missed ×4") — kept short so it fits one table column.
func runOutcomeCell(r protocol.RunInfo) string {
	switch {
	case r.Outcome == "missed" && r.Windows > 1:
		return fmt.Sprintf("missed ×%d", r.Windows)
	case r.LogPruned:
		// REQ-12: the record outlived its log; `logs --run` says so too.
		return r.Outcome + " (log pruned)"
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
	case r.Outcome == "running" || r.Outcome == "running?":
		return r.Outcome
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

// readEventArg reads and shape-checks a --event file locally before dialling,
// so an operator who mistyped a path or pointed at a log file hears about it
// here rather than as an invalid_event from the daemon. The daemon validates
// it again, against the harness, and that check is the authoritative one —
// this is only about a better error for the common mistake.
//
// The local size cap is the CEILING any harness may have
// (core.MaxWebhookMaxBody), not the 1 MiB default: the client does not know
// the harness's sources, and capping at the default here would refuse, before
// the daemon ever saw it, a replay of a delivery that a `max_body = "5MiB"`
// route legitimately accepted. The per-harness cap is the daemon's.
// Governing: SPEC-0014 REQ "Manual Trigger With Event".
//
// The read itself is bounded one byte past the cap, so pointing --event at a
// multi-gigabyte log costs a cap's worth of memory rather than the whole file;
// ParseEnvelope then refuses the over-cap result by length.
func readEventArg(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("--event: %w", err)
	}
	b, err := io.ReadAll(io.LimitReader(f, core.MaxWebhookMaxBody+1))
	_ = f.Close()
	if err != nil {
		return nil, fmt.Errorf("--event: %w", err)
	}
	if _, err := trigger.ParseEnvelope(b, core.MaxWebhookMaxBody); err != nil {
		return nil, fmt.Errorf("--event %s: %w", path, err)
	}
	return b, nil
}

// cmdTrigger starts a manual run and, with --wait, follows it to the end.
func cmdTrigger(c *client.Client, o verbOpts) error {
	var event []byte
	if o.eventFile != "" {
		b, err := readEventArg(o.eventFile)
		if err != nil {
			return err
		}
		event = b
	}
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
	td, err := c.TriggerWithEvent(o.name, event)
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

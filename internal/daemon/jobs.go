package daemon

// Scheduled Runs Over The Protocol
//
// The jobs, trigger and runs ops, and the run selector on logs: the client
// surface over run history (#119). Everything here reads the daemon's own
// records — the live scheduler's next window, the supervisor's run history — so
// a client never does cron math, and never has to guess which run it is
// looking at.
//
// trigger goes through Manager.StartRun, the same entry a schedule firing uses,
// so a manual run honors on_overlap exactly as a scheduled one does rather than
// through a parallel start path that could disagree.
//
// Governing: ADR-0002 (control mirrors the CLI verbs), ADR-0008 (run records
// carry outcomes, times and paths — never environment or output), ADR-0013;
// SPEC-0002 REQ "Control Operations"; SPEC-0008 REQ "Protocol Operations", REQ
// "Manual Trigger"; issue #120.
//
// @joestump-agent 09/11/2026 - Added for issue #120.
//
// @joestump 09/22/2026 - trigger now accepts any TRIGGERED harness, not only a
// scheduled one, and an optional event envelope to replay into it (ADR-0021;
// SPEC-0014 REQ "Manual Trigger With Event", REQ "Run Record Fields").

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/protocol"
	"github.com/stump-wtf/harness/internal/runquery"
	"github.com/stump-wtf/harness/internal/supervisor"
	"github.com/stump-wtf/harness/internal/trigger"
)

// defaultRunsLimit is how many records runs returns when the request sets no
// limit — keep_runs' default, so an unset limit shows a default history whole.
const defaultRunsLimit = 20

// defaultQueryLimit is a query's default page (SPEC-0022 REQ-14).
const defaultQueryLimit = 50

// opJobs lists every scheduled harness, in config order.
func (c *conn) opJobs() []protocol.JobInfo {
	mgr := c.srv.mgr
	out := []protocol.JobInfo{}
	for _, snap := range mgr.Snapshots() {
		if !snap.Scheduled {
			continue
		}
		h, _, ok := mgr.HarnessRecord(snap.Name)
		if !ok || h.Schedule == "" {
			continue
		}
		job := protocol.JobInfo{
			Name:        snap.Name,
			Schedule:    h.Schedule,
			Description: h.Description,
			State:       string(snap.State),
			CatchUp:     h.CatchUp,
			TimeoutMs:   h.Timeout.Milliseconds(),
			OnOverlap:   string(h.OnOverlap),
			KeepRuns:    h.KeepRuns,
		}
		if c.srv.sched != nil {
			if next, ok := c.srv.sched.NextFire(snap.Name); ok {
				job.NextRun = next.Format(time.RFC3339)
			}
		}
		runs := mgr.Runs(snap.Name)
		for i := len(runs) - 1; i >= 0 && (job.Running == nil || job.LastRun == nil); i-- {
			info := c.runInfo(snap.Name, runs[i])
			switch {
			case runs[i].Outcome == supervisor.OutcomeRunning:
				if job.Running == nil {
					job.Running = &info
				}
			case job.LastRun == nil:
				job.LastRun = &info
			}
		}
		job.ConsecutiveFailures = supervisor.ConsecutiveFailures(runs)
		out = append(out, job)
	}
	return out
}

// opTrigger starts a manual run of a triggered harness, optionally replaying
// an event into it.
//
// "Triggered", not "scheduled": SPEC-0014 REQ "Manual Trigger With Event" says
// trigger accepts any harness with a `schedule`, `triggers`, or both, and
// answers not_scheduled only for one with neither. A webhook-only harness that
// could not be triggered by hand would have no way to be exercised at all
// without a real delivery.
func (c *conn) opTrigger(req protocol.ControlReq) {
	h, _, ok := c.srv.mgr.HarnessRecord(req.Name)
	if !ok {
		_ = c.pc.WriteError(req.ID, protocol.ErrUnknownHarness, "unknown harness %q", req.Name)
		return
	}
	if !h.Triggered() {
		_ = c.pc.WriteError(req.ID, protocol.ErrNotScheduled,
			"harness %q has no schedule and no triggers; trigger runs triggered harnesses (use start for any other)", req.Name)
		return
	}
	run := supervisor.RunRequest{Trigger: supervisor.TriggerManual}
	if len(req.Event) > 0 {
		env, err := c.parseTriggerEvent(h, req.Event)
		if err != nil {
			_ = c.pc.WriteError(req.ID, protocol.ErrInvalidEvent, "%s", err.Error())
			return
		}
		run.Source = env.Source
		run.Event = env
	}
	d, ok := c.srv.mgr.StartRun(req.Name, run)
	if !ok {
		_ = c.pc.WriteError(req.ID, protocol.ErrUnknownHarness, "unknown harness %q", req.Name)
		return
	}
	out := protocol.TriggerData{Name: req.Name}
	switch d.Kind {
	case supervisor.DecisionStarted:
		out.Decision = protocol.TriggerStarted
	case supervisor.DecisionQueued:
		out.Decision = protocol.TriggerQueued
	default:
		out.Decision = protocol.TriggerSkipped
	}
	if d.Run.RunID > 0 {
		// Re-read: a run whose process failed to spawn is already final by the
		// time StartRun returns, and the reply should say so.
		rec := d.Run
		if latest, ok := c.findRun(req.Name, rec.RunID); ok {
			rec = latest
		}
		info := c.runInfo(req.Name, rec)
		out.Run = &info
	}
	c.respond(req, out)
}

// parseTriggerEvent validates a replayed envelope against the harness it is
// being replayed into, and stamps it as a replay.
//
// Three checks, each refusing a different mistake:
//
//   - The SIZE cap is the largest `max_body` among the harness's webhook
//     sources, or 1 MiB when it binds none. It is read before the decode, so a
//     huge file costs a length check rather than a parse.
//   - The SHAPE is validated by the envelope itself, so a hand-edited file
//     cannot reach the run machinery with a kind and a source that disagree.
//   - The SOURCE must be one the harness binds. This is the one an operator
//     will actually hit: replaying `pr-review`'s event into `deploy-check`
//     would otherwise record a run against a source that harness never listens
//     to, and the run's own `HARNESS_RUN_SOURCE` would be a lie.
//
// Governing: SPEC-0014 REQ "Manual Trigger With Event".
func (c *conn) parseTriggerEvent(h core.Harness, raw []byte) (*trigger.Envelope, error) {
	env, err := trigger.ParseEnvelope(raw, trigger.MaxEventBytes(h, c.srv.mgr.Config()))
	if err != nil {
		return nil, err
	}
	if !trigger.Binds(h, env.Source) {
		return nil, fmt.Errorf("harness %q does not bind %q (its triggers are %s)",
			h.Name, env.Source, triggerList(h))
	}
	return env.Replay(time.Now()), nil
}

// triggerList renders a harness's bindings for an error message, so the
// operator can see what they should have replayed into.
func triggerList(h core.Harness) string {
	if len(h.Triggers) == 0 {
		return "none"
	}
	return strings.Join(h.Triggers, ", ")
}

// opRuns answers the runs op from the run ledger (SPEC-0022 REQ-13, REQ-15).
//
// A request with only a name and a limit is SPEC-0008's: one harness's history,
// newest first, default 20, and an unknown name is an error — any known harness
// may be asked, including one that has since lost its schedule. Anything more
// (names, since/until, outcomes, triggers, a paging cursor) is a query across
// the ledger, where a name need not still be configured: a removed harness's
// history is still history. Either way the records come from memory for the
// last seven days and from the day files beyond that.
func (c *conn) opRuns(req protocol.ControlReq) {
	query := runquery.IsQuery(req)
	defaultLimit := defaultRunsLimit
	if query {
		defaultLimit = defaultQueryLimit
	} else if _, _, ok := c.srv.mgr.HarnessRecord(req.Name); !ok {
		_ = c.pc.WriteError(req.ID, protocol.ErrUnknownHarness, "unknown harness %q", req.Name)
		return
	}
	q, err := runquery.FromRequest(req, defaultLimit)
	if err != nil {
		_ = c.pc.WriteError(req.ID, protocol.ErrBadRequest, "runs: %v", err)
		return
	}
	recs, oldest, err := c.srv.mgr.Ledger().Query(q)
	if err != nil {
		_ = c.pc.WriteError(req.ID, protocol.ErrInternal, "runs: %v", err)
		return
	}
	out := protocol.RunsData{Name: req.Name, Runs: runquery.Infos(recs), OldestSeq: oldest}
	c.respond(req, out)
}

// opLogsRun serves logs for one run of a harness (req.Run). The raw view reads
// the run's own log. The events view describes exactly the run record's window
// — the seam #307 left on Since/Until — and, where it would fall back to the
// harness-wide log tail, shows the run's own log instead: the request is about
// one run.
func (c *conn) opLogsRun(req protocol.ControlReq, snap supervisor.Snapshot, lines int) {
	rec, ok := c.findRun(req.Name, req.Run)
	if !ok {
		_ = c.pc.WriteError(req.ID, protocol.ErrUnknownRun,
			"harness %q has no run %d in its history (never run, or past the ledger's retention)", req.Name, req.Run)
		return
	}
	text, hasLog := readRunLogTail(runLogOf(c.srv.mgr, req.Name, rec), lines)
	var notices []string
	if !hasLog {
		notices = append(notices, noRunLogNotice(rec))
	}
	if rec.EndedAt == nil && rec.Outcome != supervisor.OutcomeRunning {
		notices = append(notices, fmt.Sprintf("run %d's end is unknown: the daemon exited while it was in flight", rec.RunID))
	}
	if !req.Events {
		c.respond(req, protocol.LogsData{Name: req.Name, Text: text, Notices: notices})
		return
	}

	scoped := req
	scoped.Since = rec.StartedAt.Format(time.RFC3339Nano)
	scoped.Until = ""
	if rec.EndedAt != nil {
		scoped.Until = rec.EndedAt.Format(time.RFC3339Nano)
	}
	data, err := c.activity(scoped, snap, lines)
	if err != nil {
		_ = c.pc.WriteError(req.ID, protocol.ErrBadRequest, "logs %q run %d: %v", req.Name, req.Run, err)
		return
	}
	if data.Run != nil && data.Run.ExitCode == nil {
		data.Run.ExitCode = rec.ExitCode
	}
	if data.Source != protocol.LogSourceAgentTrace || data.Text != "" {
		data.Text = text
	}
	data.Notices = append(data.Notices, notices...)
	c.respond(req, data)
}

// findRun returns name's record for run id, from the run ledger.
func (c *conn) findRun(name string, id int) (supervisor.RunRecord, bool) {
	return c.srv.mgr.Run(name, id)
}

// runLogOf is the per-run log a record names. A record imported from a
// pre-ledger state.json, or written before records carried their log, falls
// back to where the log would be.
func runLogOf(mgr *supervisor.Manager, name string, r supervisor.RunRecord) string {
	if r.Log != "" {
		return r.Log
	}
	return mgr.RunLogPath(name, r.RunID)
}

// runInfo projects a run record onto the wire.
func (c *conn) runInfo(name string, r supervisor.RunRecord) protocol.RunInfo {
	info := protocol.RunInfo{
		RunID:     r.RunID,
		Trigger:   string(r.Trigger),
		Outcome:   string(r.Outcome),
		StartedAt: r.StartedAt.Format(time.RFC3339Nano),
		ExitCode:  r.ExitCode,
		Windows:   r.Windows,
	}
	if r.EndedAt != nil {
		info.EndedAt = r.EndedAt.Format(time.RFC3339Nano)
		info.DurationMs = r.EndedAt.Sub(r.StartedAt).Milliseconds()
	}
	if r.Window != nil {
		info.Window = r.Window.Format(time.RFC3339)
	}
	if r.FirstWindow != nil {
		info.FirstWindow = r.FirstWindow.Format(time.RFC3339)
	}
	// Identifiers only. The event's payload lives in the run's event file,
	// which nothing projects onto the wire (ADR-0008; SPEC-0014 REQ "Run
	// Record Fields").
	info.Source = r.Source
	info.EventID = r.EventID
	if path := runLogOf(c.srv.mgr, name, r); path != "" && !r.LogPruned {
		if _, err := os.Stat(path); err == nil {
			info.HasLog = true
		}
	}
	info.LogPruned = r.LogPruned
	info.Reason = string(r.Reason)
	info.TodoID = r.TodoID
	return info
}

// readRunLogTail returns the last lines of a run's log, and whether the log
// exists at all.
func readRunLogTail(path string, lines int) (string, bool) {
	if path == "" {
		return "", false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	// Masked like the harness-wide tail: a run log is the same durable output,
	// and opLogsRun serves it on the events path too, not only for --raw
	// (ADR-0008 as amended; issue #312).
	return redactTail(tailLines(data, lines)), true
}

// noRunLogNotice explains a run with no log.
func noRunLogNotice(r supervisor.RunRecord) string {
	switch r.Outcome {
	case supervisor.OutcomeSkipped, supervisor.OutcomeMissed:
		return fmt.Sprintf("run %d started no process (%s), so it has no log", r.RunID, r.Outcome)
	}
	if r.LogPruned {
		return fmt.Sprintf("run %d's log was pruned by keep_runs; its record is kept in the run ledger", r.RunID)
	}
	return fmt.Sprintf("run %d has no log file (it could not be created, or was removed)", r.RunID)
}

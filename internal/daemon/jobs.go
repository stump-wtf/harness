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

import (
	"fmt"
	"os"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/supervisor"
)

// defaultRunsLimit is how many records runs returns when the request sets no
// limit — keep_runs' default, so an unset limit shows a default history whole.
const defaultRunsLimit = 20

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

// opTrigger starts a manual run of a scheduled harness.
func (c *conn) opTrigger(req protocol.ControlReq) {
	h, _, ok := c.srv.mgr.HarnessRecord(req.Name)
	if !ok {
		_ = c.pc.WriteError(req.ID, protocol.ErrUnknownHarness, "unknown harness %q", req.Name)
		return
	}
	if h.Schedule == "" {
		_ = c.pc.WriteError(req.ID, protocol.ErrNotScheduled,
			"harness %q has no schedule; trigger runs scheduled harnesses (use start for any other)", req.Name)
		return
	}
	d, ok := c.srv.mgr.StartRun(req.Name, supervisor.RunRequest{Trigger: supervisor.TriggerManual})
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

// opRuns returns one harness's run history, newest first. Any known harness
// may be asked — one that has since lost its schedule still has its history.
func (c *conn) opRuns(req protocol.ControlReq) {
	if _, _, ok := c.srv.mgr.HarnessRecord(req.Name); !ok {
		_ = c.pc.WriteError(req.ID, protocol.ErrUnknownHarness, "unknown harness %q", req.Name)
		return
	}
	limit := req.Limit
	if limit <= 0 {
		limit = defaultRunsLimit
	}
	runs := c.srv.mgr.Runs(req.Name)
	out := protocol.RunsData{Name: req.Name, Runs: []protocol.RunInfo{}}
	for i := len(runs) - 1; i >= 0 && len(out.Runs) < limit; i-- {
		out.Runs = append(out.Runs, c.runInfo(req.Name, runs[i]))
	}
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
			"harness %q has no run %d in its history (never run, or pruned past keep_runs)", req.Name, req.Run)
		return
	}
	text, hasLog := readRunLogTail(c.srv.mgr.RunLogPath(req.Name, rec.RunID), lines)
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

// findRun returns name's record for run id.
func (c *conn) findRun(name string, id int) (supervisor.RunRecord, bool) {
	for _, r := range c.srv.mgr.Runs(name) {
		if r.RunID == id {
			return r, true
		}
	}
	return supervisor.RunRecord{}, false
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
	if path := c.srv.mgr.RunLogPath(name, r.RunID); path != "" {
		if _, err := os.Stat(path); err == nil {
			info.HasLog = true
		}
	}
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
	return fmt.Sprintf("run %d has no log file (it could not be created, or was removed)", r.RunID)
}

package daemon

// Run Activity
//
// Serves the structured view of `harness logs`: one run of one harness, told
// as the lifecycle lines the supervisor wrote plus the agent-trace events the
// run correlation rule attributes to that run. It works the same whether the
// harness is running or long dead — a post-mortem on the last run of a failed
// sweep is the case it exists for — and it falls back to the durable log tail
// whenever there is no agent activity to show, so the operator never gets less
// than `harness logs` used to print.
//
// Everything that could put another harness's work on this one is decided in
// runtrace and fails closed. What this file adds is the daemon's knowledge the
// rule needs: each harness's adapter, workdir and discovery environment, and
// the run windows of every harness sharing that workdir.
//
// Governing: SPEC-0002 REQ "Control Operations" ("logs"), SPEC-0006 REQ "Run
// Correlation", ADR-0011 (agent adapters), ADR-0007 (the durable log stays the
// fallback record), issues #302 and #89.
//
// @joestump-agent 09/11/2026 - Added for harness#302.
//
// @joestump-agent 09/11/2026 - Review of #307: a dead run's open window closes
// at this daemon's start; peers match through symlinks, and a peer with no
// workdir claims the daemon's own directory.

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/runtrace"
	"gitea.stump.rocks/stump.wtf/harness/internal/supervisor"
)

// activityTimeout bounds one structured logs request. The work is local
// SQLite and JSONL reads, but a store locked by a wedged writer must not hold a
// control connection open indefinitely.
const activityTimeout = 20 * time.Second

// errBadWindow reports an unparseable Since/Until on a logs request.
var errBadWindow = errors.New("since/until must be RFC 3339")

// activity builds the structured logs reply for req.Name. The harness is known
// to exist (opLogs checked the snapshot).
func (c *conn) activity(req protocol.ControlReq, snap supervisor.Snapshot, lines int) (protocol.LogsData, error) {
	mgr := c.srv.mgr
	dir := mgr.LogDir()
	data := protocol.LogsData{Name: req.Name}
	h, _, ok := mgr.HarnessRecord(req.Name)
	if !ok {
		data.Text = readLogTail(dir, req.Name, lines)
		return data, nil
	}
	target := runtrace.Scope{Name: req.Name, Adapter: h.Adapter, Workdir: supervisor.Workdir(h), Args: h.Args}
	if _, err := runtrace.Sources(target); err != nil {
		// No native transcript (generic): the durable log is this harness's
		// record, exactly as ADR-0007 has it. Source stays empty so the client
		// renders Text.
		data.Text = readLogTail(dir, req.Name, lines)
		return data, nil
	}
	data.Source = protocol.LogSourceAgentTrace
	now := time.Now()

	lifecycle, lerr := supervisor.ReadLifecycle(dir, req.Name, time.Time{})
	if lerr != nil {
		data.Notices = append(data.Notices, fmt.Sprintf("lifecycle lines partly unreadable: %v", lerr))
	}
	w, exit, note, err := runWindow(req, snap, supervisor.RunSpans(lifecycle), c.srv.started)
	if err != nil {
		return data, err
	}
	if note != "" {
		data.Notices = append(data.Notices, note)
	}
	if w.Start.IsZero() {
		data.Notices = append(data.Notices,
			"this harness has not run yet; run `harness logs "+req.Name+" --raw` to read the durable log itself")
		return data, nil
	}
	data.Run = &protocol.LogRun{
		Start:    w.Start.Format(time.RFC3339Nano),
		ExitCode: exit,
		Adapter:  h.Adapter,
		Workdir:  target.Workdir,
	}
	if !w.End.IsZero() {
		data.Run.End = w.End.Format(time.RFC3339Nano)
	}

	env, envErr := supervisor.DiscoveryEnv(h, runtrace.DiscoveryEnvKeys)
	if envErr != nil {
		data.Notices = append(data.Notices, fmt.Sprintf("env_file unreadable, discovering with the daemon's environment: %v", envErr))
	}
	target.Env = env

	ctx, cancel := context.WithTimeout(context.Background(), activityTimeout)
	defer cancel()
	peers, peerNotes := c.peerScopes(target)
	data.Notices = append(data.Notices, peerNotes...)
	att, err := runtrace.Attribute(ctx, target, w, peers, now)
	if err != nil {
		data.Notices = append(data.Notices, err.Error())
	}
	events, parseErrs := runtrace.Events(ctx, att, req.IncludeAmbiguous, now)
	for _, perr := range parseErrs {
		data.Notices = append(data.Notices, perr.Error())
	}
	for _, x := range att.Excluded {
		data.Excluded = append(data.Excluded, protocol.LogExclusion{
			Session:   x.Session.Meta.ID,
			StartedAt: x.Session.Started.Format(time.RFC3339Nano),
			Claimants: x.Claimants,
		})
	}
	if n := len(att.Excluded); n > 0 && !req.IncludeAmbiguous {
		data.Notices = append(data.Notices, fmt.Sprintf(
			"%d session(s) in this run's window not shown: %s share this workdir with overlapping runs, so nothing identifies whose they are (--include-ambiguous shows them)",
			n, strings.Join(otherClaimants(req.Name, att.Excluded), ", ")))
	}

	entries := lifecycleEntries(lifecycle, w, now)
	for _, e := range events {
		entries = append(entries, orderedEntry{at: e.Time, entry: agentEntry(e)})
	}
	sort.SliceStable(entries, func(i, j int) bool { return entries[i].at.Before(entries[j].at) })
	if lines > 0 && len(entries) > lines {
		entries = entries[len(entries)-lines:]
	}
	data.Entries = make([]protocol.LogEntry, 0, len(entries))
	for _, e := range entries {
		data.Entries = append(data.Entries, e.entry)
	}

	if len(events) == 0 {
		// Deliberately no durable-log tail here. The tail of a full-screen
		// agent is its idle screen — splash art, box-drawing chrome, a
		// farewell line — and appending it made `harness logs` look like it
		// was dumping the PTY even though the stored history is line-oriented
		// and escape-free (#279, sanitize.go). The activity view answers "what
		// did this run do"; when nothing is attributable, the honest answer is
		// the notices and where to look next, not a screenshot of the TUI.
		// Governing: #279, and the operator report behind it.
		data.Notices = append(data.Notices,
			"no agent-trace session is attributable to this run; run `harness logs "+req.Name+" --raw` to read the durable log itself")
	}
	return data, nil
}

// runWindow picks the run a structured logs request describes, in order of
// authority:
//
//  1. an explicit Since (and optional Until) on the request — the seam a run
//     selector such as `harness logs <job> --run N` (#120) resolves a run
//     record into;
//  2. the supervisor's own record of the latest run (LastStarted, and
//     LastExitAt when it closes that run);
//  3. the durable log's lifecycle lines, only when the snapshot has no start
//     at all — they share a file with program output, so they never override
//     the supervisor's record.
//
// A closed run's exit code is returned when one was recorded. A run with no
// recorded end that began before daemonStart is closed there (endedBy).
func runWindow(req protocol.ControlReq, snap supervisor.Snapshot, spans []supervisor.RunSpan, daemonStart time.Time) (runtrace.Window, *int, string, error) {
	if req.Since != "" {
		start, err := time.Parse(time.RFC3339Nano, req.Since)
		if err != nil {
			return runtrace.Window{}, nil, "", fmt.Errorf("%w: since %q", errBadWindow, req.Since)
		}
		w := runtrace.Window{Start: start}
		if req.Until != "" {
			end, err := time.Parse(time.RFC3339Nano, req.Until)
			if err != nil {
				return runtrace.Window{}, nil, "", fmt.Errorf("%w: until %q", errBadWindow, req.Until)
			}
			w.End = end
		}
		return w, nil, "", nil
	}
	if w, exit, ok := snapshotWindow(snap); ok {
		if w.Open() && snap.PID == 0 {
			// Not running, yet no exit after the start: the daemon went down
			// with the run. The log may still say when it ended.
			for _, s := range spans {
				if !s.End.IsZero() && absDuration(s.Start.Sub(w.Start)) <= runtrace.Slack {
					w.End, exit = s.End, s.ExitCode
				}
			}
			if closed, ok := endedBy(w, daemonStart); ok {
				return closed, nil, "no exit was recorded for this run; it ended no later than this daemon's start, so its window closes there", nil
			}
			if w.Open() {
				return w, nil, "no exit was recorded for this run; its window is left open", nil
			}
		}
		return w, exit, "", nil
	}
	if len(spans) > 0 {
		last := spans[len(spans)-1]
		w := runtrace.Window{Start: last.Start, End: last.End}
		note := "run window recovered from the durable log's lifecycle lines"
		if closed, ok := endedBy(w, daemonStart); ok {
			w = closed
			note += "; no exit was recorded, so it closes at this daemon's start"
		}
		return w, last.ExitCode, note, nil
	}
	return runtrace.Window{}, nil, "", nil
}

// endedBy closes an open window that began before daemonStart. A harness runs
// under its daemon's PTY and nothing re-adopts it after a restart, so a run
// with no recorded end was over, as far as supervision knows, when this daemon
// started. Left open it would be credited with every session in its workdir
// since — including ones the operator started by hand.
func endedBy(w runtrace.Window, daemonStart time.Time) (runtrace.Window, bool) {
	if !w.Open() || daemonStart.IsZero() || !w.Start.Before(daemonStart) {
		return w, false
	}
	w.End = daemonStart
	return w, true
}

// snapshotWindow is the latest run as the supervisor recorded it.
func snapshotWindow(snap supervisor.Snapshot) (runtrace.Window, *int, bool) {
	if snap.LastStarted.IsZero() {
		return runtrace.Window{}, nil, false
	}
	w := runtrace.Window{Start: snap.LastStarted}
	if snap.PID != 0 {
		return w, nil, true
	}
	if !snap.LastExitAt.IsZero() && !snap.LastExitAt.Before(snap.LastStarted) {
		w.End = snap.LastExitAt
		code := snap.LastExitCode
		return w, &code, true
	}
	return w, nil, true
}

// peerScopes returns every other harness that could have written a session in
// target's workdir, with every run window known for it: the supervisor's latest
// run plus each run the durable log records. A peer whose log cannot be read
// is treated as having always been running — the conservative answer, since
// peers only ever hide sessions.
func (c *conn) peerScopes(target runtrace.Scope) ([]runtrace.Scope, []string) {
	mgr := c.srv.mgr
	var peers []runtrace.Scope
	var notes []string
	// A harness with no workdir is spawned in the daemon's own, and writes its
	// stores there like any other peer.
	daemonDir, _ := os.Getwd()
	for _, snap := range mgr.Snapshots() {
		if snap.Name == target.Name {
			continue
		}
		h, _, ok := mgr.HarnessRecord(snap.Name)
		if !ok {
			continue
		}
		p := runtrace.Scope{Name: snap.Name, Adapter: h.Adapter, Workdir: supervisor.Workdir(h), Args: h.Args}
		if p.Workdir == "" {
			p.Workdir = daemonDir
		}
		if !runtrace.CouldWrite(p.Adapter, target.Adapter) || !runtrace.SameDir(p.Workdir, target.Workdir) {
			continue
		}
		if w, _, ok := snapshotWindow(snap); ok {
			p.Runs = append(p.Runs, w)
		}
		lifecycle, err := supervisor.ReadLifecycle(mgr.LogDir(), snap.Name, time.Time{})
		if err != nil {
			p.Runs = append(p.Runs, runtrace.Window{Start: time.Unix(1, 0)})
			notes = append(notes, fmt.Sprintf("could not read %s's run history, so it is treated as running throughout: %v", snap.Name, err))
		}
		for _, s := range supervisor.RunSpans(lifecycle) {
			p.Runs = append(p.Runs, runtrace.Window{Start: s.Start, End: s.End})
		}
		peers = append(peers, p)
	}
	return peers, notes
}

// orderedEntry pairs an entry with the instant it sorts at.
type orderedEntry struct {
	at    time.Time
	entry protocol.LogEntry
}

// lifecycleEntries converts the lifecycle lines inside w.
//
// Their stamps are truncated to the second, so several lines routinely share
// one, and the only record of their order is the file's. Each line sorts a
// nanosecond after the one before it in the same second, which keeps that order
// through the merge. Shifting "ending" lines to the end of their second instead
// looked right for a lone exit and was wrong for a restart: on tars it printed
// `starting → running` before the `stopping → stopped` it followed.
//
// The offsets start at one nanosecond, not zero, so an agent event stamped at
// the top of the same second — crush stores whole seconds — prints before the
// lifecycle lines in it. That is the order at an exit: the provider error a run
// died on lands in the second of the `exited` it caused, and printed after it.
// At a start there is no tie to break, because a session is not open in the
// second its process spawned.
func lifecycleEntries(lines []supervisor.LifecycleEntry, w runtrace.Window, now time.Time) []orderedEntry {
	var out []orderedEntry
	var second time.Time
	var nth time.Duration
	for _, l := range lines {
		if !w.Covers(l.Time, now) {
			continue
		}
		if l.Time.Equal(second) {
			nth++
		} else {
			second, nth = l.Time, 0
		}
		e := protocol.LogEntry{
			ID:   lifecycleID(l),
			Time: l.Time.Format(time.RFC3339Nano),
			Kind: protocol.LogEntryLifecycle,
		}
		at := l.Time.Add(nth + 1)
		switch l.Msg {
		case "state changed":
			e.Action = "state"
			e.Summary = l.Fields["from"] + " → " + l.Fields["to"]
			e.Error = core.State(l.Fields["to"]) == core.StateFailed
		case "exited":
			e.Action = "exited"
			e.Summary = "code=" + l.Fields["code"]
			code, err := strconv.Atoi(l.Fields["code"])
			e.Error = err != nil || code != 0
		case "flapping":
			e.Action = "flapping"
			e.Summary = "restarts=" + l.Fields["restarts"] + " next_retry_in=" + l.Fields["next_retry_in"]
			e.Error = true
		}
		out = append(out, orderedEntry{at: at, entry: e})
	}
	return out
}

// lifecycleID is stable across reads: two lines can share a second, but not a
// second, a message and the same fields.
func lifecycleID(l supervisor.LifecycleEntry) string {
	keys := make([]string, 0, len(l.Fields))
	for k := range l.Fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	fmt.Fprintf(&b, "lifecycle/%d/%s", l.Time.Unix(), strings.ReplaceAll(l.Msg, " ", "-"))
	for _, k := range keys {
		fmt.Fprintf(&b, "/%s=%s", k, l.Fields[k])
	}
	return b.String()
}

// agentEntry converts one runtrace entry to its wire form.
func agentEntry(e runtrace.Entry) protocol.LogEntry {
	kind := protocol.LogEntryTool
	switch e.Kind {
	case runtrace.KindSession:
		kind = protocol.LogEntrySession
	case runtrace.KindMark:
		kind = protocol.LogEntryMark
	}
	return protocol.LogEntry{
		ID:        e.ID,
		Time:      e.Time.Format(time.RFC3339Nano),
		Kind:      kind,
		Action:    e.Action,
		Tool:      e.Tool,
		Target:    e.Target,
		Summary:   e.Summary,
		Session:   e.Session,
		Error:     e.Error,
		Ambiguous: e.Ambiguous,
	}
}

// otherClaimants names every harness besides self that an exclusion names,
// once each, in order.
func otherClaimants(self string, excluded []runtrace.Exclusion) []string {
	seen := map[string]bool{self: true}
	var out []string
	for _, x := range excluded {
		for _, n := range x.Claimants {
			if !seen[n] {
				seen[n] = true
				out = append(out, n)
			}
		}
	}
	sort.Strings(out)
	return out
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

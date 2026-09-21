package observe

// Scan
//
// One scan: build a correlation scope for every declared harness from the
// Manager's snapshots, list the stores those harnesses write to, attribute each
// session active inside the listing window, and read what each tracked session
// gained since the last scan. Reads are incremental wherever the adapter
// supports it (tail.IncrementalParser): a per-session watermark plus the next
// seq, so a session's items are read — and delivered — at most once, in seq
// order, and never re-read from the start.
//
// What is delivered is bounded twice. By time: nothing timestamped before the
// later of the observer's start and the attributed harness's current run start
// (less runtrace.Slack) — the observer reports live activity, it does not
// replay history. And by baseline: the first read of a session delivers only
// timestamped items past that floor, because an item with no timestamp that is
// already there when a session is first seen cannot be told apart from ancient
// history. Items appearing after that first read are live by definition and are
// delivered with or without a timestamp.
//
// Attribution is asked at the moment of the activity (runtrace.ClaimantAt), not
// at the session's start, because a crush harness resumes one session across
// restarts (issue #347) and the start-time rule would never attribute that
// session again. A contested session is read and dropped, and counted: its
// watermark still advances, so a peer stopping later cannot release a backlog
// the peer may have written.
//
// Governing: issue #390; SPEC-0006 REQ "Run Correlation"; ADR-0007.
//
// @joestump-agent 09/21/2026 - Added for harness#390.
//
// @joestump-agent 09/21/2026 - review: force a summary-cache sweep every
// ForgetAfter so a store that always fails cannot pin the cache; drop a
// session whose transcript was deleted instead of counting a parse error
// every scan; deep-copy target line ranges when redacting.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"time"

	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"

	"github.com/stump-wtf/harness/internal/redact"
	"github.com/stump-wtf/harness/internal/runtrace"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// session is one tracked agent session.
type session struct {
	// id is the tool plus the session id. A store reachable from two sources
	// (the project store, and a registry entry naming it) yields one session
	// under two paths; the id is what makes it one session here, as in
	// runtrace.Attribute.
	id string
	// adapter and path are bound at discovery, so a session is always read
	// through the source that first found it and its watermark keeps meaning.
	adapter tail.Adapter
	path    string
	meta    tail.SessionMeta

	watermark int64 // incremental adapters: agent-trace's opaque resume point
	nextSeq   int   // seq the next read continues from
	marksSeen int   // full-parse adapters: marks already consumed

	// baseline is set until the first successful read, and again when the
	// store is found rewritten under us. A baseline read withholds items with
	// no timestamp.
	baseline bool
	// floor is a per-session history floor on top of the observer's own: the
	// moment a tombstoned session was forgotten, or a rewrite was noticed.
	floor time.Time
	// lastSeen is the latest scan that listed the session or read items
	// from it; ForgetAfter is measured from it.
	lastSeen  time.Time
	contested bool
}

// target is one declared harness as a scan sees it.
type target struct {
	scope runtrace.Scope
	// hasWorkdir is false for a harness spawned in the daemon's directory:
	// a possible author of sessions there, never an attributed one, the same
	// refusal runtrace.Attribute makes (ErrNoWorkdir).
	hasWorkdir bool
	// runStart is the current (latest) run's start, zero if it never ran.
	runStart time.Time
}

// source is one agent-trace store, shared by every harness that writes to it.
type source struct {
	adapter  tail.Adapter
	workdirs []string
}

// item is one tool call or mark, before attribution.
type item struct {
	seq  int
	mark bool
	ts   string
	tool classify.Event
	mk   classify.Mark
}

func (o *Observer) scan(ctx context.Context) {
	now := o.opts.Now()
	o.count(func(s *Stats) { s.LastScan = now })
	scopes, targets, order := o.targets()
	listed := map[string]bool{}
	complete := true
	for _, src := range o.sources(targets, order) {
		if ctx.Err() != nil {
			return
		}
		metas, err := o.list(ctx, src, now)
		if err != nil {
			if ctx.Err() != nil {
				return
			}
			complete = false
			o.count(func(s *Stats) { s.ScanErrors++ })
			o.log.Debug("agent observer: listing failed", "store", src.adapter.SessionDir(), "err", err)
			continue
		}
		for _, m := range metas {
			if ctx.Err() != nil {
				return
			}
			id := string(m.Harness) + "/" + m.ID
			if listed[id] {
				continue
			}
			listed[id] = true
			st := o.sessions[id]
			if st == nil {
				if st = o.discover(src.adapter, m, scopes, now); st == nil {
					continue
				}
			} else {
				// Fresh listing metadata (EndedAt, Title); the path and adapter
				// stay the ones the watermark belongs to.
				m.Path = st.path
				if m.Model == "" {
					m.Model = st.meta.Model
				}
				st.meta = m
			}
			st.lastSeen = now
			o.read(ctx, st, scopes, targets, now)
		}
	}
	// A tracked session missing from this scan's listing is still read. Its
	// store may have stopped reporting activity the listing can see — crush
	// rewrites a streaming row in place without touching the session row — and
	// reading a quiet session costs one query or one stat.
	for id, st := range o.sessions {
		if ctx.Err() != nil {
			return
		}
		if !listed[id] {
			o.read(ctx, st, scopes, targets, now)
		}
	}
	o.forget(now)
	// Normally only after every listing ran: an aborted scan has not looked up
	// every live file, and sweeping would evict entries that are still
	// current. But a store that fails every scan (an unreadable directory)
	// would then block the sweep for the daemon's lifetime, and the cache
	// would keep an entry for every transcript ever summarised. Past
	// ForgetAfter without a sweep, sweep anyway: the cost is re-summarising
	// the live files once, the alternative is unbounded growth. Sweep is
	// mark-then-sweep, so a dead entry goes within two such sweeps.
	if complete || now.Sub(o.lastSweep) > o.opts.ForgetAfter {
		o.summaries.Sweep()
		o.lastSweep = now
	}
	contested := 0
	for _, st := range o.sessions {
		if st.contested {
			contested++
		}
	}
	o.count(func(s *Stats) { s.Sessions, s.Contested = len(o.sessions), contested })
}

// targets builds a scope for every declared harness. Every one of them is a
// peer in attribution, whatever its kind or state: a peer can only ever make a
// session ambiguous, never misattribute it.
func (o *Observer) targets() ([]runtrace.Scope, map[string]*target, []string) {
	snaps := o.src.Snapshots()
	scopes := make([]runtrace.Scope, 0, len(snaps))
	targets := make(map[string]*target, len(snaps))
	order := make([]string, 0, len(snaps))
	for _, snap := range snaps {
		h, _, ok := o.src.HarnessRecord(snap.Name)
		if !ok {
			continue
		}
		t := &target{scope: runtrace.Scope{
			Name:    snap.Name,
			Adapter: h.Adapter,
			Workdir: supervisor.Workdir(h),
			Args:    h.Args,
			Env:     o.opts.DiscoveryEnv(h),
		}}
		t.hasWorkdir = t.scope.Workdir != ""
		if !t.hasWorkdir {
			t.scope.Workdir = o.opts.DaemonDir
		}
		if w, ok := runWindow(snap, o.opts.Since); ok {
			t.scope.Runs = []runtrace.Window{w}
			// Only the latest run is known here, so everything before it is
			// history this scope does not hold — the TUI's view, and the
			// reason a restarted sibling still counts as a possible author of
			// anything older than its restart.
			t.scope.KnownSince = w.Start
			t.runStart = w.Start
		} else if !snap.LastExitAt.IsZero() {
			t.scope.KnownSince = snap.LastExitAt
		}
		scopes = append(scopes, t.scope)
		targets[snap.Name] = t
		order = append(order, snap.Name)
	}
	return scopes, targets, order
}

// runWindow is a harness's latest run as the supervisor records it, closed the
// way the daemon's `harness logs` closes it: at the recorded exit, or — for a
// run restored from state.json with no exit after its start — at the observer's
// start, because nothing re-adopts a harness across a daemon restart.
func runWindow(snap supervisor.Snapshot, since time.Time) (runtrace.Window, bool) {
	if snap.LastStarted.IsZero() {
		return runtrace.Window{}, false
	}
	w := runtrace.Window{Start: snap.LastStarted}
	if snap.PID != 0 {
		return w, true
	}
	if !snap.LastExitAt.IsZero() && !snap.LastExitAt.Before(snap.LastStarted) {
		w.End = snap.LastExitAt
		return w, true
	}
	w.End = snap.LastStarted
	if snap.LastStarted.Before(since) {
		w.End = since
	}
	return w, true
}

// sources resolves and dedupes the stores every declared harness with a
// workdir writes to, in config order.
func (o *Observer) sources(targets map[string]*target, order []string) []*source {
	var out []*source
	byKey := map[string]*source{}
	for _, name := range order {
		t := targets[name]
		if !t.hasWorkdir {
			continue
		}
		adapters, err := o.opts.Sources(t.scope)
		if err != nil {
			continue // no native trajectory (generic): nothing to observe
		}
		work := filepath.Clean(t.scope.Workdir)
		for _, a := range adapters {
			key := sourceKey(a)
			src := byKey[key]
			if src == nil {
				if sc, ok := a.(tail.SummaryCacheSetter); ok {
					sc.SetSummaryCache(o.summaries)
				}
				src = &source{adapter: a}
				byKey[key] = src
				out = append(out, src)
			}
			if !containsString(src.workdirs, work) {
				src.workdirs = append(src.workdirs, work)
			}
		}
	}
	return out
}

// list returns the sessions of one store active inside the listing window
// whose working directory is one a harness writing there runs in.
func (o *Observer) list(ctx context.Context, src *source, now time.Time) ([]tail.SessionMeta, error) {
	active := now.Add(-o.opts.ForgetAfter)
	if floor := o.opts.Since.Add(-runtrace.Slack); floor.After(active) {
		active = floor
	}
	f := tail.SessionFilter{ActiveSince: active}
	if len(src.workdirs) == 1 {
		f.Cwd = src.workdirs[0] // a pushdown; the exact check is below
	}
	lctx, cancel := context.WithTimeout(ctx, o.opts.SourceTimeout)
	defer cancel()
	metas, err := tail.ListSessionsFiltered(lctx, src.adapter, f)
	if err != nil {
		return nil, err
	}
	out := metas[:0]
	for _, m := range metas {
		for _, w := range src.workdirs {
			if runtrace.SameDir(m.Cwd, w) {
				out = append(out, m)
				break
			}
		}
	}
	return out, nil
}

// discover starts tracking a session some harness could have written. One
// nobody could have written is not tracked at all: there is nothing to deliver,
// and should a harness start writing into it later, that harness's run start
// is the floor that keeps what came before from replaying.
func (o *Observer) discover(a tail.Adapter, m tail.SessionMeta, scopes []runtrace.Scope, now time.Time) *session {
	at, ok := m.Ended()
	if !ok {
		at = now
	}
	name, cands := runtrace.ClaimantAt(m, at, scopes, now)
	if name == "" && len(cands) < 2 {
		return nil
	}
	st := &session{
		id:       string(m.Harness) + "/" + m.ID,
		adapter:  a,
		path:     m.Path,
		meta:     m,
		baseline: true,
	}
	if t, ok := o.tombstones[st.id]; ok {
		st.floor = t
		delete(o.tombstones, st.id)
	}
	o.sessions[st.id] = st
	return st
}

// read consumes whatever st gained since its last read and delivers the part
// of it that is attributable and live.
func (o *Observer) read(ctx context.Context, st *session, scopes []runtrace.Scope, targets map[string]*target, now time.Time) {
	rctx, cancel := context.WithTimeout(ctx, o.opts.SourceTimeout)
	defer cancel()
	baseline := st.baseline
	items, rewritten, err := o.fetch(rctx, st)
	if err != nil {
		if ctx.Err() != nil {
			return // shutting down, not a parse failure
		}
		if gone(err) {
			// The transcript was deleted (a removed JSONL, a session deleted
			// in crush). That is not a broken collector: counting it would
			// raise ParseErrors every scan until ForgetAfter, which a
			// consumer reports as collection errors (SPEC-0013 REQ-6). Forget
			// it now, with a tombstone, so a reappearance cannot replay.
			o.log.Debug("agent observer: session gone", "session", st.id)
			o.tombstones[st.id] = now
			delete(o.sessions, st.id)
			return
		}
		o.count(func(s *Stats) { s.ParseErrors[string(st.meta.Harness)]++ })
		o.log.Debug("agent observer: read failed", "session", st.id, "err", err)
		return
	}
	if rewritten {
		// The store shrank or was replaced: start over from the top as a
		// baseline, delivering nothing already there.
		st.baseline, st.floor = true, now
		return
	}
	st.baseline = false
	if len(items) == 0 {
		return
	}
	st.lastSeen = now

	// What could be live at all, whoever wrote it: past the observer's own
	// floor and this session's, and — on a baseline read — dated. Only these
	// count toward Ambiguous and Unattributed; history was never going to be
	// delivered, and counting it would make every first sight of a contested
	// session look like a burst of withheld work.
	floor := o.opts.Since.Add(-runtrace.Slack)
	if st.floor.After(floor) {
		floor = st.floor
	}
	type live struct {
		item
		when time.Time
	}
	var cands []live
	var at time.Time // the activity's instant, for attribution
	for _, it := range items {
		when, ok := parseTime(it.ts)
		switch {
		case !ok && baseline:
			continue
		case !ok:
			when = now // not there at the last read, so it is from since then
		case when.Before(floor):
			continue
		}
		if when.After(at) {
			at = when
		}
		cands = append(cands, live{it, when})
	}
	if len(cands) == 0 {
		return
	}

	name, claimants := runtrace.ClaimantAt(st.meta, at, scopes, now)
	st.contested = len(claimants) > 1
	t := targets[name]
	if name == "" || t == nil || !t.hasWorkdir {
		n := uint64(len(cands))
		if st.contested {
			o.count(func(s *Stats) { s.Ambiguous += n })
		} else {
			o.count(func(s *Stats) { s.Unattributed += n })
		}
		return
	}

	// The attributed harness's current run is the last floor: what it wrote
	// before this run was reported live then, or predates this daemon.
	cutoff := floor
	if run := t.runStart.Add(-runtrace.Slack); run.After(cutoff) {
		cutoff = run
	}
	meta := st.meta
	meta.Title = redact.String(meta.Title)
	events := make([]Event, 0, len(cands))
	for _, c := range cands {
		if c.when.Before(cutoff) {
			continue
		}
		ev := Event{Harness: name, Adapter: t.scope.Adapter, Session: meta, Time: c.when, ObservedAt: now}
		if c.mark {
			ev.Kind, ev.Mark = KindMark, redactMark(c.mk)
		} else {
			ev.Kind, ev.Tool = KindTool, redactTool(c.tool)
		}
		events = append(events, ev)
	}
	o.publish(events)
}

// fetch reads what st gained since the last successful fetch, in seq order.
// rewritten reports a store that moved backwards (a truncated or replaced
// file); st is then reset to read from the top.
func (o *Observer) fetch(ctx context.Context, st *session) ([]item, bool, error) {
	if ip, ok := st.adapter.(tail.IncrementalParser); ok {
		// ParseSince's watermark never lands inside an open tool call: it
		// withholds everything past the last point with no call outstanding,
		// and returns that point. Storing only what it returns is what makes
		// the stream complete and duplicate-free.
		events, marks, meta, wm, err := ip.ParseSince(ctx, st.path, st.watermark, st.nextSeq)
		if err != nil {
			return nil, false, err
		}
		if wm < st.watermark {
			st.watermark = 0
			return nil, true, nil
		}
		st.watermark = wm
		st.nextSeq += len(events)
		if meta.Model != "" {
			st.meta.Model = meta.Model
		}
		return merge(events, marks), false, nil
	}
	// No incremental support: re-parse and take what lies past what was
	// already consumed. Every adapter agent-trace ships is incremental; this
	// keeps a future one correct, if not cheap.
	events, marks, meta, err := st.adapter.Parse(ctx, st.path)
	if err != nil {
		return nil, false, err
	}
	if len(marks) < st.marksSeen || (len(events) > 0 && events[len(events)-1].Seq+1 < st.nextSeq) {
		st.nextSeq, st.marksSeen = 0, 0
		return nil, true, nil
	}
	var fresh []classify.Event
	for _, ev := range events {
		if ev.Seq >= st.nextSeq {
			fresh = append(fresh, ev)
			st.nextSeq = ev.Seq + 1
		}
	}
	freshMarks := marks[st.marksSeen:]
	st.marksSeen = len(marks)
	if meta.Model != "" {
		st.meta.Model = meta.Model
	}
	return merge(fresh, freshMarks), false, nil
}

// merge interleaves events and marks by seq. A mark carries the seq of the
// tool call that follows it, so at a tie the mark goes first — the order
// runtrace.Events and agent-trace's trace builder use.
func merge(events []classify.Event, marks []classify.Mark) []item {
	out := make([]item, 0, len(events)+len(marks))
	for _, m := range marks {
		out = append(out, item{seq: m.Seq, mark: true, ts: m.Timestamp, mk: m})
	}
	for _, e := range events {
		out = append(out, item{seq: e.Seq, ts: e.Timestamp, tool: e})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].seq != out[j].seq {
			return out[i].seq < out[j].seq
		}
		return out[i].mark && !out[j].mark
	})
	return out
}

// forget drops sessions unchanged for ForgetAfter, leaving a tombstone that
// keeps a session which wakes up again from replaying what it already
// delivered, and caps the tombstones.
func (o *Observer) forget(now time.Time) {
	for id, st := range o.sessions {
		if now.Sub(st.lastSeen) > o.opts.ForgetAfter {
			o.tombstones[id] = now
			delete(o.sessions, id)
		}
	}
	if excess := len(o.tombstones) - o.opts.TombstoneLimit; excess > 0 {
		type tomb struct {
			id string
			at time.Time
		}
		all := make([]tomb, 0, len(o.tombstones))
		for id, at := range o.tombstones {
			all = append(all, tomb{id, at})
		}
		sort.Slice(all, func(i, j int) bool { return all[i].at.Before(all[j].at) })
		for _, t := range all[:excess] {
			delete(o.tombstones, t.id)
		}
	}
}

// redactTool masks credentials in every string a tool event carries from the
// transcript (ADR-0008). Slices are copied: the event reaches several
// subscribers and none of them may see another's edits.
func redactTool(ev classify.Event) classify.Event {
	ev.Summary = redact.String(ev.Summary)
	if len(ev.Targets) > 0 {
		ts := make([]classify.Target, len(ev.Targets))
		copy(ts, ev.Targets)
		for i := range ts {
			ts[i].Path = redact.String(ts[i].Path)
		}
		ev.Targets = ts
	}
	if len(ev.Outside) > 0 {
		out := make([]classify.OutsideTouch, len(ev.Outside))
		copy(out, ev.Outside)
		for i := range out {
			out[i].Path = redact.String(out[i].Path)
		}
		ev.Outside = out
	}
	return ev
}

// redactMark masks credentials in a mark's note — a user message verbatim, or
// a provider error that may echo the request.
func redactMark(m classify.Mark) classify.Mark {
	m.Note = redact.String(m.Note)
	return m
}

// gone reports a read that failed because the session no longer exists,
// rather than because it could not be parsed.
func gone(err error) bool {
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, sql.ErrNoRows)
}

// parseTime parses an agent-trace timestamp (RFC 3339, nanoseconds optional).
func parseTime(ts string) (time.Time, bool) {
	if ts == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, ts); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// sourceKey identifies a store. Sources builds fresh adapter values every
// scan, and two harnesses in one workdir build identical ones; the adapter
// type plus where it reads is what makes them the same store. A crush project
// store also carries the working directory classify resolves paths against.
func sourceKey(a tail.Adapter) string {
	key := fmt.Sprintf("%T|%s", a, a.SessionDir())
	if c, ok := a.(*tail.CrushAdapter); ok {
		key += "|" + c.DBPath + "|" + c.ProjectsPath + "|" + c.Cwd
	}
	return key
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

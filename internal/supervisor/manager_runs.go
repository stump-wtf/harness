package supervisor

// Run History Store
//
// The Manager's half of run history (runs.go is the supervisor's). It hands out
// run ids, and is the single writer of the run ledger (internal/ledger): every
// record opens, closes or is decided through the RunJournal methods here, and
// each of those lines is synced before the method returns. keep_runs bounds the
// per-run log files only; records stay in the ledger, and read log_pruned once
// their log is gone. state.json keeps nothing of run history but each harness's
// last run id.
//
// Run ids never repeat. The first allocation of each process floors the last id
// at the highest log file on disk and at the highest run id the ledger holds,
// so neither a lost state file nor a harness that briefly left the config can
// reissue an id.
//
// At boot (bootLedger) a pre-ledger state.json's history is imported once, and
// a record still open belonged to a daemon that died under it: it is closed
// interrupted, reason daemon_crash, with ended_at unset because the real end is
// unknown, and a line saying so is appended to its log.
//
// Governing: ADR-0007, ADR-0008, ADR-0013, ADR-0028; SPEC-0008 REQ "Run
// History", REQ "Per-Run Logs"; SPEC-0022 REQ-3, REQ-6, REQ-7, REQ-13.
//
// @joestump-agent 09/11/2026 - Added for issue #119.
//
// @joestump-agent 09/11/2026 - Review of PR #310: run log paths go through
// runLogDir, so a name that is not a single path element (a project harness,
// or a name from the protocol in #120) never writes or prunes outside jobs/.
//
// @joestump 09/24/2026 - Records moved from state.json to the run ledger
// (harness#444); keep_runs now bounds logs only.

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"charm.land/log/v2"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/ledger"
)

// runHistory is one harness's run id allocator: the only part of run history
// state.json still carries (SPEC-0022 REQ-13). The records live in the ledger.
type runHistory struct {
	LastRunID int `json:"last_run_id"`
	// floored is set once LastRunID has been checked against the log files on
	// disk and the ledger in this process. Not persisted.
	floored bool
}

// legacyHistory is a history as a daemon before the ledger wrote it: the id
// allocator and the record list. It is read once, for the first-boot import,
// and never written.
type legacyHistory struct {
	LastRunID int         `json:"last_run_id"`
	Runs      []RunRecord `json:"runs,omitempty"`
}

// runRef names one run.
type runRef struct {
	name string
	id   int
	log  string
}

// OpenRun implements RunJournal: it allocates the next run id and appends the
// record's `opened` line, synced, before returning, so the line is on disk
// before the process the caller is about to spawn (SPEC-0022 REQ-6).
//
// A resident's record names its durable log, which the caller sets, and it
// gets no per-run log: the returned path is empty (SPEC-0022 REQ-4).
func (m *Manager) OpenRun(name string, rec RunRecord) (RunRecord, string, error) {
	rec, err := m.appendNew(name, ledger.TypeOpened, rec)
	if rec.Kind == KindResident {
		return rec, "", err
	}
	m.pruneRunLogs(name, rec.RunID)
	return rec, rec.Log, err
}

// AppendRun implements RunJournal: it allocates the next run id and appends a
// `decided` line carrying the whole record, synced.
func (m *Manager) AppendRun(name string, rec RunRecord) (RunRecord, error) {
	return m.appendNew(name, ledger.TypeDecided, rec)
}

// appendNew allocates name's next run id and appends rec's first line under
// it. The allocation and the enqueue happen under one lock, so a harness's
// ids and its lines' seqs increase together; the sync is waited for outside
// it, so one harness's fsync never delays another's allocation.
//
// The allocator is saved by the debounced persist loop. That is enough: the id
// is durable in this line, and boot floors the allocator at the ledger.
func (m *Manager) appendNew(name string, typ ledger.Type, rec RunRecord) (RunRecord, error) {
	m.journalMu.Lock()
	m.mu.Lock()
	h := m.historyLocked(name)
	h.LastRunID++
	rec.RunID = h.LastRunID
	m.mu.Unlock()
	if typ == ledger.TypeOpened && rec.Kind != KindResident {
		rec.Log = m.RunLogPath(name, rec.RunID)
	}
	_, wait, err := m.ledger.Enqueue(ledger.Line{
		Type: typ, At: rec.StartedAt, Harness: name, RunID: rec.RunID, Record: toLedger(rec),
	}, true)
	m.journalMu.Unlock()
	m.markDirty()
	if err != nil {
		return rec, err
	}
	return rec, wait()
}

// CoalesceRun implements RunJournal: it counts one more firing on name's
// skipped record with this id, with an `updated` line, and returns the record
// as it now reads.
//
// It emits NO lifecycle event, by design (SPEC-0014 REQ "Overlap Skip
// Coalescing"): the first skip already announced itself, and 199 more
// announcements are exactly the noise coalescing exists to remove.
//
// A missing record is errNoRunToCoalesce rather than a silent no-op, because
// the caller uses that answer to decide whether to open a fresh record —
// swallowing it would lose the skip entirely. Any other error is a failed
// append: the increment HAS been queued, and the returned record carries it.
func (m *Manager) CoalesceRun(name string, id int) (RunRecord, error) {
	// Pending, not Get: the previous increment may still be queued, and
	// counting from the committed value would drop it.
	f, ok, err := m.ledger.Pending(name, id)
	if err != nil || !ok {
		return RunRecord{}, fmt.Errorf("%w: run %d of %q", errors.Join(errNoRunToCoalesce, err), id, name)
	}
	n := max(f.Coalesced, 1) + 1
	f.Coalesced = n
	// Buffered: a count is a checkpoint, not a fact a crash must not lose,
	// and a burst of 200 firings should not cost 200 fsyncs.
	_, err = m.ledger.Append(ledger.Line{
		Type: ledger.TypeUpdated, Harness: name, RunID: id, Record: ledger.Record{Coalesced: n},
	}, false)
	return fromLedger(f), err
}

// CloseRun implements RunJournal: it appends the record's `closed` line,
// synced, before returning, so nothing publishes an outcome the ledger does not
// hold (SPEC-0022 REQ-6).
func (m *Manager) CloseRun(name string, rec RunRecord) error {
	fields := ledger.Record{
		Outcome:     string(rec.Outcome),
		Reason:      string(rec.Reason),
		MissingPath: rec.MissingPath,
		EndedAt:     rec.EndedAt,
		ExitCode:    rec.ExitCode,
	}
	at := time.Now()
	if rec.EndedAt != nil {
		at = *rec.EndedAt
		fields.DurationMs = rec.EndedAt.Sub(rec.StartedAt).Milliseconds()
	}
	_, err := m.ledger.Append(ledger.Line{
		Type: ledger.TypeClosed, At: at, Harness: name, RunID: rec.RunID, Record: fields,
	}, true)
	return err
}

// StartRun is a scheduled firing for name (SPEC-0008 REQ "Overlap Policy").
// It returns false for an unknown harness.
//
// A firing that lands while the harness is mid graceful stop is recorded
// skipped here rather than sent to the loop: the loop would only see it once
// the stop completed, and would then start a harness an operator just stopped
// (SPEC-0008 REQ "Firing And Overlap").
func (m *Manager) StartRun(name string, req RunRequest) (RunDecision, bool) {
	s := m.get(name)
	if s == nil {
		return RunDecision{}, false
	}
	if s.Snapshot().State == core.StateStopping {
		// Recorded here rather than sent to the loop, and so NOT coalesced:
		// the loop's open-skip map is keyed to the run in flight, and there
		// is no run in flight during a stop. A burst arriving mid-stop is
		// bounded by the stop grace, which is seconds — unlike a burst during
		// a long run, which is what coalescing exists for.
		// Governing: SPEC-0014 REQ "Firing", REQ "Run Record Fields".
		rec := decisionRecord(req, OutcomeSkipped, time.Now())
		rec.Reason = ReasonStopping
		rec.Coalesced = 1
		rec, _ = m.AppendRun(name, rec)
		m.publishRun(EventRunFinished, name, rec)
		return RunDecision{Kind: DecisionSkipped, Run: rec}, true
	}
	d := s.StartRun(req)
	m.clearDormant(name)
	return d, true
}

// publishRun announces a run record the Manager made itself on the lifecycle
// bus (SPEC-0008 REQ "Lifecycle Events").
func (m *Manager) publishRun(kind EventKind, name string, rec RunRecord) {
	m.bus.Publish(Event{Kind: kind, Name: name, Time: time.Now(), Run: rec})
}

// PublishScheduleChanged announces a scheduled harness's new next window on the
// lifecycle bus (SPEC-0008 REQ "Lifecycle Events"); a zero next means it has
// none. The scheduler reports these and the Manager owns the bus, so the daemon
// wires this in as the scheduler's NextChanged.
func (m *Manager) PublishScheduleChanged(name string, next time.Time) {
	m.bus.Publish(Event{Kind: EventScheduleChanged, Name: name, Time: time.Now(), NextRun: next})
}

// PublishHoursChanged announces a gated harness's in_hours flip on the
// lifecycle bus (SPEC-0012 REQ "Operating Hours Visibility"); next is zero
// when the expression has no next flip (it covers the entire week). The
// scheduler's gate pass detects the flip — comparing each tick's evaluation
// to its own last-observed value — and the Manager owns the bus, so the
// daemon wires this in as the scheduler's HoursChanged, the same way
// PublishScheduleChanged is wired to NextChanged.
func (m *Manager) PublishHoursChanged(name string, inHours bool, next time.Time) {
	m.bus.Publish(Event{Kind: EventHoursChanged, Name: name, Time: time.Now(), InHours: inHours, HoursNext: next})
}

// LogLifecycle writes msg as a lifecycle line to name's durable log (ADR-0007)
// with the given key/value pairs, via Supervisor.LogEvent — on the actor
// loop, exactly like hold/release/closeStep's own lines. It is the seam for a
// lifecycle event decided outside the loop itself — an after-hours lease
// start/end, decided by StartFor's caller and by the gate pass's Lease query
// — to land in the same durable record those write to (SPEC-0012 REQ
// "Operating Hours Visibility"). ok is false for an unknown harness.
func (m *Manager) LogLifecycle(name, msg string, kv ...any) bool {
	s := m.get(name)
	if s == nil {
		return false
	}
	s.LogEvent(msg, kv...)
	return true
}

// ConsecutiveFailures counts runs, newest first, whose outcome is a failure
// (RunOutcome.Verdict: failed, timed_out, budget_exceeded, model_mismatch,
// model_unattested), back to the latest success. Records that pass no verdict
// on the harness — running, skipped, missed, replaced, cancelled, interrupted,
// quota_parked — neither count nor break the streak: an operator stop or a
// provider quota says nothing about whether the job works (SPEC-0022 REQ-5).
func ConsecutiveFailures(runs []RunRecord) int {
	n := 0
	for i := len(runs) - 1; i >= 0; i-- {
		switch runs[i].Outcome.Verdict() {
		case 1:
			return n
		case -1:
			n++
		}
	}
	return n
}

// RecordMissed records schedule windows that elapsed while nobody was
// evaluating them, for a harness with catch_up = false: the scheduler's missed
// window seam (SPEC-0008 REQ "Missed Window Handling").
func (m *Manager) RecordMissed(name string, first, last time.Time, windows int, detectedAt time.Time) error {
	if m.get(name) == nil {
		return fmt.Errorf("supervisor: record missed windows: unknown harness %q", name)
	}
	rec := RunRecord{
		Trigger:     TriggerSchedule,
		Outcome:     OutcomeMissed,
		StartedAt:   detectedAt,
		EndedAt:     &detectedAt,
		Window:      &last,
		FirstWindow: &first,
		Windows:     windows,
	}
	rec, err := m.AppendRun(name, rec)
	m.publishRun(EventRunFinished, name, rec)
	return err
}

// Runs returns name's run records from the ledger's memory, oldest first: the
// last seven days of them, and at least the newest twenty. Each record's
// StartedAt and EndedAt bound one run exactly, which is what run correlation
// needs to scope an agent transcript to a single run.
func (m *Manager) Runs(name string) []RunRecord {
	recs := m.ledger.Records(name)
	out := make([]RunRecord, len(recs))
	for i, f := range recs {
		out[i] = fromLedger(f)
	}
	return out
}

// LatestRuns returns name's newest limit records, newest first, reading the
// ledger's day files when memory does not reach back far enough (SPEC-0022
// REQ-15).
func (m *Manager) LatestRuns(name string, limit int) ([]RunRecord, error) {
	recs, _, err := m.ledger.Query(ledger.Query{Names: []string{name}, Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]RunRecord, len(recs))
	for i, f := range recs {
		out[i] = fromLedger(f)
	}
	return out, nil
}

// Run returns one of name's run records.
func (m *Manager) Run(name string, id int) (RunRecord, bool) {
	f, ok, err := m.ledger.Get(name, id)
	if err != nil || !ok {
		return RunRecord{}, false
	}
	return fromLedger(f), true
}

// Ledger is the run ledger the Manager writes (SPEC-0022), for the readers
// that need more than one harness's records: the runs op's query, the metrics
// feed, doctor.
func (m *Manager) Ledger() *ledger.Ledger { return m.ledger }

// RunLogPath is where run id of name logs: <jobs dir>/<name>/<id>.log, or ""
// when name cannot be a directory of its own (runLogDir).
func (m *Manager) RunLogPath(name string, id int) string {
	dir := m.runLogDir(name)
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, strconv.Itoa(id)+".log")
}

// runLogDir is name's directory under the jobs root, or "" for a name that is
// not a single path element. The config parser already refuses "/" and dot
// names, but this is where a name becomes a file write and a delete, so the
// guard lives here too: a project harness ("proj/name"), or a name arriving
// over the protocol, must never address a directory outside its own.
func (m *Manager) runLogDir(name string) string {
	if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
		return ""
	}
	return filepath.Join(m.jobsDir, name)
}

// historyLocked returns name's allocator, creating it, and floors its last id
// the first time it is used in this process: at the highest log file on disk,
// and at the highest run id the ledger holds for name. Caller holds mu.
func (m *Manager) historyLocked(name string) *runHistory {
	h := m.runs[name]
	if h == nil {
		h = &runHistory{}
		m.runs[name] = h
	}
	if !h.floored {
		h.floored = true
		h.LastRunID = max(h.LastRunID, highestRunLog(m.runLogDir(name)), m.ledger.MaxRunID(name))
	}
	return h
}

// keepRuns is name's per-run log bound.
func (m *Manager) keepRuns(name string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	if h, ok := m.cfg.Harnesses[name]; ok && h.KeepRuns > 0 {
		return h.KeepRuns
	}
	return core.DefaultKeepRuns
}

// pruneRunLogs keeps the artifacts of name's newest keep_runs runs and
// deletes the rest. keep_runs bounds per-run logs only; the records stay in the
// ledger, and read log_pruned once their log is gone (SPEC-0022 REQ-12, REQ-13).
//
// "Artifacts" is both the run's log and its event file (SPEC-0014 REQ "Event
// Delivery To The Run": the event file is pruned together with the log).
// Pruning only the log would leave every event file on disk forever, which is
// the worse half to keep: a webhook body is attacker-supplied text, and a
// directory of them accumulating is exactly what `keep_runs` exists to stop.
//
// Only ids up to the allocator's current value are considered, and a run the
// ledger holds open is never pruned: its log is still being written. opening
// is the run whose record was just opened; its log is created next, and it
// counts toward keep_runs already.
func (m *Manager) pruneRunLogs(name string, opening int) {
	dir := m.runLogDir(name)
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	m.mu.Lock()
	upTo := 0
	if h := m.runs[name]; h != nil {
		upTo = h.LastRunID
	}
	m.mu.Unlock()
	keep := m.keepRuns(name)
	open := map[int]bool{}
	for _, f := range m.ledger.OpenRecords() {
		if f.Harness == name {
			open[f.RunID] = true
		}
	}
	// The run being opened has no log yet, but it is the newest and counts.
	ids := []int{opening}
	for _, e := range entries {
		if id, ok := runArtifactID(e.Name()); ok && id <= upTo && !slices.Contains(ids, id) {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	drop := map[int]bool{}
	for i, id := range ids {
		if len(ids)-i > keep && !open[id] {
			drop[id] = true
		}
	}
	for _, e := range entries {
		if id, ok := runArtifactID(e.Name()); ok && drop[id] {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// highestRunLog is the largest run id with a log file in dir, or 0.
func highestRunLog(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	highest := 0
	for _, e := range entries {
		if id, ok := runLogID(e.Name()); ok && id > highest {
			highest = id
		}
	}
	return highest
}

// runLogID parses "<id>.log". It is deliberately narrower than
// runArtifactID: highestRunLog floors the next run id from it, and a run id
// must be floored by a run that actually produced a log.
func runLogID(file string) (int, bool) {
	return runIDWithSuffix(file, ".log")
}

// runArtifactID parses any of a run's on-disk artifacts — "<id>.log" or
// "<id>.event.json" — into its run id. Pruning walks this, so a new artifact
// kind is one line here rather than a second loop that can forget one.
func runArtifactID(file string) (int, bool) {
	if id, ok := runIDWithSuffix(file, ".log"); ok {
		return id, true
	}
	return runIDWithSuffix(file, ".event.json")
}

func runIDWithSuffix(file, suffix string) (int, bool) {
	base, ok := strings.CutSuffix(file, suffix)
	if !ok {
		return 0, false
	}
	id, err := strconv.Atoi(base)
	if err != nil || id < 1 {
		return 0, false
	}
	return id, true
}

// restoreRunsLocked loads each harness's run id allocator, and keeps any
// record list a pre-ledger daemon left for the first-boot import. A malformed
// entry costs only that harness its import — never the rest of state.json, and
// never an id, because of the log and ledger floors. Caller holds mu.
func (m *Manager) restoreRunsLocked(raw json.RawMessage) {
	if len(raw) == 0 {
		return
	}
	var byName map[string]json.RawMessage
	if err := json.Unmarshal(raw, &byName); err != nil {
		return
	}
	for name, body := range byName {
		var h legacyHistory
		if err := json.Unmarshal(body, &h); err != nil {
			continue
		}
		m.runs[name] = &runHistory{LastRunID: h.LastRunID}
		if len(h.Runs) > 0 {
			m.legacyRuns[name] = h.Runs
		}
	}
}

// bootLedger brings the ledger up to date before any start is admitted:
//
//  1. the first-boot import copies state.json's record lists in, once
//     (SPEC-0022 REQ-13), after which the next save drops them;
//  2. backfill reads far enough back for every harness's newest records, so a
//     run opened before the in-memory window is still found;
//  3. reconciliation closes every record a dead daemon left open as
//     interrupted, reason daemon_crash, with no end, and says so in its log
//     (REQ-7).
//
// Errors are logged, not returned: a daemon that cannot write its ledger must
// still supervise (REQ-6), and the failure is counted where doctor reads it.
func (m *Manager) bootLedger() {
	m.mu.Lock()
	legacy := m.legacyRuns
	names := slices.Collect(maps.Keys(m.supervisors))
	for name := range legacy {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	m.mu.Unlock()

	if !m.ledger.Imported() {
		var lines []ledger.Line
		n := 0
		for name, runs := range legacy {
			for _, r := range runs {
				lines = append(lines, m.importLines(name, r)...)
				n++
			}
		}
		if err := m.ledger.Import(lines); err != nil {
			log.Error("run history import failed; state.json keeps it for the next boot", "records", n, "err", err)
		} else {
			if n > 0 {
				log.Info("imported run history from state.json into the ledger", "records", n, "dir", m.ledger.Dir())
			}
			m.mu.Lock()
			m.legacyRuns = map[string][]RunRecord{}
			m.mu.Unlock()
			m.markDirty()
		}
	} else if len(legacy) > 0 {
		// The import ran in an earlier boot that died before its save. The
		// records are in the ledger; the list is only waiting to be dropped.
		m.mu.Lock()
		m.legacyRuns = map[string][]RunRecord{}
		m.mu.Unlock()
		m.markDirty()
	}

	if err := m.ledger.Backfill(names); err != nil {
		log.Warn("run ledger backfill incomplete", "err", err)
	}

	var interrupted []runRef
	for _, f := range m.ledger.OpenRecords() {
		_, err := m.ledger.Append(ledger.Line{
			Type: ledger.TypeClosed, Harness: f.Harness, RunID: f.RunID,
			Record: ledger.Record{Outcome: string(OutcomeInterrupted), Reason: string(ReasonDaemonCrash)},
		}, true)
		if err != nil {
			log.Error("could not close a run the last daemon left open", "harness", f.Harness, "run_id", f.RunID, "err", err)
		}
		interrupted = append(interrupted, runRef{name: f.Harness, id: f.RunID, log: f.Log})
	}
	m.noteInterrupted(interrupted)
}

// importLines turns one state.json record into ledger lines: a `decided` line
// for a decision that started no process, else an `opened` line and, for a run
// that had ended, a `closed` one. A record still running was left by a daemon
// that died under it; it is imported open, and reconciliation closes it.
func (m *Manager) importLines(name string, r RunRecord) []ledger.Line {
	rec := toLedger(r)
	rec.Imported = true
	switch r.Outcome {
	case OutcomeSkipped, OutcomeMissed:
		return []ledger.Line{{Type: ledger.TypeDecided, At: r.StartedAt, Harness: name, RunID: r.RunID, Record: rec}}
	}
	rec.Log = m.RunLogPath(name, r.RunID)
	open := rec
	open.Outcome, open.EndedAt, open.ExitCode, open.Reason = string(OutcomeRunning), nil, nil, ""
	lines := []ledger.Line{{Type: ledger.TypeOpened, At: r.StartedAt, Harness: name, RunID: r.RunID, Record: open}}
	if r.Outcome == OutcomeRunning {
		return lines
	}
	end := ledger.Record{Outcome: rec.Outcome, Reason: rec.Reason, EndedAt: rec.EndedAt, ExitCode: rec.ExitCode, Imported: true}
	at := r.StartedAt
	if r.EndedAt != nil {
		at = *r.EndedAt
		end.DurationMs = r.EndedAt.Sub(r.StartedAt).Milliseconds()
	}
	return append(lines, ledger.Line{Type: ledger.TypeClosed, At: at, Harness: name, RunID: r.RunID, Record: end})
}

// closeOpenRuns closes, as interrupted by a shutdown, every record still open
// once the supervisors have stopped (SPEC-0022 REQ-7). Each supervisor closes
// its own run on the way down; this is the net under a record no supervisor
// owns any more.
func (m *Manager) closeOpenRuns() {
	now := time.Now()
	for _, f := range m.ledger.OpenRecords() {
		rec := ledger.Record{Outcome: string(OutcomeInterrupted), Reason: string(ReasonShutdown), EndedAt: &now}
		if f.StartedAt != nil {
			rec.DurationMs = now.Sub(*f.StartedAt).Milliseconds()
		}
		if _, err := m.ledger.Append(ledger.Line{Type: ledger.TypeClosed, At: now, Harness: f.Harness, RunID: f.RunID, Record: rec}, true); err != nil {
			log.Error("could not close a run at shutdown", "harness", f.Harness, "run_id", f.RunID, "err", err)
		}
	}
}

// noteInterrupted appends the reconciliation to each interrupted run's log, so
// the log does not simply stop mid-run with no explanation. Only a per-run
// log is written to; a resident's record names its durable log, which is the
// supervisor's to write.
func (m *Manager) noteInterrupted(runs []runRef) {
	for _, r := range runs {
		path := m.RunLogPath(r.name, r.id)
		if path == "" || (r.log != "" && r.log != path) {
			continue
		}
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			continue
		}
		newEventLogger(f).Warn("run interrupted", "run_id", r.id, "reason", "the daemon exited while this run was in flight")
		_ = f.Close()
	}
}

// persistedRunsLocked marshals the run id allocators of registered harnesses,
// plus any record list still waiting for a first-boot import that failed. A
// harness gone from the config takes its allocator with it; its ids stay
// reserved by the log and ledger floors. Caller holds mu.
func (m *Manager) persistedRunsLocked() json.RawMessage {
	out := make(map[string]legacyHistory, len(m.runs))
	for name, h := range m.runs {
		if _, ok := m.supervisors[name]; ok {
			out[name] = legacyHistory{LastRunID: h.LastRunID, Runs: m.legacyRuns[name]}
		}
	}
	if len(out) == 0 {
		return nil
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return raw
}

// toLedger projects a run record onto the ledger's record fields.
func toLedger(r RunRecord) ledger.Record {
	kind := string(r.Kind)
	if kind == "" {
		kind = ledger.KindOneshot
	}
	rec := ledger.Record{
		Kind:        kind,
		Trigger:     string(r.Trigger),
		Source:      r.Source,
		EventID:     r.EventID,
		EndedAt:     r.EndedAt,
		ExitCode:    r.ExitCode,
		Outcome:     string(r.Outcome),
		Reason:      string(r.Reason),
		MissingPath: r.MissingPath,
		Log:         r.Log,
		Window:      r.Window,
		FirstWindow: r.FirstWindow,
		Windows:     r.Windows,
		Coalesced:   r.Coalesced,
	}
	if !r.StartedAt.IsZero() {
		t := r.StartedAt
		rec.StartedAt = &t
	}
	if r.Mismatch != nil {
		rec.Mismatch = &ledger.Mismatch{Kind: r.Mismatch.Kind, ServedModel: r.Mismatch.ServedModel, ServedProvider: r.Mismatch.ServedProvider, At: r.Mismatch.At}
	}
	if r.EndedAt != nil {
		rec.DurationMs = r.EndedAt.Sub(r.StartedAt).Milliseconds()
	}
	return rec
}

// fromLedger projects a folded ledger record back onto a run record.
func fromLedger(f ledger.Folded) RunRecord {
	r := RunRecord{
		RunID:       f.RunID,
		Trigger:     RunTrigger(f.Trigger),
		Outcome:     RunOutcome(f.Outcome),
		EndedAt:     f.EndedAt,
		ExitCode:    f.ExitCode,
		Window:      f.Window,
		FirstWindow: f.FirstWindow,
		Windows:     f.Windows,
		Reason:      RunReason(f.Reason),
		MissingPath: f.MissingPath,
		Coalesced:   f.Coalesced,
		Source:      f.Source,
		EventID:     f.EventID,
		Log:         f.Log,
		LogPruned:   f.LogPruned,
		Kind:        RunKind(f.Kind),
	}
	if f.Mismatch != nil {
		r.Mismatch = &RunMismatch{Kind: f.Mismatch.Kind, ServedModel: f.Mismatch.ServedModel, ServedProvider: f.Mismatch.ServedProvider, At: f.Mismatch.At}
	}
	if f.StartedAt != nil {
		r.StartedAt = *f.StartedAt
	} else {
		r.StartedAt = f.FirstAt
	}
	return r
}

package supervisor

// Run History Store
//
// The Manager's half of run history (runs.go is the supervisor's). It hands out
// run ids, bounds each scheduled harness's history to keep_runs, deletes the
// log files of records that fall out of it, and keeps all of it in state.json
// next to the rest of ADR-0007's runtime state.
//
// Run ids never repeat. The history persists the last id handed out, and the
// first allocation of each process also floors it at the highest log file on
// disk, so a history lost to a malformed state file, or dropped because its
// harness briefly left the config, cannot reissue an id whose log still exists.
//
// A record still "running" when the daemon boots belonged to a daemon that died
// under it. Restore marks it interrupted — leaving ended_at unset, because the
// real end is unknown — and appends a line saying so to its log.
//
// Governing: ADR-0007, ADR-0008, ADR-0013; SPEC-0008 REQ "Run History", REQ
// "Per-Run Logs".
//
// @joestump-agent 09/11/2026 - Added for issue #119.
//
// @joestump-agent 09/11/2026 - Review of PR #310: run log paths go through
// runLogDir, so a name that is not a single path element (a project harness,
// or a name from the protocol in #120) never writes or prunes outside jobs/.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// runHistory is one scheduled harness's run history, in memory and on disk.
type runHistory struct {
	LastRunID int         `json:"last_run_id"`
	Runs      []RunRecord `json:"runs"`
	// floored is set once LastRunID has been checked against the log files on
	// disk in this process. Not persisted.
	floored bool
}

// runRef names one run.
type runRef struct {
	name string
	id   int
}

// OpenRun implements RunJournal.
func (m *Manager) OpenRun(name string, rec RunRecord) (RunRecord, string, error) {
	rec, err := m.AppendRun(name, rec)
	return rec, m.RunLogPath(name, rec.RunID), err
}

// AppendRun implements RunJournal: it assigns the next run id, bounds the
// history, prunes the logs of dropped records, and saves synchronously — the
// id must be on disk before a run that uses it writes a log.
func (m *Manager) AppendRun(name string, rec RunRecord) (RunRecord, error) {
	m.mu.Lock()
	h := m.historyLocked(name)
	h.LastRunID++
	rec.RunID = h.LastRunID
	h.Runs = append(h.Runs, rec)
	h.prune(m.keepRunsLocked(name))
	kept := make(map[int]bool, len(h.Runs))
	for _, r := range h.Runs {
		kept[r.RunID] = true
	}
	upTo := h.LastRunID
	m.mu.Unlock()

	m.pruneRunLogs(name, kept, upTo)
	return rec, m.Save()
}

// CloseRun implements RunJournal.
func (m *Manager) CloseRun(name string, rec RunRecord) error {
	m.mu.Lock()
	if h := m.runs[name]; h != nil {
		for i := range h.Runs {
			if h.Runs[i].RunID == rec.RunID {
				h.Runs[i] = rec
				break
			}
		}
	}
	m.mu.Unlock()
	return m.Save()
}

// StartRun is a scheduled firing for name (SPEC-0008 REQ "Overlap Policy").
// It returns false for an unknown harness.
//
// A firing that lands while the harness is mid graceful stop is recorded
// skipped here rather than sent to the loop: the loop would only see it once
// the stop completed, and would then start a harness an operator just stopped
// (SPEC-0008 REQ "Firing And Overlap").
func (m *Manager) StartRun(name string, req RunRequest) bool {
	s := m.get(name)
	if s == nil {
		return false
	}
	if s.Snapshot().State == core.StateStopping {
		_, _ = m.AppendRun(name, decisionRecord(req, OutcomeSkipped, time.Now()))
		return true
	}
	s.StartRun(req)
	m.clearDormant(name)
	return true
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
	_, err := m.AppendRun(name, rec)
	return err
}

// Runs returns name's run history, oldest first. Each record's StartedAt and
// EndedAt bound one run exactly, which is what run correlation needs to scope
// an agent transcript to a single run.
func (m *Manager) Runs(name string) []RunRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := m.runs[name]
	if h == nil {
		return nil
	}
	return slices.Clone(h.Runs)
}

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

// historyLocked returns name's history, creating it, and floors its last id at
// the logs on disk the first time it is used in this process. Caller holds mu.
func (m *Manager) historyLocked(name string) *runHistory {
	h := m.runs[name]
	if h == nil {
		h = &runHistory{}
		m.runs[name] = h
	}
	if !h.floored {
		h.floored = true
		if n := highestRunLog(m.runLogDir(name)); n > h.LastRunID {
			h.LastRunID = n
		}
	}
	return h
}

// keepRunsLocked is name's history bound. Caller holds mu.
func (m *Manager) keepRunsLocked(name string) int {
	if h, ok := m.cfg.Harnesses[name]; ok && h.KeepRuns > 0 {
		return h.KeepRuns
	}
	return core.DefaultKeepRuns
}

// prune drops the oldest finished records until at most keep remain. The run
// in flight is never dropped, so its log is never deleted from under it.
func (h *runHistory) prune(keep int) {
	for len(h.Runs) > keep {
		i := slices.IndexFunc(h.Runs, func(r RunRecord) bool { return r.Outcome != OutcomeRunning })
		if i < 0 {
			return
		}
		h.Runs = slices.Delete(h.Runs, i, i+1)
	}
}

// pruneRunLogs deletes name's run logs that no record refers to, so history and
// logs cannot drift. Only ids up to upTo are considered: a log newer than the
// history this call saw belongs to a run opened since, and is not ours to judge.
func (m *Manager) pruneRunLogs(name string, kept map[int]bool, upTo int) {
	dir := m.runLogDir(name)
	if dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		id, ok := runLogID(e.Name())
		if !ok || id > upTo || kept[id] {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
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

// runLogID parses "<id>.log".
func runLogID(file string) (int, bool) {
	base, ok := strings.CutSuffix(file, ".log")
	if !ok {
		return 0, false
	}
	id, err := strconv.Atoi(base)
	if err != nil || id < 1 {
		return 0, false
	}
	return id, true
}

// restoreRunsLocked loads persisted histories and reconciles runs a dead daemon
// left "running". A malformed history costs only that harness its records —
// never the rest of state.json, and never an id, because of the log floor.
// Caller holds mu.
func (m *Manager) restoreRunsLocked(raw json.RawMessage) []runRef {
	if len(raw) == 0 {
		return nil
	}
	var byName map[string]json.RawMessage
	if err := json.Unmarshal(raw, &byName); err != nil {
		return nil
	}
	var interrupted []runRef
	for name, body := range byName {
		var h runHistory
		if err := json.Unmarshal(body, &h); err != nil {
			continue
		}
		for i := range h.Runs {
			if h.Runs[i].Outcome == OutcomeRunning {
				h.Runs[i].Outcome = OutcomeInterrupted
				interrupted = append(interrupted, runRef{name: name, id: h.Runs[i].RunID})
			}
		}
		m.runs[name] = &h
	}
	return interrupted
}

// noteInterrupted appends the reconciliation to each interrupted run's log, so
// the log does not simply stop mid-run with no explanation.
func (m *Manager) noteInterrupted(runs []runRef) {
	for _, r := range runs {
		f, err := os.OpenFile(m.RunLogPath(r.name, r.id), os.O_WRONLY|os.O_APPEND, 0)
		if err != nil {
			continue
		}
		newEventLogger(f).Warn("run interrupted", "run_id", r.id, "reason", "the daemon exited while this run was in flight")
		_ = f.Close()
	}
}

// persistedRunsLocked marshals the histories of registered harnesses. A
// harness gone from the config takes its history with it; its ids stay
// reserved by the log floor. Caller holds mu.
func (m *Manager) persistedRunsLocked() json.RawMessage {
	out := make(map[string]*runHistory, len(m.runs))
	for name, h := range m.runs {
		if _, ok := m.supervisors[name]; ok {
			out[name] = h
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

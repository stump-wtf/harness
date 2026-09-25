// Package ledger is the daemon's run history: one durable, append-only record
// per run of every harness.
//
// # Run History Ledger
//
// A run is one firing of a one-shot, or one process lifetime of a resident.
// Each gets a record, written as lines to one JSONL file per UTC day under
// $XDG_STATE_HOME/harness/ledger/. A record is never rewritten in place: an
// `opened` line starts it, `updated` lines checkpoint it, a `closed` line ends
// it, and a `decided` line stands for a whole record that started no process
// (skipped, missed). A reader folds every line of one (harness, run_id), in seq
// order, into the record.
//
// The package has no supervisor dependency. The Manager's RunJournal is its
// only writer, and the offline CLI path its only other reader, so the format,
// the single writer goroutine, the fold and the in-memory index all live here,
// tested on their own with temp directories and a fake clock.
//
// Governing: ADR-0028 (run history ledger); SPEC-0022 REQ-1, REQ-2, REQ-6,
// REQ-7, REQ-13, REQ-17, REQ-20; ADR-0008 (no environment, prompt, output or
// payload ever reaches a ledger line).
//
// @joestump 09/23/2026 - Added for harness#444.
package ledger

import (
	"errors"
	"time"
)

// Version is the line schema version, the `v` of every line (REQ-1).
const Version = 1

// Type is a line's kind (REQ-2).
type Type string

const (
	// TypeOpened starts a record: written when a run is admitted, before its
	// process spawns, and synced.
	TypeOpened Type = "opened"
	// TypeUpdated is a checkpoint carrying only the fields that changed. It
	// may be buffered.
	TypeUpdated Type = "updated"
	// TypeClosed ends a record, by any path, and is synced.
	TypeClosed Type = "closed"
	// TypeDecided is a whole record that started no process (skipped,
	// missed), and is synced.
	TypeDecided Type = "decided"
)

// Kinds of run (REQ-4).
const (
	KindOneshot  = "oneshot"
	KindResident = "resident"
)

// Outcomes and reasons the ledger itself writes. The rest of the vocabulary is
// the supervisor's (SPEC-0008, SPEC-0014, SPEC-0022 REQ-5): the ledger stores
// them as opaque strings.
const (
	OutcomeRunning     = "running"
	OutcomeInterrupted = "interrupted"
	// ReasonDaemonCrash marks a record found open at boot (REQ-7).
	ReasonDaemonCrash = "daemon_crash"
	// ReasonShutdown marks a record a clean shutdown closed (REQ-7).
	ReasonShutdown = "shutdown"
)

// Limits (REQ-1, REQ-4).
const (
	// MaxLineBytes caps one encoded line, newline included.
	MaxLineBytes = 16 << 10
	// MaxStringBytes caps every string field of a record.
	MaxStringBytes = 256
	// MaxListEntries caps `models` and `sessions`, and the classes in `errors`.
	MaxListEntries = 16
)

// Sentinel errors (REQ-20).
var (
	// ErrLedgerUnavailable: a line could not be written and synced in time.
	// It stays queued and is retried in order; the caller decides whether to
	// proceed (SPEC-0021 REQ-4 refuses budgeted admission, nothing else does).
	ErrLedgerUnavailable = errors.New("ledger: unavailable")
	// ErrLedgerCorrupt: a line was refused as malformed before it was
	// written, or a ledger file could not be read as JSONL at all.
	ErrLedgerCorrupt = errors.New("ledger: corrupt")
	// ErrClosed: an append after Close.
	ErrClosed = errors.New("ledger: closed")
)

// Line is one line of a day file: the envelope every line carries, and the
// record fields it sets, flattened into the same JSON object.
type Line struct {
	V       int       `json:"v"`
	Seq     uint64    `json:"seq"`
	Type    Type      `json:"type"`
	At      time.Time `json:"at"`
	Harness string    `json:"harness"`
	RunID   int       `json:"run_id"`
	Record
}

// Record is a run's fields (REQ-4). Every field is omitted when it does not
// apply, so a line carries exactly what it sets, and the fold (fold.go) reads
// "present" as "non-zero".
//
// It holds outcomes, times, codes, identifiers and counts. Never environment,
// prompt text, agent output, event payloads, header values or credentials
// (REQ-17; ADR-0008): a field that could carry one of those does not belong
// here.
type Record struct {
	Kind      string     `json:"kind,omitempty"`
	Trigger   string     `json:"trigger,omitempty"`
	Source    string     `json:"source,omitempty"`
	EventID   string     `json:"event_id,omitempty"`
	TodoID    string     `json:"todo_id,omitempty"`
	Attempt   int        `json:"attempt,omitempty"`
	StartedAt *time.Time `json:"started_at,omitempty"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	// DurationMs is EndedAt - StartedAt, written with the end.
	DurationMs int64 `json:"duration_ms,omitempty"`
	// ExitCode is set only when a process was reaped; -1 if signalled.
	ExitCode *int   `json:"exit_code,omitempty"`
	Outcome  string `json:"outcome,omitempty"`
	Reason   string `json:"reason,omitempty"`

	// Usage (REQ-8), folded in by the usage accumulator.
	Model         string         `json:"model,omitempty"`
	Models        []ModelUse     `json:"models,omitempty"`
	Tokens        *Tokens        `json:"tokens,omitempty"`
	CostUSD       *float64       `json:"cost_usd,omitempty"`
	CostSource    string         `json:"cost_source,omitempty"`
	ModelCalls    int            `json:"model_calls,omitempty"`
	Errors        map[string]int `json:"errors,omitempty"`
	UsageComplete *bool          `json:"usage_complete,omitempty"`
	Sessions      []Session      `json:"sessions,omitempty"`
	TraceURL      string         `json:"trace_url,omitempty"`

	// Log is the per-run log path (one-shot) or the durable log path
	// (resident).
	Log string `json:"log,omitempty"`
	// LogPruned is never written: a reader sets it when Log names a file
	// keep_runs has since deleted (REQ-12).
	LogPruned bool      `json:"log_pruned,omitempty"`
	Override  bool      `json:"override,omitempty"`
	Mismatch  *Mismatch `json:"mismatch,omitempty"`
	// Imported marks a record the first-boot import copied out of state.json
	// (REQ-13).
	Imported bool `json:"imported,omitempty"`

	// SPEC-0008 / SPEC-0014 fields.
	Window      *time.Time `json:"window,omitempty"`
	FirstWindow *time.Time `json:"first_window,omitempty"`
	Windows     int        `json:"windows,omitempty"`
	Coalesced   int        `json:"coalesced,omitempty"`
}

// ModelUse is one served model and provider (REQ-4).
type ModelUse struct {
	Model        string `json:"model"`
	Provider     string `json:"provider,omitempty"`
	OutputTokens int64  `json:"output_tokens,omitempty"`
}

// Tokens are a run's token counts (REQ-4).
type Tokens struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
}

// Session is one agent session attributed to a run (REQ-4, REQ-9).
type Session struct {
	ID      string `json:"id"`
	Adapter string `json:"adapter,omitempty"`
	TraceID string `json:"trace_id,omitempty"`
}

// Mismatch is the first mismatching call of a model_mismatch record
// (SPEC-0020).
type Mismatch struct {
	Kind           string    `json:"kind"`
	ServedModel    string    `json:"served_model,omitempty"`
	ServedProvider string    `json:"served_provider,omitempty"`
	At             time.Time `json:"at"`
}

// Folded is one run's record: the fold of its lines (REQ-2).
type Folded struct {
	Harness string `json:"harness"`
	RunID   int    `json:"run_id"`
	Record
	// Seq is the seq of the record's first line. Records sort by it, and the
	// runs op pages by it (REQ-15).
	Seq uint64 `json:"seq"`
	// LastSeq is the seq of the latest line folded in.
	LastSeq uint64 `json:"-"`
	// FirstAt is the `at` of the record's first line.
	FirstAt time.Time `json:"-"`
	// HasOpened is set once an `opened` line was folded.
	HasOpened bool `json:"-"`
	// Closed is set once a `closed` or `decided` line was folded.
	Closed bool `json:"-"`
}

// Open reports a run in flight: opened, and neither closed nor decided.
func (f Folded) Open() bool { return f.HasOpened && !f.Closed }

// Partial reports a record whose opening line is not in what was read: it was
// pruned, or lies in a file older than the reader looked at (REQ-2).
func (f Folded) Partial() bool { return !f.HasOpened && !f.Closed }

// when is the instant a record sorts and filters by: its start, else the time
// of its first line.
func (f Folded) when() time.Time {
	if f.StartedAt != nil {
		return *f.StartedAt
	}
	return f.FirstAt
}

// dayName is the file name of t's UTC day.
func dayName(t time.Time) string { return t.UTC().Format("2006-01-02") + ".jsonl" }

// dayOf parses a day file name back into the start of its UTC day.
func dayOf(name string) (time.Time, bool) {
	if len(name) != len("2006-01-02.jsonl") {
		return time.Time{}, false
	}
	t, err := time.Parse("2006-01-02.jsonl", name)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

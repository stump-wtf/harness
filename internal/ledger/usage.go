package ledger

// The Usage Accumulator
//
// The observer (internal/observe) is the source of agent activity: tool calls,
// error marks and sessions, each attributed to a harness. The accumulator
// attributes each item to the run open for that harness at the item's time and
// folds it into that run's record (SPEC-0022 REQ-8):
//
//   - a tool call adds one to model_calls;
//   - an error mark adds one to errors[class], the class SPEC-0013's
//     classifier gave it (the caller classifies, so this package and the
//     metrics collector cannot disagree about what a class is);
//   - a new session adds {id, adapter, trace_id} to sessions, and sets
//     trace_url from the [ledger] trace_url template (REQ-9).
//
// It writes an `updated` checkpoint at most every 30 seconds per run, and hands
// its final totals to the ledger when the run's `closed` line is enqueued
// (Ledger.SetCloseHook), so the close carries them whatever path ended the run.
//
// It never blocks the observer: the caller reads a buffered subscription and
// the observer drops for it rather than wait. A drop cannot be attributed to a
// run, so every run open when one is seen reads usage_complete: false.
//
// Tokens, cost and the served model are the next story; they need
// stump.wtf/agent-trace#105's usage items.
//
// Governing: SPEC-0022 REQ-8, REQ-9, REQ-4; design "The usage accumulator".
//
// @joestump 09/24/2026 - Added for harness#459.

import (
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
)

// TraceIDPlaceholder is the one substitution a trace_url template supports.
const TraceIDPlaceholder = "{trace_id}"

// Item is one observer item, already attributed and classified.
type Item struct {
	Harness string
	// At is the item's own time; it picks the run.
	At time.Time
	// Tool marks a successful model call (a tool event, as SPEC-0013 counts
	// them).
	Tool bool
	// ErrorClass is set for an error mark: its SPEC-0013 class.
	ErrorClass string
	// Session is the session the item came from.
	Session Session
}

// AccumulatorOptions tunes an Accumulator. The zero value is production.
type AccumulatorOptions struct {
	// CheckpointEvery bounds how often a run's `updated` line is written
	// (default and maximum rate: 30s).
	CheckpointEvery time.Duration
	// Now is the clock (default time.Now).
	Now func() time.Time
}

// AccumulatorStats are the accumulator's counters (REQ-18).
type AccumulatorStats struct {
	// Folded counts items folded into a run.
	Folded uint64
	// NoRun counts items that arrived when their harness had no open run,
	// or dated before the open run started (they belonged to one that has
	// closed).
	NoRun uint64
	// Dropped is the observer's drop count for the accumulator's
	// subscription, as last reported.
	Dropped uint64
	// Incomplete counts runs marked usage_complete: false.
	Incomplete uint64
}

// Accumulator folds observer items into open runs.
type Accumulator struct {
	l    *Ledger
	opts AccumulatorOptions

	mu       sync.Mutex
	runs     map[key]*usage
	template string
	stats    AccumulatorStats
}

// usage is one open run's accumulated activity.
type usage struct {
	calls      int
	errors     map[string]int
	sessions   []Session
	incomplete bool
	// dirty is set when something changed since the last checkpoint.
	dirty     bool
	lastWrite time.Time
}

// NewAccumulator returns an accumulator writing to l, and registers its final
// totals as l's close hook.
func NewAccumulator(l *Ledger, opts AccumulatorOptions) *Accumulator {
	if opts.CheckpointEvery <= 0 {
		opts.CheckpointEvery = DefaultFlushEvery
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	a := &Accumulator{l: l, opts: opts, runs: map[key]*usage{}}
	l.SetCloseHook(a.final)
	return a
}

// SetTraceURL sets the trace_url template (REQ-9). A reload applies it to
// records closed after it (REQ-19), so it is read when a line is written, not
// when a session is seen.
func (a *Accumulator) SetTraceURL(template string) {
	a.mu.Lock()
	a.template = template
	a.mu.Unlock()
}

// Fold attributes one item to its harness's open run and folds it in.
func (a *Accumulator) Fold(it Item) {
	f, ok := a.l.OpenRun(it.Harness)
	if !ok || (f.StartedAt != nil && it.At.Before(*f.StartedAt)) {
		a.mu.Lock()
		a.stats.NoRun++
		a.mu.Unlock()
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	u := a.usageLocked(key{f.Harness, f.RunID})
	switch {
	case it.Tool:
		u.calls++
	case it.ErrorClass != "":
		if u.errors == nil {
			u.errors = map[string]int{}
		}
		u.errors[it.ErrorClass]++
	}
	if it.Session.ID != "" && !slices.ContainsFunc(u.sessions, func(s Session) bool { return s.ID == it.Session.ID }) {
		u.sessions = append(u.sessions, it.Session)
	}
	u.dirty = true
	a.stats.Folded++
}

// Dropped reports the observer's cumulative drop count for the accumulator's
// subscription. When it has grown, the items lost belonged to some open run,
// and nothing says which: every run open now reads usage_complete: false.
func (a *Accumulator) Dropped(total uint64) {
	a.mu.Lock()
	grew := total > a.stats.Dropped
	a.stats.Dropped = max(a.stats.Dropped, total)
	a.mu.Unlock()
	if !grew {
		return
	}
	open := a.l.OpenRecords()
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, f := range open {
		u := a.usageLocked(key{f.Harness, f.RunID})
		if !u.incomplete {
			u.incomplete = true
			u.dirty = true
			a.stats.Incomplete++
		}
	}
}

func (a *Accumulator) usageLocked(k key) *usage {
	u := a.runs[k]
	if u == nil {
		u = &usage{}
		a.runs[k] = u
	}
	return u
}

// Tick writes an `updated` checkpoint for each run whose totals changed and
// whose last checkpoint is at least CheckpointEvery old, and forgets runs that
// are no longer open. The caller runs it on a short ticker.
func (a *Accumulator) Tick() {
	now := a.opts.Now()
	open := map[key]bool{}
	for _, f := range a.l.OpenRecords() {
		open[key{f.Harness, f.RunID}] = true
	}
	var lines []Line
	a.mu.Lock()
	for k, u := range a.runs {
		if !open[k] {
			// Closed without passing through the hook (a line written by
			// another path), or never opened: nothing left to write to.
			delete(a.runs, k)
			continue
		}
		if !u.dirty || now.Sub(u.lastWrite) < a.opts.CheckpointEvery {
			continue
		}
		u.dirty, u.lastWrite = false, now
		lines = append(lines, Line{Type: TypeUpdated, Harness: k.harness, RunID: k.id, Record: a.recordLocked(u)})
	}
	a.mu.Unlock()
	for _, ln := range lines {
		// Buffered: a checkpoint is not a fact, and REQ-6 lets it wait.
		if _, err := a.l.Append(ln, false); err != nil {
			a.l.log.Error("usage checkpoint not written", "harness", ln.Harness, "run_id", ln.RunID, "err", err)
		}
	}
}

// final is the ledger's close hook: the run's last totals, folded into its
// `closed` line, and the run forgotten.
func (a *Accumulator) final(harness string, id int) Record {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := key{harness, id}
	u := a.runs[k]
	if u == nil {
		return Record{}
	}
	delete(a.runs, k)
	return a.recordLocked(u)
}

// recordLocked renders u as record fields. Caller holds mu.
func (a *Accumulator) recordLocked(u *usage) Record {
	complete := !u.incomplete
	r := Record{
		ModelCalls:    u.calls,
		Errors:        maps.Clone(u.errors),
		Sessions:      slices.Clone(u.sessions),
		UsageComplete: &complete,
	}
	if a.template != "" {
		for _, s := range u.sessions {
			if s.TraceID != "" {
				r.TraceURL = strings.ReplaceAll(a.template, TraceIDPlaceholder, s.TraceID)
				break
			}
		}
	}
	return r
}

// Stats returns a copy of the counters.
func (a *Accumulator) Stats() AccumulatorStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.stats
}

// OpenRun returns harness's open run: the newest committed record opened and
// not closed.
func (l *Ledger) OpenRun(harness string) (Folded, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	list := l.idx.by[harness]
	for i := len(list) - 1; i >= 0; i-- {
		if list[i].Open() {
			return *list[i], true
		}
	}
	return Folded{}, false
}

// SetCloseHook registers fn to supply fields for every `closed` line as it is
// enqueued: the accumulator's final totals. The line's own fields win over
// the hook's.
func (l *Ledger) SetCloseHook(fn func(harness string, id int) Record) {
	l.hookMu.Lock()
	l.closeHook = fn
	l.hookMu.Unlock()
}

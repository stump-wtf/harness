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
// A usage report (Item.Usage: tokens, a recorded cost, the model and provider
// that served it) adds its tokens, adds its cost as SPEC-0021 REQ-8 resolves
// it, and adds its (model, provider) to `models`, with `model` the entry with
// the most output tokens. A cumulative report (crush keeps only session totals,
// and a resident crush resumes one session across restarts) is differenced
// against the last total seen for its session, across runs, so a run counts
// only what it spent; a total lower than the last is a new baseline, never a
// negative delta. Every fold hands the run's live totals to OnFold, so a
// budget cap is checked on the item that crosses it (SPEC-0021 REQ-9).
//
// Usage reports need stump.wtf/agent-trace#105. Until the observer delivers
// them, no item carries one, and records have no tokens, cost_usd or models
// (REQ-8's last sentence): nothing here invents them.
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
	// Usage is a usage report (agent-trace#105), or nil.
	Usage *UsageReport
}

// UsageReport is one usage record from a transcript.
type UsageReport struct {
	// Model and Provider served the message; "" when not recorded.
	Model, Provider string
	Tokens          Tokens
	// CostUSD is the cost the agent recorded, or nil. Never computed by
	// the reader.
	CostUSD *float64
	// Cumulative marks session totals rather than a per-message delta.
	Cumulative bool
}

// Cost sources (SPEC-0021 REQ-8), strongest first. A run's cost_source is the
// weakest among its items.
const (
	CostRecorded = "recorded"
	CostPriced   = "priced"
	CostUnknown  = "unknown"
)

// PriceFunc prices tokens for a served model from [budget.prices], and
// reports false when there is no entry. The daemon ships no default prices.
type PriceFunc func(model, provider string, t Tokens) (usd float64, ok bool)

// LiveTotals is a run's usage so far, handed to OnFold on every fold.
type LiveTotals struct {
	Harness    string
	RunID      int
	Tokens     Tokens
	CostUSD    float64
	CostSource string
}

// AccumulatorOptions tunes an Accumulator. The zero value is production.
type AccumulatorOptions struct {
	// CheckpointEvery bounds how often a run's `updated` line is written
	// (default and maximum rate: 30s).
	CheckpointEvery time.Duration
	// Now is the clock (default time.Now).
	Now func() time.Time
	// Price resolves a cost the agent did not record (SPEC-0021 REQ-8).
	// Nil prices nothing: every such item is unknown.
	Price PriceFunc
	// OnFold receives a run's live totals after every usage fold, outside
	// the accumulator's lock (SPEC-0021 REQ-9's caps).
	OnFold func(LiveTotals)
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
	// lastTotals is the last cumulative report per session, kept across
	// runs: the baseline a resident's next run differences against.
	lastTotals map[string]UsageReport
}

// usage is one open run's accumulated activity.
type usage struct {
	calls      int
	errors     map[string]int
	sessions   []Session
	incomplete bool
	// Usage reports folded in (REQ-8): hasUsage stays false until the first,
	// so a run with none carries no tokens, cost or models at all.
	hasUsage bool
	tokens   Tokens
	models   []ModelUse
	cost     float64
	costSrc  string
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
	a := &Accumulator{l: l, opts: opts, runs: map[key]*usage{}, lastTotals: map[string]UsageReport{}}
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
		if it.Usage != nil && it.Usage.Cumulative {
			// Spent by no open run, but it moves the session's baseline:
			// the next run must count from here, not from before it.
			a.lastTotals[it.Session.ID] = *it.Usage
		}
		a.mu.Unlock()
		return
	}
	a.mu.Lock()
	u := a.usageLocked(key{f.Harness, f.RunID})
	var live *LiveTotals
	if it.Usage != nil {
		if a.foldUsageLocked(u, it) {
			live = &LiveTotals{Harness: f.Harness, RunID: f.RunID, Tokens: u.tokens, CostUSD: u.cost, CostSource: u.costSrc}
		}
	}
	a.foldActivityLocked(u, it)
	a.mu.Unlock()
	if live != nil && a.opts.OnFold != nil {
		a.opts.OnFold(*live)
	}
}

// foldActivityLocked folds an item's tool call, error and session. Caller
// holds mu.
func (a *Accumulator) foldActivityLocked(u *usage, it Item) {
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

// foldUsageLocked folds a usage report into u and reports whether it added
// anything. Caller holds mu.
func (a *Accumulator) foldUsageLocked(u *usage, it Item) bool {
	rep := *it.Usage
	delta := rep.Tokens
	var cost *float64 = rep.CostUSD
	if rep.Cumulative {
		last, seen := a.lastTotals[it.Session.ID]
		a.lastTotals[it.Session.ID] = rep
		if !seen || lower(rep, last) {
			// The first total this daemon has seen for the session, or one
			// that went backwards (a new session reusing the id, a reset
			// store): a baseline, never a negative delta (REQ-8).
			return false
		}
		delta = Tokens{
			Input:      rep.Tokens.Input - last.Tokens.Input,
			Output:     rep.Tokens.Output - last.Tokens.Output,
			CacheRead:  rep.Tokens.CacheRead - last.Tokens.CacheRead,
			CacheWrite: rep.Tokens.CacheWrite - last.Tokens.CacheWrite,
		}
		cost = nil
		if rep.CostUSD != nil && last.CostUSD != nil && *rep.CostUSD >= *last.CostUSD {
			d := *rep.CostUSD - *last.CostUSD
			cost = &d
		}
	}
	u.hasUsage = true
	u.tokens.Input += delta.Input
	u.tokens.Output += delta.Output
	u.tokens.CacheRead += delta.CacheRead
	u.tokens.CacheWrite += delta.CacheWrite

	// Cost: recorded, else priced, else unknown (SPEC-0021 REQ-8); the
	// run's source is the weakest of its items'.
	src := CostUnknown
	switch {
	case cost != nil:
		u.cost += *cost
		src = CostRecorded
	case a.opts.Price != nil:
		if usd, ok := a.opts.Price(rep.Model, rep.Provider, delta); ok {
			u.cost += usd
			src = CostPriced
		}
	}
	if u.costSrc == "" || costRank(src) > costRank(u.costSrc) {
		u.costSrc = src
	}

	if rep.Model != "" || rep.Provider != "" {
		i := slices.IndexFunc(u.models, func(m ModelUse) bool { return m.Model == rep.Model && m.Provider == rep.Provider })
		if i < 0 {
			u.models = append(u.models, ModelUse{Model: rep.Model, Provider: rep.Provider})
			i = len(u.models) - 1
		}
		u.models[i].OutputTokens += delta.Output
	}
	u.dirty = true
	return true
}

// lower reports a cumulative total that went backwards on any count.
func lower(cur, last UsageReport) bool {
	return cur.Tokens.Input < last.Tokens.Input || cur.Tokens.Output < last.Tokens.Output ||
		cur.Tokens.CacheRead < last.Tokens.CacheRead || cur.Tokens.CacheWrite < last.Tokens.CacheWrite
}

// costRank orders cost sources weakest last.
func costRank(src string) int {
	switch src {
	case CostRecorded:
		return 0
	case CostPriced:
		return 1
	}
	return 2
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
	if u.hasUsage {
		t := u.tokens
		r.Tokens = &t
		r.Models = slices.Clone(u.models)
		r.CostSource = u.costSrc
		if u.costSrc != CostUnknown || u.cost > 0 {
			c := u.cost
			r.CostUSD = &c
		}
		// model is the served model with the most output tokens (REQ-4).
		var best int64 = -1
		for _, m := range u.models {
			if m.Model != "" && m.OutputTokens > best {
				r.Model, best = m.Model, m.OutputTokens
			}
		}
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

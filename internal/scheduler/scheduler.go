// Package scheduler fires harnesses on a cron schedule.
//
// Governing: ADR-0013 (daemon-owned cron; `schedule` on [harness.*] rather
// than a [job.*] table kind); SPEC-0008 REQ "Suspend-Safe Schedule
// Evaluation", REQ "Missed Window Handling", REQ "Schedule Time Zone", REQ
// "Firing And Overlap", REQ "Schedule Reconciliation On Reload", REQ
// "Scheduler Fault Isolation"; issues #66, #117; ADR-0006 (harness.toml is the
// source of truth the entries reconcile against); ADR-0007 (the scheduler's
// position survives a restart in state.json); ADR-0011 (the scheduled unit is
// a prompt one-shot). A scheduled harness is a one-shot agent run that the
// daemon owns: when a window comes due the daemon starts the harness if it is
// not already running (an overlapping firing is skipped, not stacked). The run
// exiting is terminal for that firing; the restart policy applies only to
// abnormal exit if configured.
//
// The scheduler never arms a timer to a window. It asks "is anything due?"
// against the WALL clock on a short tick, so a laptop that sleeps through
// 03:00 and wakes at 08:00 reaches a deliberate decision — run once, or record
// the miss — instead of whatever the OS did to a long timer across the
// suspend. A daemon that starts after a window it was down for takes the very
// same path: boot is just the first tick.
//
// @joestump-agent 09/11/2026 - Replaced robfig/cron's timer-driven runner with
// a wall-clock tick (issue #117). robfig/cron is still the parser, so config
// validation and firing cannot disagree about what an expression means. Added
// missed-window detection, `catch_up`, a durable per-harness mark, an
// injectable clock, and DST/CRON_TZ-correct window resolution (window.go).
package scheduler

import (
	"slices"
	"sync"
	"time"

	"charm.land/log/v2"
	"github.com/robfig/cron/v3"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// TickInterval is how often the scheduler compares the wall clock against
// every armed window.
//
// One second, because that is the finest resolution a schedule can ask for:
// robfig/cron's `@every` rounds to whole seconds and the 5-field form fires on
// minute boundaries, so a tick any longer makes `@every 30s` visibly late and
// any shorter buys nothing. The cost is one wakeup per second for the whole
// daemon, not per job: each tick is a single pass comparing one cached
// time.Time per armed entry, and cron arithmetic runs only for an entry that
// is actually due. That stays negligible with hundreds of entries armed.
//
// It is also the longest timer the scheduler ever holds, which is the point:
// across a suspend, the worst a stale timer can cost is one tick.
const TickInterval = time.Second

// LateGrace is how far past its window a run may start and still count as on
// time.
//
// Anything later means nobody was evaluating when the window passed — the
// machine was suspended, the daemon was down, or the clock jumped forward —
// and the window is handled as missed (SPEC-0008 REQ "Missed Window
// Handling"). A minute comfortably absorbs a slow tick under load or a quick
// daemon restart, while any real suspend or outage lands well outside it.
const LateGrace = time.Minute

// backwardsSlack is how far the wall clock may step back between ticks before
// the scheduler logs it as a backwards jump. Sub-second NTP slews are noise.
const backwardsSlack = 2 * time.Second

// maxWindowScan bounds how many elapsed windows one evaluation walks when a
// job wakes far behind (an `@every 1s` job after a weekend asleep). Past it the
// count is reported as a floor and the entry re-arms from now.
const maxWindowScan = 10000

// Trigger names why a run was started.
type Trigger string

const (
	// TriggerSchedule is an on-time firing: the window came due while the
	// scheduler was evaluating.
	TriggerSchedule Trigger = "schedule"
	// TriggerCatchUp is the single run a `catch_up = true` harness gets for
	// windows that elapsed while nobody was evaluating.
	TriggerCatchUp Trigger = "catch_up"
)

// Firing is one decision to start a run.
type Firing struct {
	Name    string
	Trigger Trigger
	// Window is the scheduled time being honored: the due window for an
	// on-time firing, the most recent elapsed window for a catch-up.
	Window time.Time
	// Late is how long after Window the decision was made.
	Late time.Duration
	// Missed counts the elapsed windows a catch-up run stands in for (0 for
	// an on-time firing). A floor when the scan hit maxWindowScan.
	Missed int
}

// StartFunc is the callback the scheduler invokes when a harness fires. It
// starts the harness (if it is not already running) and returns. It runs on
// its own goroutine, so a slow start never delays evaluating other entries.
type StartFunc func(Firing)

// MissedWindow records windows the scheduler decided NOT to run: they elapsed
// while nobody was evaluating and the harness has `catch_up = false`.
type MissedWindow struct {
	Name string
	Spec string
	// First and Last bound the missed windows; Count is how many there were
	// (a floor when the scan hit maxWindowScan).
	First time.Time
	Last  time.Time
	Count int
	// DetectedAt is the wall-clock time the scheduler noticed.
	DetectedAt time.Time
}

// Recorder is where the scheduler reports decisions that started no process.
//
// It is the seam run history (issue #119) fills with a `missed` record. Until
// that exists, LogRecorder makes the miss loud in the daemon log — a window
// that silently never ran is exactly the failure this package exists to end.
type Recorder interface {
	RecordMissed(MissedWindow)
}

// LogRecorder is the interim Recorder: it logs every missed window at warn
// level.
type LogRecorder struct{}

// RecordMissed logs m.
func (LogRecorder) RecordMissed(m MissedWindow) {
	log.Warn("scheduled run MISSED: windows elapsed while the daemon was not evaluating (suspend, outage, or clock jump)",
		"harness", m.Name,
		"schedule", m.Spec,
		"first", m.First.Format(time.RFC3339),
		"last", m.Last.Format(time.RFC3339),
		"windows", m.Count,
		"detected_at", m.DetectedAt.Format(time.RFC3339),
		"hint", "set catch_up = true to run once after a missed window",
	)
}

// Mark is a scheduled harness's durable position: every window at or before
// DecidedThrough has been decided — fired, caught up, or recorded missed — and
// must never be decided again. It is what lets a daemon started after a window
// tell "missed while down" from "not due yet", and what keeps a daemon that
// crashes right after firing from firing the same window on restart.
type Mark struct {
	// Spec is the expression the mark was taken under. A mark whose Spec no
	// longer matches the config is discarded: windows of the old expression
	// say nothing about the new one.
	Spec           string
	DecidedThrough time.Time
	// LastRunAt is when the scheduler last started a run (zero if never).
	LastRunAt time.Time
}

// Store persists Marks (ADR-0007: the daemon's state.json).
type Store interface {
	// LoadMark returns the persisted mark for name, if any.
	LoadMark(name string) (Mark, bool)
	// UpdateMarks durably drops forget, then writes put, before returning.
	// Synchronous on purpose: a mark must be on disk before the run it
	// accounts for starts, or a crash in between fires the window twice.
	UpdateMarks(put map[string]Mark, forget []string) error
}

// Options configure a Scheduler. Only Start is required.
type Options struct {
	Start StartFunc
	// Clock defaults to the real wall clock.
	Clock Clock
	// Location is the zone an expression without a CRON_TZ=/TZ= prefix is
	// evaluated in. Defaults to time.Local.
	Location *time.Location
	// Store defaults to none: marks live in memory for the process lifetime.
	Store Store
	// Recorder defaults to LogRecorder.
	Recorder Recorder
	// NextChanged, if set, is told each time an entry's next window moves:
	// armed, re-armed by a reload, advanced past a decision, or disarmed (a
	// zero time). It runs after the scheduler's lock is released, so it may
	// call back in. The daemon relays it as job_schedule_changed.
	NextChanged func(name string, next time.Time)
}

// entry is one armed schedule.
type entry struct {
	spec      string
	sched     cron.Schedule
	catchUp   bool
	next      time.Time // next window due; zero if the expression never fires again
	decided   time.Time // every window at or before this has been decided
	lastRunAt time.Time
}

// Scheduler evaluates scheduled harnesses against the wall clock. Apply
// reconciles entries incrementally against a config, so an unchanged entry
// keeps its phase across reloads (an "@every 6h" countdown is not reset by a
// config rewrite that didn't touch it). It is safe for concurrent use.
type Scheduler struct {
	start    StartFunc
	clock    Clock
	loc      *time.Location
	store    Store
	recorder Recorder
	// nextChanged is Options.NextChanged.
	nextChanged func(name string, next time.Time)

	mu       sync.Mutex
	entries  map[string]*entry
	order    []string // config order, so evaluation is deterministic
	lastTick time.Time

	runMu   sync.Mutex
	stop    chan struct{}
	done    chan struct{}
	closed  bool
	firings sync.WaitGroup
}

// New creates a Scheduler. Call Apply to register entries and Start to begin
// evaluating.
func New(opts Options) *Scheduler {
	s := &Scheduler{
		start:    opts.Start,
		clock:    opts.Clock,
		loc:      opts.Location,
		store:    opts.Store,
		recorder: opts.Recorder,
		entries:  make(map[string]*entry),

		nextChanged: opts.NextChanged,
	}
	if s.start == nil {
		s.start = func(Firing) {}
	}
	if s.clock == nil {
		s.clock = realClock{}
	}
	if s.loc == nil {
		s.loc = time.Local
	}
	if s.recorder == nil {
		s.recorder = LogRecorder{}
	}
	return s
}

// now reads the wall clock.
//
// Round(0) strips Go's monotonic clock reading, and that is load-bearing, not
// tidiness: comparisons between two times that both carry one use the
// monotonic clock, which does not advance while a laptop is suspended. A
// window computed by adding to a monotonic time (robfig's `@every` does
// exactly that) would then come due hours late after a wake — the precise bug
// the wall-clock tick exists to prevent.
func (s *Scheduler) now() time.Time {
	return s.clock.Now().Round(0)
}

// Apply reconciles the scheduler's entries against the given config: it adds
// entries for harnesses with a non-empty Schedule, removes entries for
// harnesses that lost their schedule (or disappeared), and re-registers
// entries whose spec changed. Unchanged entries keep their next window, so
// their phase survives reloads; a changed `catch_up` updates in place. Harnesses
// without a schedule are ignored.
//
// A newly armed entry resumes from its persisted mark when that mark was taken
// under the same expression, so windows that elapsed while the daemon was down
// come due on the first tick — the same path a wake takes. With no usable mark
// it arms from now and persists that, so the NEXT outage is detectable.
func (s *Scheduler) Apply(cfg *core.Config) {
	var changes []nextChange
	// Deferred before the unlock below, so it runs after it.
	defer func() { s.notifyNext(changes) }()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()

	var forget []string
	disarmed := make(map[string]bool)
	for name, e := range s.entries {
		h, ok := cfg.Harnesses[name]
		if ok && h.Schedule == e.spec {
			if e.catchUp != h.CatchUp {
				e.catchUp = h.CatchUp
				log.Info("schedule catch_up changed", "harness", name, "catch_up", h.CatchUp)
			}
			continue
		}
		delete(s.entries, name)
		forget = append(forget, name)
		disarmed[name] = true
		if !ok || h.Schedule == "" {
			log.Info("unscheduled harness", "harness", name)
		}
	}

	put := make(map[string]Mark)
	order := make([]string, 0, len(cfg.HarnessOrder))
	for _, name := range cfg.HarnessOrder {
		h := cfg.Harnesses[name]
		if h.Schedule == "" {
			continue
		}
		if _, ok := s.entries[name]; ok {
			order = append(order, name)
			continue // unchanged: keep its phase
		}
		sched, err := cron.ParseStandard(h.Schedule)
		if err != nil {
			// Defense in depth: config validation already rejects invalid
			// specs at parse time (registerHarness).
			log.Error("invalid schedule, skipping harness", "harness", name, "schedule", h.Schedule, "err", err)
			continue
		}
		e := &entry{spec: h.Schedule, sched: sched, catchUp: h.CatchUp}
		from := now
		if m, ok := s.loadMark(name); ok && m.Spec == h.Schedule && !m.DecidedThrough.IsZero() {
			from = m.DecidedThrough.Round(0)
			e.lastRunAt = m.LastRunAt
			if from.After(now.Add(LateGrace)) {
				log.Warn("schedule mark is in the future; the wall clock moved backwards while the daemon was down, so decided windows will not fire again",
					"harness", name, "decided_through", from.Format(time.RFC3339))
			}
		} else {
			put[name] = Mark{Spec: h.Schedule, DecidedThrough: now}
		}
		e.decided = from
		e.next = s.nextAfter(e, from)
		s.entries[name] = e
		order = append(order, name)
		delete(disarmed, name) // a changed spec re-arms rather than disarms
		changes = append(changes, nextChange{name: name, next: e.next})
		log.Info("scheduled harness", "harness", name, "schedule", h.Schedule, "catch_up", h.CatchUp, "next", formatNext(e.next))
	}
	s.order = order
	gone := make([]string, 0, len(disarmed))
	for name := range disarmed {
		gone = append(gone, name)
	}
	slices.Sort(gone)
	for _, name := range gone {
		changes = append(changes, nextChange{name: name})
	}
	s.persist(put, forget)
}

// NextFire reports when the named harness's schedule next comes due. The zero
// bool means the harness has no schedule registered, or its expression never
// fires again.
func (s *Scheduler) NextFire(name string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[name]
	if !ok || e.next.IsZero() {
		return time.Time{}, false
	}
	return e.next, true
}

// Start begins evaluating: once immediately — a daemon that boots after a
// window it was down for decides it now, exactly as a wake would — and then on
// every tick. Entries applied while running take effect on the next tick.
// Start after Close, or a second Start, is a no-op.
func (s *Scheduler) Start() {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	if s.closed || s.stop != nil {
		return
	}
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	ticks, stopTicker := s.clock.NewTicker(TickInterval)
	go func() {
		defer close(s.done)
		defer stopTicker()
		s.evaluate()
		for {
			select {
			case <-s.stop:
				return
			case <-ticks:
				s.evaluate()
			}
		}
	}()
}

// Close stops evaluating and waits for in-progress firings to finish. It is
// idempotent.
func (s *Scheduler) Close() {
	s.runMu.Lock()
	if !s.closed && s.stop != nil {
		close(s.stop)
		<-s.done
	}
	s.closed = true
	s.runMu.Unlock()
	s.firings.Wait()
}

// evaluate is one tick: decide every entry whose window has come due.
func (s *Scheduler) evaluate() {
	now := s.now()

	s.mu.Lock()
	if !s.lastTick.IsZero() {
		switch gap := now.Sub(s.lastTick); {
		case gap < -backwardsSlack:
			// Decided windows never fire again (every mark and next window is
			// an absolute time after them), so a backwards jump delays the
			// next run rather than repeating the last one.
			log.Warn("wall clock moved backwards; already-decided windows will not fire again", "by", (-gap).Round(time.Second))
		case gap > LateGrace:
			log.Info("wall clock jumped forward (suspend, stalled daemon, or clock change); re-evaluating schedules", "by", gap.Round(time.Second))
		}
	}
	s.lastTick = now

	var (
		put     map[string]Mark
		fires   []Firing
		misses  []MissedWindow
		changes []nextChange
	)
	for _, name := range s.order {
		e := s.entries[name]
		if e == nil || e.next.IsZero() || now.Before(e.next) {
			continue
		}
		fire, miss := s.decide(name, e, now)
		changes = append(changes, nextChange{name: name, next: e.next})
		if put == nil {
			put = make(map[string]Mark)
		}
		if fire != nil {
			e.lastRunAt = now
			fires = append(fires, *fire)
		}
		if miss != nil {
			misses = append(misses, *miss)
		}
		put[name] = Mark{Spec: e.spec, DecidedThrough: e.decided, LastRunAt: e.lastRunAt}
	}
	// The marks go to disk before any run starts (see Store.UpdateMarks),
	// still under mu so a concurrent Apply cannot forget a mark this write
	// would then resurrect.
	s.persist(put, nil)
	s.mu.Unlock()

	s.notifyNext(changes)

	for _, m := range misses {
		s.safely("record missed window", m.Name, func() { s.recorder.RecordMissed(m) })
	}
	for _, f := range fires {
		s.dispatch(f)
	}
}

// decide resolves one due entry at now, advancing its next window past every
// elapsed one. It returns the run to start (if any) and the windows to record
// as missed (if any).
//
// The most recent elapsed window decides: if it is still within LateGrace it
// runs on time, and any older windows behind it that nobody evaluated are
// recorded missed (unless catch_up, in which case this one run already stands
// in for them). If even the most recent window is stale, `catch_up = true`
// runs exactly once for all of them and `catch_up = false` records exactly one
// miss covering all of them.
func (s *Scheduler) decide(name string, e *entry, now time.Time) (*Firing, *MissedWindow) {
	first := e.next
	last := first
	count := 1
	stale, lastStale := 0, time.Time{}
	if now.Sub(first) > LateGrace {
		stale, lastStale = 1, first
	}
	next := s.nextAfter(e, first)
	capped := false
	for !next.IsZero() && !next.After(now) {
		if count >= maxWindowScan {
			capped = true
			break
		}
		last = next
		count++
		if now.Sub(next) > LateGrace {
			stale, lastStale = stale+1, next
		}
		next = s.nextAfter(e, next)
	}
	if capped {
		// Re-arm from now: the windows past the scan limit are behind us too.
		e.next = s.nextAfter(e, now)
		e.decided = now
	} else {
		e.next = next
		e.decided = last
	}

	late := now.Sub(last)
	switch {
	case late <= LateGrace:
		fire := &Firing{Name: name, Trigger: TriggerSchedule, Window: last, Late: late}
		if stale > 0 && !e.catchUp {
			return fire, &MissedWindow{Name: name, Spec: e.spec, First: first, Last: lastStale, Count: stale, DetectedAt: now}
		}
		return fire, nil
	case e.catchUp:
		return &Firing{Name: name, Trigger: TriggerCatchUp, Window: last, Late: late, Missed: stale}, nil
	default:
		return nil, &MissedWindow{Name: name, Spec: e.spec, First: first, Last: last, Count: stale, DetectedAt: now}
	}
}

// dispatch starts f on its own goroutine, so a slow or blocked start cannot
// delay the tick that evaluates every other entry. Close waits for it.
func (s *Scheduler) dispatch(f Firing) {
	s.firings.Add(1)
	go func() {
		defer s.firings.Done()
		s.safely("firing", f.Name, func() { s.start(f) })
	}()
}

// safely runs fn, recovering a panic so a bad callback cannot take down the
// daemon and every harness it supervises (SPEC-0008 REQ "Scheduler Fault
// Isolation").
func (s *Scheduler) safely(what, name string, fn func()) {
	defer func() {
		if r := recover(); r != nil {
			log.Error("scheduler: recovered panic", "in", what, "harness", name, "panic", r)
		}
	}()
	fn()
}

// loadMark reads a persisted mark, tolerating no store.
func (s *Scheduler) loadMark(name string) (Mark, bool) {
	if s.store == nil {
		return Mark{}, false
	}
	return s.store.LoadMark(name)
}

// persist writes marks through the store. A failed write is logged and the
// decision stands: running the job matters more than the at-most-once
// guarantee a lost mark weakens.
func (s *Scheduler) persist(put map[string]Mark, forget []string) {
	if s.store == nil || (len(put) == 0 && len(forget) == 0) {
		return
	}
	if err := s.store.UpdateMarks(put, forget); err != nil {
		log.Error("scheduler: could not persist schedule marks; a crash now may repeat or lose a window", "err", err)
	}
}

// nextChange is one entry's new next window, for NextChanged. A zero next means
// the entry was disarmed.
type nextChange struct {
	name string
	next time.Time
}

// notifyNext reports changes to NextChanged. Called without the lock held.
func (s *Scheduler) notifyNext(changes []nextChange) {
	if s.nextChanged == nil {
		return
	}
	for _, c := range changes {
		s.safely("next-window callback", c.name, func() { s.nextChanged(c.name, c.next) })
	}
}

// formatNext renders a next window for logs.
func formatNext(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.Format(time.RFC3339)
}

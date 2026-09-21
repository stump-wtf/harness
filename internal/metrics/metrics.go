// Package metrics is the daemon's Prometheus surface: GET /metrics, led by
// the series that tell "running" apart from "able to work".
//
// # Prometheus Metrics
//
// On 2026-09-14 a provider quota emptied and four supervised agents failed
// every model call for twenty hours while `harness list` called them running.
// The supervisor was right — it watches processes — and that is the gap:
// a process can be up while the provider refuses it. This package puts both
// facts on one scrape so an alert can join them (ADR-0020):
//
//	time() - harness_last_successful_call_timestamp > 900
//	  and on(instance, harness) harness_harness_state{state="running"} == 1
//
// The on() clause is not optional: the state series carries an extra `state`
// label, and a bare `and` matches only identical label sets, so without it the
// expression never fires.
//
// Two families, two mechanisms (design.md "Where the numbers come from"):
//
//   - Supervisor state — harness state, restarts, consecutive failures,
//     session activity, next scheduled run — is read at scrape time from the
//     Manager's snapshots and emitted as const metrics. Nothing is mirrored
//     into a gauge, so no transition can forget to update one and leave a
//     graph permanently, plausibly wrong.
//   - Event counters — model calls and their error classes, sessions started,
//     state transitions, scheduled-run outcomes — are counted as the events
//     arrive: agent items from the observer (internal/observe), lifecycle
//     events from the Manager's bus. Both feeds are lossy fan-outs that never
//     block the supervisor (ADR-0007); loss on the observer feed is counted in
//     harness_metrics_collection_errors_total rather than hidden.
//
// Honest absence (SPEC-0013 REQ-6) runs through all of it: a value the daemon
// cannot compute is omitted, never zeroed. A harness whose adapter writes no
// transcript the observer can read (generic, or no workdir to attribute
// sessions against) has no model-call series at all, rather than a confident
// zero; a harness that never succeeded has no last-success timestamp, rather
// than 1970.
//
// Governing: ADR-0020; SPEC-0013 REQ-1..REQ-6; ADR-0007 (never block the
// supervisor on a slow consumer); ADR-0008 (no credentials, prompts or
// environment in any label).
//
// @joestump-agent 09/21/2026 - Added for harness#356.
package metrics

import (
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/observe"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// Source is what the collector reads from the supervisor. *supervisor.Manager
// satisfies it.
type Source interface {
	Snapshots() []supervisor.Snapshot
	HarnessDef(name string) (core.Harness, bool)
	Events() (<-chan supervisor.Event, func())
}

// EventSource is the agent event feed. *observe.Observer satisfies it.
type EventSource interface {
	Subscribe(name string, buf int) (<-chan observe.Event, func())
	Stats() observe.Stats
}

// Defaults for the zero Options fields.
const (
	// DefaultSessionIdle is how long after its latest agent item a harness
	// still counts as working (harness_session_active = 1). Ten minutes is
	// longer than one slow turn — an extended-thinking generation or a long
	// tool run writes nothing until it finishes — plus the observer's poll
	// lag, so a busy agent does not flicker to idle between items; and short
	// enough that "idle while the queue has work" shows up well inside the
	// fifteen-minute staleness window the ADR's alert uses.
	DefaultSessionIdle = 10 * time.Minute
	// DefaultSessionMemory is how long a session key is remembered after its
	// latest item, so its next item is not counted as a new session. A
	// session silent for a day that wakes up again counts as started again;
	// remembering keys forever would grow without bound on a daemon that
	// runs a scheduled job every few minutes for months.
	DefaultSessionMemory = 24 * time.Hour
)

// subscriberName is this package's name on the observer, and the label its
// drops carry in harness_observer_events_dropped_total.
const subscriberName = "metrics"

// subscriberBuffer is deep enough to absorb one scan's burst (a harness
// replaying a long tool sequence) while the collector goroutine, which only
// increments counters, drains it.
const subscriberBuffer = 1024

// Collector names for harness_metrics_collection_errors_total{collector}.
const (
	collectorSupervisor = "supervisor" // scrape-time snapshot read
	collectorSchedule   = "schedule"   // scrape-time next-run read
	collectorObserver   = "observer"   // the agent event feed
	collectorLifecycle  = "lifecycle"  // the Manager's lifecycle bus
)

var collectorNames = []string{collectorSupervisor, collectorSchedule, collectorObserver, collectorLifecycle}

// Options configures Metrics. The zero value is production defaults with no
// observer and no schedule reader.
type Options struct {
	// Observer is the agent event feed. Nil omits every model-reachability
	// and session series: without it they cannot be computed (REQ-6).
	Observer EventSource
	// NextRun reports a scheduled harness's next window (the scheduler's
	// NextFire). Nil omits harness_scheduled_next_run_timestamp.
	NextRun func(name string) (time.Time, bool)
	// MaxHarnesses caps distinct harness label values (default
	// DefaultMaxHarnesses).
	MaxHarnesses int
	// SessionIdle is the harness_session_active window (default
	// DefaultSessionIdle).
	SessionIdle time.Duration
	// Now is the clock (default time.Now).
	Now func() time.Time
}

func (o Options) withDefaults() Options {
	if o.MaxHarnesses <= 0 {
		o.MaxHarnesses = DefaultMaxHarnesses
	}
	if o.SessionIdle <= 0 {
		o.SessionIdle = DefaultSessionIdle
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	return o
}

// The four values of harness_harness_state (SPEC-0013 REQ-2).
const (
	stateRunning  = "running"
	stateStopped  = "stopped"
	stateFailed   = "failed"
	stateFlapping = "flapping"
)

var stateValues = []string{stateRunning, stateStopped, stateFailed, stateFlapping}

// StateValue maps a snapshot onto SPEC-0013's four state values. The
// supervisor has seven states (SPEC-0003); the spec exposes four, so the
// mapping is fixed here, in priority order:
//
//   - failed:   core failed. Terminal, needs a human; it wins over a lingering
//     flapping flag because it is the state an operator alerts on by equality.
//   - flapping: the crash-loop flag is set, or core degraded (restarts
//     escalating under backoff) — whichever of running/restarting the process
//     happens to be in at the instant of the scrape.
//   - running:  core running or starting. Starting is the spawn in progress,
//     a moment long; calling it anything else would make every start blip.
//   - stopped:  a harness held by its operating hours (SPEC-0012), whatever
//     else the snapshot carries; then core stopped, stopping, or restarting
//     outside a crash loop — no process is up at this instant (restarting is
//     the restart_delay wait before a respawn). A harness failing too slowly
//     to trip the crash window shows here, alternating with running, while
//     harness_consecutive_failures climbs toward give-up.
//
// Held sits below failed and above everything else. The gate shuts a held
// harness down on purpose, so a crash-loop flag or a degraded state caught
// mid-hold is history, not a fault, and must not read as flapping. The
// supervisor never holds a failed harness (hold refuses one, and every start
// clears the hold), so held-and-failed cannot arise; were it to, failed is
// the one that needs a human. Held is not a fifth state value: REQ-2's enum is
// fixed, so held reads as stopped here.
//
// @joestump-agent 09/21/2026 - Held (SPEC-0012, arrived on rebase) maps to
// stopped explicitly (review, harness#356).
func StateValue(s supervisor.Snapshot) string {
	switch {
	case s.State == core.StateFailed:
		return stateFailed
	case s.Held:
		return stateStopped
	case s.Flapping || s.State == core.StateDegraded:
		return stateFlapping
	case s.State == core.StateRunning || s.State == core.StateStarting:
		return stateRunning
	default:
		return stateStopped
	}
}

// Observable reports whether the observer can attribute model calls to h: its
// adapter writes a transcript agent-trace parses (runtrace.Sources) and it has
// a workdir to correlate sessions against (runtrace.ErrNoWorkdir). For any
// other harness the model-call series are uncomputable, so they are omitted
// rather than reported as a zero that looks like a healthy, idle agent.
func Observable(h core.Harness) bool {
	switch h.Adapter {
	case "crush", "claude-code", "codex":
		return h.Workdir != ""
	}
	return false
}

// Outcomes of harness_model_calls_total and harness_scheduled_runs_total.
const (
	outcomeSuccess = "success"
	outcomeError   = "error"
	outcomeFailure = "failure"
)

// series is the counted state behind one harness label value.
type series struct {
	calls           [2]uint64 // success, error
	errors          map[Class]uint64
	unclassified    uint64
	lastSuccess     time.Time // zero until the first success: omitted, not 0
	lastItem        time.Time // latest agent item of any kind
	sessionsStarted uint64
	transitions     map[core.State]uint64
	runs            [2]uint64 // success, failure
}

func newSeries() *series {
	return &series{errors: make(map[Class]uint64), transitions: make(map[core.State]uint64)}
}

// Metrics owns the registry and the counters behind it. Build it with New,
// feed it with Start, serve Handler.
type Metrics struct {
	src  Source
	opts Options
	reg  *prometheus.Registry

	mu        sync.Mutex
	labels    *labeler
	per       map[string]*series   // by harness label value
	sessions  map[string]time.Time // harness + session key → latest item
	lastPrune time.Time
	collErrs  map[string]uint64
	// observerDead is set when the agent event feed closed while Metrics was
	// still running: model reachability can no longer be computed and is
	// omitted from then on, rather than frozen into a plausible flat line.
	observerDead bool
	// droppedSeen is the observer's drop count for this subscriber as of the
	// previous scrape; the delta becomes collection errors.
	droppedSeen uint64

	started bool
	closed  bool
	cancels []func()
	wg      sync.WaitGroup
}

// New builds Metrics over src. Harnesses declared now are granted label slots
// in config order before anything else can claim one.
func New(src Source, opts Options) *Metrics {
	opts = opts.withDefaults()
	m := &Metrics{
		src:      src,
		opts:     opts,
		reg:      prometheus.NewRegistry(),
		labels:   newLabeler(opts.MaxHarnesses),
		per:      make(map[string]*series),
		sessions: make(map[string]time.Time),
		collErrs: make(map[string]uint64),
	}
	for _, n := range collectorNames {
		m.collErrs[n] = 0
	}
	if snaps, ok := m.snapshots(); ok {
		for _, s := range snaps {
			m.labels.label(s.Name)
		}
	}
	// A private registry, not the global default: nothing a dependency
	// registers behind our back reaches the scrape, and tests get a clean
	// registry per instance. Go and process collectors are mandatory — the
	// daemon is long-lived and its own growth is part of reading the rest
	// (REQ-1).
	m.reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		&collector{m: m},
	)
	return m
}

// Registry is the private registry /metrics serves.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

// Handler serves the registry in Prometheus text format. A collector that
// fails mid-scrape still yields the rest of the scrape (ContinueOnError); the
// failure itself is counted by harness_metrics_collection_errors_total.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.reg, promhttp.HandlerOpts{ErrorHandling: promhttp.ContinueOnError})
}

// Start subscribes to the lifecycle bus and the observer, synchronously — so
// nothing published after Start returns is missed — and begins counting.
// Calling it again, or after Close, does nothing.
func (m *Metrics) Start() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started || m.closed {
		return
	}
	m.started = true

	evs, cancelEvs := m.src.Events()
	m.cancels = append(m.cancels, cancelEvs)
	m.wg.Add(1)
	go m.consumeLifecycle(evs)

	if m.opts.Observer != nil {
		items, cancelItems := m.opts.Observer.Subscribe(subscriberName, subscriberBuffer)
		m.cancels = append(m.cancels, cancelItems)
		m.wg.Add(1)
		go m.consumeItems(items)
	}
}

// Close unsubscribes and waits for the consumers to finish. Idempotent.
func (m *Metrics) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	cancels := m.cancels
	m.cancels = nil
	m.mu.Unlock()
	for _, c := range cancels {
		c()
	}
	m.wg.Wait()
}

// consumeLifecycle counts state transitions and scheduled-run outcomes.
func (m *Metrics) consumeLifecycle(ch <-chan supervisor.Event) {
	defer m.wg.Done()
	for ev := range ch {
		m.lifecycle(ev)
	}
	m.feedClosed(collectorLifecycle)
}

// consumeItems counts model calls and sessions from the observer.
func (m *Metrics) consumeItems(ch <-chan observe.Event) {
	defer m.wg.Done()
	for ev := range ch {
		m.item(ev)
	}
	m.feedClosed(collectorObserver)
}

// feedClosed records a feed that ended while Metrics was still running: a
// collector that has gone blind, which REQ-6 says must be visible.
func (m *Metrics) feedClosed(collector string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return
	}
	m.collErrs[collector]++
	if collector == collectorObserver {
		m.observerDead = true
	}
}

// seriesFor returns the series behind name's label. Callers hold m.mu.
func (m *Metrics) seriesFor(name string) *series {
	lbl := m.labels.label(name)
	s, ok := m.per[lbl]
	if !ok {
		s = newSeries()
		m.per[lbl] = s
	}
	return s
}

// lifecycle applies one Manager bus event.
func (m *Metrics) lifecycle(ev supervisor.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch ev.Kind {
	case supervisor.EventStateChanged:
		m.seriesFor(ev.Name).transitions[ev.To]++
	case supervisor.EventRunFinished:
		// Only runs that pass a verdict on the job count, the same rule as
		// supervisor.ConsecutiveFailures: skipped, missed, replaced,
		// cancelled and interrupted runs say nothing about whether it works.
		switch ev.Run.Outcome {
		case supervisor.OutcomeSuccess:
			m.seriesFor(ev.Name).runs[0]++
		case supervisor.OutcomeFailed, supervisor.OutcomeTimedOut:
			m.seriesFor(ev.Name).runs[1]++
		}
	}
}

// item applies one observer event.
//
// A tool call is a model call that succeeded: the model answered with work.
// An error mark is one that failed. Other marks (user messages, compactions,
// subagent launches) are activity, not calls. A turn that ends in plain text
// with no tool call is invisible here — agent-trace records no mark for it —
// so an agent that only chats reads as idle; the workers this exists for act
// through tools.
func (m *Metrics) item(ev observe.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.seriesFor(ev.Harness)
	if ev.Time.After(s.lastItem) {
		s.lastItem = ev.Time
	}

	key := ev.Session.Key
	if key == "" {
		key = ev.Session.Path + "#" + ev.Session.ID
	}
	key = ev.Harness + "\x00" + key
	if _, seen := m.sessions[key]; !seen {
		s.sessionsStarted++
	}
	now := m.opts.Now()
	m.sessions[key] = now
	if now.Sub(m.lastPrune) > time.Hour {
		for k, at := range m.sessions {
			if now.Sub(at) > DefaultSessionMemory {
				delete(m.sessions, k)
			}
		}
		m.lastPrune = now
	}

	switch ev.Kind {
	case observe.KindTool:
		s.calls[0]++
		// Never backwards: an item delivered late must not rewind the
		// timestamp an alert measures staleness from.
		if ev.Time.After(s.lastSuccess) {
			s.lastSuccess = ev.Time
		}
	case observe.KindMark:
		if ev.Mark.Type != "error" {
			return
		}
		s.calls[1]++
		class, known := Classify(ev.Adapter, ev.Mark.Note)
		s.errors[class]++
		if !known {
			s.unclassified++
		}
	}
}

// snapshots reads the Manager, turning a panic into a collection error rather
// than a dead scrape.
func (m *Metrics) snapshots() (snaps []supervisor.Snapshot, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			m.countError(collectorSupervisor)
			snaps, ok = nil, false
		}
	}()
	return m.src.Snapshots(), true
}

func (m *Metrics) harnessDef(name string) (h core.Harness, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			m.countError(collectorSupervisor)
			h, ok = core.Harness{}, false
		}
	}()
	return m.src.HarnessDef(name)
}

func (m *Metrics) nextRun(name string) (t time.Time, ok bool) {
	if m.opts.NextRun == nil {
		return time.Time{}, false
	}
	defer func() {
		if r := recover(); r != nil {
			m.countError(collectorSchedule)
			t, ok = time.Time{}, false
		}
	}()
	return m.opts.NextRun(name)
}

func (m *Metrics) observerStats() (st observe.Stats, ok bool) {
	if m.opts.Observer == nil {
		return observe.Stats{}, false
	}
	defer func() {
		if r := recover(); r != nil {
			m.countError(collectorObserver)
			st, ok = observe.Stats{}, false
		}
	}()
	return m.opts.Observer.Stats(), true
}

func (m *Metrics) countError(collector string) {
	m.mu.Lock()
	m.collErrs[collector]++
	m.mu.Unlock()
}

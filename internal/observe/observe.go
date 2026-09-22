// Package observe watches the agent sessions supervised harnesses write and
// fans each new tool call and mark out to in-daemon consumers.
//
// # Agent Event Observer
//
// The supervisor watches processes; it cannot see that a running crush is
// failing every model call. The only record of that is the agent's own
// transcript, which agent-trace parses. This package polls those transcripts
// from inside the daemon, attributes each session to the one harness that could
// have written it (the runtrace rule, fail-closed), and delivers what is new —
// tool calls and marks — to subscribers: the Prometheus collector (SPEC-0013)
// and the telemetry exporters (issue #391) are the intended ones.
//
// It deliberately does not use agent-trace's tail.Watcher. The Watcher parks a
// mark until a tool call arrives to carry it, and during a provider outage
// there are only error marks and no tool calls — the 2026-09-14 outage would
// have produced no events at all. Here marks travel on their own, and delivery
// never waits on a subscriber: a full subscriber loses the event, and the loss
// is counted (ADR-0007: never block the supervisor on a slow consumer).
//
// agent-trace's incremental reads also never pass a tool call with no result,
// so a crush killed mid-call and resumed into the same session would pin its
// watermark there forever. stall.go detects that — cheaply, and only for
// sessions that wrote something while held — and recovers past the orphaned
// call until agent-trace releases superseded calls itself. Its known limits:
// a stall is proven only by a mark (a user message, a provider error) written
// after the orphan, which every resume writes; the orphaned call is never
// delivered; and adapters other than crush, Claude Code and Codex get no
// fallback.
//
// Governing: issue #390; SPEC-0006 REQ "Run Correlation"; SPEC-0013 REQ-3
// (model reachability is observed here); ADR-0007; ADR-0008 (every string
// that came from a transcript is redacted before it leaves this package).
//
// @joestump-agent 09/21/2026 - Added for harness#390.
//
// @joestump-agent 09/21/2026 - Orphaned-tool-call fallback (stall.go), and a
// forgotten session now resumes from its read cursor, so a tool call open
// longer than the listing window plus ForgetAfter is delivered, not dropped.
package observe

import (
	"context"
	"os"
	"sync"
	"time"

	"charm.land/log/v2"
	"github.com/stump-wtf/agent-trace/classify"
	"github.com/stump-wtf/agent-trace/tail"

	"github.com/stump-wtf/harness/internal/core"
	"github.com/stump-wtf/harness/internal/runtrace"
	"github.com/stump-wtf/harness/internal/supervisor"
)

// Kind says which of Event's payloads is valid.
type Kind string

const (
	// KindTool is a classified tool call; Event.Tool is valid.
	KindTool Kind = "tool"
	// KindMark is a non-tool annotation — a user message, a compaction, a
	// subagent launch, an agent error; Event.Mark is valid.
	KindMark Kind = "mark"
)

// Event is one agent-trace item attributed to a supervised harness.
//
// Every string in it that came from a transcript (Tool.Summary, target paths,
// Mark.Note, Session.Title) has been through redact.String: transcripts record
// commands verbatim, credentials included, and a consumer that ships events
// off the host must not be the place that remembers to mask them.
type Event struct {
	// Harness is the attributed harness name, never empty.
	Harness string
	// Adapter is the harness's adapter name ("crush", "claude-code", …).
	Adapter string
	Session tail.SessionMeta
	Kind    Kind
	// Tool is valid when Kind == KindTool.
	Tool classify.Event
	// Mark is valid when Kind == KindMark.
	Mark classify.Mark
	// Time is the item's own timestamp, or ObservedAt when it has none.
	Time time.Time
	// ObservedAt is when the observer read the item.
	ObservedAt time.Time
}

// Source is what the observer needs from the supervisor. *supervisor.Manager
// satisfies it; both calls are short snapshot reads under the Manager's lock.
type Source interface {
	Snapshots() []supervisor.Snapshot
	HarnessRecord(name string) (core.Harness, string, bool)
}

// Defaults for the zero Options fields.
const (
	// DefaultPollInterval bounds how late an event is. Five seconds is well
	// inside a scrape interval and costs one short query or tail read per
	// live session.
	DefaultPollInterval = 5 * time.Second
	// DefaultForgetAfter is how long a session may go without new content
	// before its state is dropped. Discovery lists only sessions active inside
	// this window, so it is also the lookback of every listing.
	DefaultForgetAfter = time.Hour
	// DefaultTombstoneLimit caps how many forgotten sessions are remembered
	// well enough not to replay what was already delivered if they wake up
	// again. A tombstone is a key, a time and a read cursor (a path and a few
	// integers); ten thousand is a few megabytes at most and far more sessions
	// than one daemon lifetime produces.
	DefaultTombstoneLimit = 10000
	// DefaultSourceTimeout bounds one store's listing, and one session's read,
	// so a store locked by a wedged writer cannot starve the others.
	DefaultSourceTimeout = 10 * time.Second
)

// Options configures an Observer. The zero value is production.
type Options struct {
	// PollInterval is the time between scans (default DefaultPollInterval).
	PollInterval time.Duration
	// ForgetAfter is how long an unchanged session is tracked (default
	// DefaultForgetAfter).
	ForgetAfter time.Duration
	// TombstoneLimit caps remembered forgotten sessions; the oldest go first
	// (default DefaultTombstoneLimit).
	TombstoneLimit int
	// SourceTimeout bounds each store listing and each session read (default
	// DefaultSourceTimeout).
	SourceTimeout time.Duration
	// StallCheckInterval is the least time between two full parses of one
	// session suspected of being pinned behind an orphaned tool call (default
	// DefaultStallCheckInterval; see stall.go).
	StallCheckInterval time.Duration
	// Since is the history floor: nothing timestamped before it (less
	// runtrace.Slack) is delivered. Zero means the moment New is called, which
	// in the daemon is just after Autostart.
	Since time.Time
	// Now is the clock (default time.Now).
	Now func() time.Time
	// Logger receives debug and warning lines (default log.Default()).
	Logger *log.Logger
	// DaemonDir is the working directory a harness with no workdir is spawned
	// in; such a harness is a possible author of sessions there, never an
	// attributed one (default os.Getwd()).
	DaemonDir string

	// Sources resolves a harness's agent-trace stores (default
	// runtrace.Sources). A seam for hermetic tests.
	Sources func(runtrace.Scope) ([]tail.Adapter, error)
	// DiscoveryEnv reads the environment keys that relocate a tool's store
	// from a harness's env_file layered over the daemon's environment
	// (default supervisor.DiscoveryEnv with runtrace.DiscoveryEnvKeys).
	DiscoveryEnv func(core.Harness) map[string]string
}

func (o Options) withDefaults() Options {
	if o.PollInterval <= 0 {
		o.PollInterval = DefaultPollInterval
	}
	if o.ForgetAfter <= 0 {
		o.ForgetAfter = DefaultForgetAfter
	}
	if o.TombstoneLimit <= 0 {
		o.TombstoneLimit = DefaultTombstoneLimit
	}
	if o.SourceTimeout <= 0 {
		o.SourceTimeout = DefaultSourceTimeout
	}
	if o.StallCheckInterval <= 0 {
		o.StallCheckInterval = DefaultStallCheckInterval
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = log.Default()
	}
	if o.DaemonDir == "" {
		o.DaemonDir, _ = os.Getwd()
	}
	if o.Sources == nil {
		o.Sources = runtrace.Sources
	}
	if o.DiscoveryEnv == nil {
		o.DiscoveryEnv = func(h core.Harness) map[string]string {
			// An unreadable env_file still yields the daemon-environment
			// values; discovering with those is the best available answer, and
			// the supervisor already reports the broken file at spawn.
			env, _ := supervisor.DiscoveryEnv(h, runtrace.DiscoveryEnvKeys)
			return env
		}
	}
	return o
}

// Stats is a point-in-time copy of the observer's counters. Counters are
// monotonic for the observer's lifetime.
type Stats struct {
	// Delivered counts events fanned out, once per event however many
	// subscribers received it.
	Delivered uint64
	// Dropped counts, per subscriber name, events that subscriber lost
	// because its buffer was full.
	Dropped map[string]uint64
	// Ambiguous counts items withheld because more than one harness could
	// have written their session.
	Ambiguous uint64
	// Unattributed counts items read from a tracked session that no harness
	// could claim when they were read (its harness had exited, say).
	Unattributed uint64
	// ParseErrors counts failed session reads per agent-trace adapter
	// ("crush", "claude-code", "codex").
	ParseErrors map[string]uint64
	// ScanErrors counts failed store listings.
	ScanErrors uint64
	// StallChecks counts full parses spent on sessions suspected of being
	// pinned behind an orphaned tool call; Stalls counts the checks that found
	// one; OrphansSkipped counts the orphaned records skipped to recover. On a
	// healthy store all three stay zero, and once agent-trace releases
	// superseded calls itself Stalls and OrphansSkipped stay zero everywhere.
	StallChecks    uint64
	Stalls         uint64
	OrphansSkipped uint64
	// Sessions is how many sessions are currently tracked; Contested is how
	// many of those were ambiguous at their latest read.
	Sessions  int
	Contested int
	// LastScan is when the latest scan began; zero before the first.
	LastScan time.Time
}

// subscriber is one Subscribe call's channel.
type subscriber struct {
	name string
	ch   chan Event
}

// Observer polls agent-trace stores for the harnesses a Source declares and
// fans new items out to subscribers. Create it with New.
type Observer struct {
	src  Source
	opts Options
	log  *log.Logger

	// Scan state, touched only by the loop goroutine (or by a test driving
	// scan directly without Start).
	sessions   map[string]*session
	tombstones map[string]tombstone
	summaries  *tail.SummaryCache
	lastSweep  time.Time // when summaries was last swept

	// mu guards everything below. Sends to subscribers happen under it; they
	// never block (select with default), so holding it across a fan-out is
	// bounded, and it is what keeps a send from racing a cancel's close.
	mu      sync.Mutex
	subs    map[*subscriber]struct{}
	stats   Stats
	started bool
	stopped bool

	ctx      context.Context
	cancel   context.CancelFunc
	stopOnce sync.Once
	done     chan struct{}
}

// New builds an observer over src. Start runs it.
func New(src Source, opts Options) *Observer {
	opts = opts.withDefaults()
	if opts.Since.IsZero() {
		opts.Since = opts.Now()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Observer{
		src:        src,
		opts:       opts,
		log:        opts.Logger,
		sessions:   make(map[string]*session),
		tombstones: make(map[string]tombstone),
		summaries:  tail.NewSummaryCache(),
		lastSweep:  opts.Now(),
		subs:       make(map[*subscriber]struct{}),
		stats: Stats{
			Dropped:     make(map[string]uint64),
			ParseErrors: make(map[string]uint64),
		},
		ctx:    ctx,
		cancel: cancel,
		done:   make(chan struct{}),
	}
}

// Start launches the polling loop: one scan immediately, then one per
// PollInterval. Calling it again, or after Stop, does nothing.
func (o *Observer) Start() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.started || o.stopped {
		return
	}
	o.started = true
	go o.loop()
}

// Stop ends the loop, waits for an in-flight scan to abandon its reads, and
// closes every subscriber channel. It is idempotent and safe without Start.
func (o *Observer) Stop() {
	o.stopOnce.Do(func() {
		o.mu.Lock()
		o.stopped = true
		started := o.started
		o.mu.Unlock()
		o.cancel()
		if started {
			<-o.done
		}
		o.mu.Lock()
		for s := range o.subs {
			close(s.ch)
			delete(o.subs, s)
		}
		o.mu.Unlock()
	})
}

func (o *Observer) loop() {
	defer close(o.done)
	t := time.NewTicker(o.opts.PollInterval)
	defer t.Stop()
	for {
		o.scan(o.ctx)
		select {
		case <-o.ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Subscribe registers a consumer. Events are delivered to the returned channel
// without ever blocking: when its buffer (buf, at least 1) is full, the event
// is dropped for this subscriber and counted in Stats.Dropped[name]. The
// cancel function unregisters and closes the channel; Stop closes it too.
// Subscribing after Stop returns an already-closed channel.
func (o *Observer) Subscribe(name string, buf int) (<-chan Event, func()) {
	if buf < 1 {
		buf = 1
	}
	s := &subscriber{name: name, ch: make(chan Event, buf)}
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.stats.Dropped[name]; !ok {
		o.stats.Dropped[name] = 0
	}
	if o.stopped {
		close(s.ch)
		return s.ch, func() {}
	}
	o.subs[s] = struct{}{}
	var once sync.Once
	return s.ch, func() {
		once.Do(func() {
			o.mu.Lock()
			defer o.mu.Unlock()
			if _, ok := o.subs[s]; ok {
				delete(o.subs, s)
				close(s.ch)
			}
		})
	}
}

// Stats returns a copy of the counters.
func (o *Observer) Stats() Stats {
	o.mu.Lock()
	defer o.mu.Unlock()
	st := o.stats
	st.Dropped = make(map[string]uint64, len(o.stats.Dropped))
	for k, v := range o.stats.Dropped {
		st.Dropped[k] = v
	}
	st.ParseErrors = make(map[string]uint64, len(o.stats.ParseErrors))
	for k, v := range o.stats.ParseErrors {
		st.ParseErrors[k] = v
	}
	return st
}

// publish fans events out to every subscriber, in order, without blocking.
func (o *Observer) publish(events []Event) {
	if len(events) == 0 {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.stopped {
		return
	}
	for _, ev := range events {
		o.stats.Delivered++
		for s := range o.subs {
			select {
			case s.ch <- ev:
			default:
				o.stats.Dropped[s.name]++
			}
		}
	}
}

// count applies a counter update under the lock.
func (o *Observer) count(f func(*Stats)) {
	o.mu.Lock()
	f(&o.stats)
	o.mu.Unlock()
}

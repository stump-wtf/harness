package supervisor

// Governing: SPEC-0003 (harness-lifecycle) REQ "Lifecycle Events"; SPEC-0002
// (daemon-protocol) REQ "Event Subscription"; ADR-0005 (the daemon supervises
// harnesses in-process and is the one thing that sees lifecycle activity).
//
// This is the subscribable lifecycle event stream the daemon will later expose
// over the control-plane socket (SPEC-0002 EVENT frames). Emission lives here,
// in the supervision core, so state changes, exits, and flapping are published
// from the single goroutine that owns each harness — no polling required.

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

// EventKind names a lifecycle event. The three kinds map 1:1 to the payloads
// SPEC-0003 REQ "Lifecycle Events" requires the daemon to emit.
type EventKind string

const (
	// EventStateChanged is emitted on every state transition
	// (`harness_state_changed { name, from, to }`).
	EventStateChanged EventKind = "harness_state_changed"
	// EventExited is emitted when a supervised process exits
	// (`harness_exited { name, code }`).
	EventExited EventKind = "harness_exited"
	// EventFlapping is emitted when crash-loop backoff escalates
	// (`harness_flapping { name, restarts, next_retry_in }`).
	EventFlapping EventKind = "harness_flapping"

	// EventRunStarted is emitted when a scheduled harness's run starts a
	// process (SPEC-0008 REQ "Lifecycle Events").
	EventRunStarted EventKind = "job_run_started"
	// EventRunFinished is emitted when a run record becomes final — including
	// a decision that started no process (skipped, missed).
	EventRunFinished EventKind = "job_run_finished"
	// EventScheduleChanged is emitted when a scheduled harness's next window
	// changes. The Manager publishes it for the scheduler, which knows nothing
	// of the bus.
	EventScheduleChanged EventKind = "job_schedule_changed"

	// EventHoursChanged is emitted when a gated harness's in_hours flips
	// (`harness_hours_changed { name, in_hours, hours_next }`). The Manager
	// publishes it for the scheduler's gate pass, which knows nothing of the
	// bus, the same way it does for EventScheduleChanged. Governing: ADR-0019,
	// SPEC-0012 REQ "Operating Hours Visibility".
	EventHoursChanged EventKind = "harness_hours_changed"

	// EventHoldChanged is emitted whenever a harness's set of hold reasons
	// changes (`harness_hold_changed { name, hold_reasons, next }`): a reason
	// added, a reason cleared, or every reason dropped by a start or stop.
	// The supervisor publishes it from the actor loop, at the moment the new
	// set becomes visible in its snapshot. Governing: ADR-0027, SPEC-0021
	// REQ-19 "Events and the durable log", REQ-14.
	EventHoldChanged EventKind = "harness_hold_changed"

	// EventTemplateRenderFailed is emitted when a spawn's argv template
	// could not be rendered, so nothing was exec'd; RenderFailure says why.
	// It feeds harness_template_render_failures_total and is NOT relayed to
	// clients: the run record (or the failed start) is the operator-facing
	// account, and this event carries no path or value of its own.
	// Governing: SPEC-0017 REQ-11 "Rendering", REQ "Error Handling
	// Standards".
	EventTemplateRenderFailed EventKind = "harness_template_render_failed"
	// EventHealth is emitted when a harness's health probe has failed
	// health_failures times in a row and the prober has taken its action
	// (restart or notify): Action names the action taken, Streak the failure
	// count that tripped it, and Code the probe's exit code (issue #960).
	// The notify Watcher turns it into the `health` event.
	EventHealth EventKind = "harness_health"
)

// Render failure reasons, the `reason` label of
// harness_template_render_failures_total (SPEC-0017 REQ-11).
const (
	// RenderFailureUnresolved: a required template value was absent.
	RenderFailureUnresolved = "unresolved"
	// RenderFailureGrammar: a template did not parse at spawn — only
	// reachable by a definition that bypassed every load-time check.
	RenderFailureGrammar = "grammar"
)

// Event is one lifecycle notification. Only the fields relevant to Kind are
// populated; the rest are zero. It carries every field named in SPEC-0003 REQ
// "Lifecycle Events" across the three kinds.
type Event struct {
	// Kind selects which payload fields are meaningful.
	Kind EventKind
	// Name is the harness the event concerns.
	Name string
	// Time is when the event was produced.
	Time time.Time

	// From/To are set for EventStateChanged.
	From core.State
	To   core.State

	// Run is set for EventRunStarted and EventRunFinished: the record as it
	// stood when the event was produced.
	Run RunRecord
	// NextRun is set for EventScheduleChanged; zero when the harness no longer
	// has a next window.
	NextRun time.Time

	// InHours and HoursNext are set for EventHoursChanged: the gate's new
	// state, and the next flip (zero when OperatingHours covers the entire
	// week). Governing: SPEC-0012 REQ "Operating Hours Visibility".
	InHours   bool
	HoursNext time.Time

	// Holds and HoldNext are set for EventHoldChanged: the harness's new set
	// of hold reasons (empty once it is no longer held), and when that hold
	// is next expected to clear (zero when unknown or not held). Governing:
	// SPEC-0021 REQ-19.
	Holds    core.HoldSet
	HoldNext time.Time

	// Code is the process exit code, set for EventExited (-1 if signalled).
	Code int

	// Restarts (↻) and NextRetryIn are set for EventFlapping.
	Restarts    int
	NextRetryIn time.Duration

	// RenderFailure is set for EventTemplateRenderFailed: one of the
	// RenderFailure* reasons.
	RenderFailure string

	// Action ("restart" | "notify") and Streak are set for EventHealth
	// (issue #960), alongside Code: the probe's exit code, and Output: the
	// capped tail of its combined output.
	Action string
	Streak int
	Output string
}

// subBuffer is the per-subscriber queue depth. Publishing never blocks on a
// slow subscriber (ADR-0007 backpressure discipline: one slow client can never
// stall the supervisor); once the queue is full, further events for that
// subscriber are dropped until it drains.
const subBuffer = 128

// Bus is a fan-out publisher of lifecycle Events. It is safe for concurrent
// use: supervisors publish from their own goroutines while the daemon (and
// tests) subscribe from theirs.
//
// Publish never blocks, so a subscriber that falls behind loses events. Each
// subscriber's losses are counted, and SubscribeCounted hands the count back
// to a subscriber that must not undercount silently — the metrics collector,
// whose state-transition counters would otherwise read low with nothing to
// say so (SPEC-0013 REQ-6). Plain Subscribe keeps its old shape; the count is
// kept for it too and simply never read.
//
// @joestump-agent 09/21/2026 - Added per-subscriber drop counts for
// harness#356.
type Bus struct {
	mu     sync.Mutex
	nextID int
	subs   map[int]*busSub
}

// busSub is one subscriber: its queue, and the events it has lost because
// that queue was full.
type busSub struct {
	ch      chan Event
	dropped atomic.Uint64
}

// NewBus returns an empty event bus.
func NewBus() *Bus {
	return &Bus{subs: make(map[int]*busSub)}
}

// Subscribe registers a new subscriber, returning its receive channel and a
// cancel func that unregisters it and closes the channel. The channel is
// buffered (subBuffer); events that arrive while it is full are dropped for
// that subscriber only.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	ch, cancel, _ := b.SubscribeCounted()
	return ch, cancel
}

// SubscribeCounted is Subscribe plus a func reporting how many events this
// subscriber has lost to a full buffer so far. The count is monotonic and
// stays readable after cancel.
func (b *Bus) SubscribeCounted() (<-chan Event, func(), func() uint64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID
	b.nextID++
	sub := &busSub{ch: make(chan Event, subBuffer)}
	b.subs[id] = sub
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if c, ok := b.subs[id]; ok {
				delete(b.subs, id)
				close(c.ch)
			}
		})
	}
	return sub.ch, cancel, sub.dropped.Load
}

// Publish delivers ev to every current subscriber without blocking. A
// subscriber whose buffer is full misses this event (bounded, lossy fan-out by
// design — see subBuffer), and the miss is counted against it.
func (b *Bus) Publish(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, sub := range b.subs {
		select {
		case sub.ch <- ev:
		default:
			// Subscriber is not keeping up; drop for it only.
			sub.dropped.Add(1)
		}
	}
}

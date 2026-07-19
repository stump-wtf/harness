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
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
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

	// Code is the process exit code, set for EventExited (-1 if signalled).
	Code int

	// Restarts (↻) and NextRetryIn are set for EventFlapping.
	Restarts    int
	NextRetryIn time.Duration
}

// subBuffer is the per-subscriber queue depth. Publishing never blocks on a
// slow subscriber (ADR-0007 backpressure discipline: one slow client can never
// stall the supervisor); once the queue is full, further events for that
// subscriber are dropped until it drains.
const subBuffer = 128

// Bus is a fan-out publisher of lifecycle Events. It is safe for concurrent
// use: supervisors publish from their own goroutines while the daemon (and
// tests) subscribe from theirs.
type Bus struct {
	mu     sync.Mutex
	nextID int
	subs   map[int]chan Event
}

// NewBus returns an empty event bus.
func NewBus() *Bus {
	return &Bus{subs: make(map[int]chan Event)}
}

// Subscribe registers a new subscriber, returning its receive channel and a
// cancel func that unregisters it and closes the channel. The channel is
// buffered (subBuffer); events that arrive while it is full are dropped for
// that subscriber only.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	id := b.nextID
	b.nextID++
	ch := make(chan Event, subBuffer)
	b.subs[id] = ch
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if c, ok := b.subs[id]; ok {
				delete(b.subs, id)
				close(c)
			}
		})
	}
	return ch, cancel
}

// Publish delivers ev to every current subscriber without blocking. A
// subscriber whose buffer is full misses this event (bounded, lossy fan-out by
// design — see subBuffer).
func (b *Bus) Publish(ev Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, ch := range b.subs {
		select {
		case ch <- ev:
		default:
			// Subscriber is not keeping up; drop for it only.
		}
	}
}

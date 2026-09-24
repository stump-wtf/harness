// Package runusage feeds the agent event observer into the run ledger's usage
// accumulator: the glue between internal/observe, which knows what the agents
// did, and internal/ledger, which knows which run was open.
//
// It is its own package because neither side can import the other: the
// observer depends on the supervisor, and the supervisor writes the ledger.
//
// The subscription is non-blocking by construction (observe.Observer never
// waits for a subscriber), so a slow ledger shows up as drops on this
// subscription, which the accumulator turns into usage_complete: false,
// never as an observer stall (SPEC-0022 REQ-8).
//
// Governing: SPEC-0022 REQ-8, REQ-9; SPEC-0013 REQ-3 (the error classes are
// the metrics collector's own classifier, so the two can never disagree).
//
// @joestump 09/24/2026 - Added for harness#459.
package runusage

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/stump-wtf/agent-trace/tail"

	"github.com/stump-wtf/harness/internal/ledger"
	"github.com/stump-wtf/harness/internal/metrics"
	"github.com/stump-wtf/harness/internal/observe"
)

// SubscriberName is the accumulator's observer subscription, as it appears in
// the observer's drop counts.
const SubscriberName = "ledger-usage"

// DefaultBuffer is the subscription's buffer (design: "The usage
// accumulator").
const DefaultBuffer = 4096

// DefaultTick is how often drops are checked and checkpoints considered. The
// accumulator itself holds each run to one checkpoint per 30 seconds.
const DefaultTick = 5 * time.Second

// Observer is what Feed needs from the observer.
type Observer interface {
	Subscribe(name string, buf int) (<-chan observe.Event, func())
	Stats() observe.Stats
}

// Options tunes Feed. The zero value is production.
type Options struct {
	Buffer int
	Tick   time.Duration
}

// Feed runs the accumulator over obs until Stop.
type Feed struct {
	obs    Observer
	acc    *ledger.Accumulator
	cancel func()
	done   chan struct{}
	once   sync.Once
}

// Start subscribes acc to obs and runs the fold loop.
func Start(obs Observer, acc *ledger.Accumulator, opts Options) *Feed {
	if opts.Buffer <= 0 {
		opts.Buffer = DefaultBuffer
	}
	if opts.Tick <= 0 {
		opts.Tick = DefaultTick
	}
	ch, cancel := obs.Subscribe(SubscriberName, opts.Buffer)
	f := &Feed{obs: obs, acc: acc, cancel: cancel, done: make(chan struct{})}
	go f.loop(ch, opts.Tick)
	return f
}

// Stop unsubscribes and waits for the loop to end. Runs still open keep what
// was folded; their close collects it.
func (f *Feed) Stop() {
	f.once.Do(f.cancel)
	<-f.done
}

func (f *Feed) loop(ch <-chan observe.Event, every time.Duration) {
	defer close(f.done)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			f.acc.Fold(ItemOf(ev))
		case <-t.C:
			f.acc.Dropped(f.obs.Stats().Dropped[SubscriberName])
			f.acc.Tick()
		}
	}
}

// ItemOf turns one observer event into an accumulator item: a tool call is a
// model call, an error mark carries its SPEC-0013 class, and every item names
// its session and that session's trace id.
func ItemOf(ev observe.Event) ledger.Item {
	it := ledger.Item{
		Harness: ev.Harness,
		At:      ev.Time,
		Session: ledger.Session{ID: ev.Session.Key, Adapter: ev.Adapter, TraceID: TraceID(ev.Session)},
	}
	switch ev.Kind {
	case observe.KindTool:
		it.Tool = true
	case observe.KindMark:
		if ev.Mark.Type == "error" {
			class, _ := metrics.Classify(ev.Adapter, ev.Mark.Note)
			it.ErrorClass = string(class)
		}
	}
	return it
}

// TraceID is the trace id agent-trace's otel.BuildTrace assigns a session
// (REQ-9): the first 16 bytes of sha256("trace:" + session key), in hex.
// agent-trace keeps the derivation unexported; a test pins this copy to
// BuildTrace's output so the two cannot drift apart silently.
func TraceID(s tail.SessionMeta) string {
	if s.Key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("trace:" + s.Key))
	return hex.EncodeToString(sum[:16])
}

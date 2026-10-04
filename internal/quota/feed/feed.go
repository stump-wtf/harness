// Package feed feeds the agent event observer into the park detector: the
// glue between internal/observe, which knows what the agents wrote, and the
// supervisor Manager, which owns one quota.Detector per harness and applies
// the parks they call for.
//
// It is its own package because neither side can import the other: the
// observer depends on the supervisor, and the supervisor owns the detectors
// (internal/runusage is the same glue for the run ledger).
//
// # Feeding The Park Detector
//
// The subscription (SubscriberName, DefaultBuffer) is non-blocking by
// construction: the observer never waits for a subscriber, and a full buffer
// is counted in its Stats.Dropped["budget"], which doctor surfaces, so a lossy
// detector is visible rather than silently lenient (design.md § "The park
// detector"). A tool call is a successful model call; an error mark is a
// model error, classified by the sink; every other mark is no model outcome.
//
// Sync closes the race between a run's exit and the observer's poll. A
// one-shot that runs out of credits exits about two seconds after it starts,
// having written its error to its transcript; the next poll may be seconds
// away; and the park is decided at the exit, before the restart policy or the
// run's outcome is settled (SPEC-0021 REQ-13). So the exit path calls Sync:
// the observer scans on the caller's goroutine, publishing what the agent
// wrote up to now, and the feed's own loop then drains everything buffered
// before Sync returns. One goroutine reads the subscription and answers the
// flush, so no event can be in hand, unfed, when the flush is answered.
//
// Governing: ADR-0027; SPEC-0021 REQ-11 "Quota detection", REQ-13 "Park
// effects", REQ-21 "Error handling and concurrency safety"; design.md § "The
// park detector".
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#477.
package feed

import (
	"context"
	"sync"
	"time"

	"github.com/stump-wtf/harness/internal/observe"
)

// SubscriberName is the detector's observer subscription, as it appears in
// the observer's drop counts (design.md: Subscribe("budget", 4096)).
const SubscriberName = "budget"

// DefaultBuffer is the subscription's buffer.
const DefaultBuffer = 4096

// Observer is what the feed needs from the observer.
type Observer interface {
	Subscribe(name string, buf int) (<-chan observe.Event, func())
	Sync(ctx context.Context)
}

// Sink receives each model outcome the observer attributes to a harness: a
// successful call (success true, note empty) or an error mark's note, at the
// item's own time. The Manager implements it. It is called on the feed's
// goroutine and must not block on a supervisor: a supervisor's exit path
// waits on this goroutine in Sync.
type Sink interface {
	QuotaObserve(harness, adapter string, at time.Time, success bool, note string)
}

// Options tunes Start. The zero value is production.
type Options struct {
	Buffer int
}

// Feed runs the subscription until Stop.
type Feed struct {
	obs    Observer
	sink   Sink
	cancel func()
	flush  chan chan struct{}
	done   chan struct{}
	once   sync.Once
}

// Start subscribes sink to obs and runs the feed's loop.
func Start(obs Observer, sink Sink, opts Options) *Feed {
	if opts.Buffer <= 0 {
		opts.Buffer = DefaultBuffer
	}
	ch, cancel := obs.Subscribe(SubscriberName, opts.Buffer)
	f := &Feed{obs: obs, sink: sink, cancel: cancel, flush: make(chan chan struct{}), done: make(chan struct{})}
	go f.loop(ch)
	return f
}

// Stop unsubscribes and waits for the loop to end. A Sync after Stop returns
// at once.
func (f *Feed) Stop() {
	f.once.Do(f.cancel)
	<-f.done
}

// Sync makes everything the agents wrote up to now reach the sink before it
// returns: the observer scans now, and the loop then folds every event that
// scan (or an earlier one) left buffered. ctx bounds the wait; its error is
// returned when it ran out first, and the sink then has whatever arrived.
func (f *Feed) Sync(ctx context.Context) error {
	f.obs.Sync(ctx)
	ack := make(chan struct{})
	select {
	case f.flush <- ack:
	case <-f.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-ack:
		return nil
	case <-f.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *Feed) loop(ch <-chan observe.Event) {
	defer close(f.done)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return
			}
			f.fold(ev)
		case ack := <-f.flush:
			// Everything published before the flush was asked for is
			// buffered here or already folded: fold the rest, then answer.
			open := f.drain(ch)
			close(ack)
			if !open {
				return
			}
		}
	}
}

// drain folds every event buffered on ch without waiting for more. It
// reports false when ch has been closed.
func (f *Feed) drain(ch <-chan observe.Event) bool {
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return false
			}
			f.fold(ev)
		default:
			return true
		}
	}
}

// fold hands one event to the sink if it is a model outcome.
func (f *Feed) fold(ev observe.Event) {
	switch {
	case ev.Kind == observe.KindTool:
		f.sink.QuotaObserve(ev.Harness, ev.Adapter, ev.Time, true, "")
	case ev.Kind == observe.KindMark && ev.Mark.Type == "error":
		f.sink.QuotaObserve(ev.Harness, ev.Adapter, ev.Time, false, ev.Mark.Note)
	}
}

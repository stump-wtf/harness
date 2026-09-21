package supervisor

// Governing tests: SPEC-0003 REQ "Lifecycle Events"; SPEC-0002 REQ "Event
// Subscription" (subscribe-and-push, backpressure never blocks the producer).

import (
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
)

func TestBusFanOutToAllSubscribers(t *testing.T) {
	b := NewBus()
	ch1, cancel1 := b.Subscribe()
	ch2, cancel2 := b.Subscribe()
	defer cancel1()
	defer cancel2()

	ev := Event{Kind: EventStateChanged, Name: "a", From: core.StateStopped, To: core.StateStarting}
	b.Publish(ev)

	for i, ch := range []<-chan Event{ch1, ch2} {
		select {
		case got := <-ch:
			if got.Kind != EventStateChanged || got.To != core.StateStarting {
				t.Errorf("subscriber %d got %+v", i, got)
			}
		case <-time.After(time.Second):
			t.Fatalf("subscriber %d received nothing", i)
		}
	}
}

func TestBusCancelStopsDelivery(t *testing.T) {
	b := NewBus()
	ch, cancel := b.Subscribe()
	cancel()
	// Channel is closed after cancel.
	if _, ok := <-ch; ok {
		t.Fatal("expected closed channel after cancel")
	}
	// Publishing after cancel must not panic.
	b.Publish(Event{Kind: EventExited, Name: "a"})
}

func TestBusBackpressureNeverBlocks(t *testing.T) {
	b := NewBus()
	_, cancel := b.Subscribe() // never drained
	defer cancel()

	done := make(chan struct{})
	go func() {
		for i := 0; i < subBuffer*4; i++ {
			b.Publish(Event{Kind: EventExited, Name: "a", Code: i})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Publish blocked on a slow subscriber (no backpressure drop)")
	}
}

// A stalled subscriber's losses are counted against it alone: a subscriber
// that keeps up reports zero, and the stalled one reports exactly the events
// published past its buffer. The count survives cancel, so a consumer that
// reads it on the way out still sees the whole loss. (harness#356: the
// metrics collector reports this count so its transition counters cannot
// undercount silently, SPEC-0013 REQ-6.)
func TestBusCountsDropsPerSubscriber(t *testing.T) {
	b := NewBus()
	_, cancelStalled, stalledDrops := b.SubscribeCounted()
	live, cancelLive, liveDrops := b.SubscribeCounted()
	defer cancelLive()

	const extra = 7
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range live {
		}
	}()
	for i := 0; i < subBuffer+extra; i++ {
		b.Publish(Event{Kind: EventStateChanged, Name: "a"})
		// Let the live reader keep pace, so only the stalled one overflows.
		for len(live) > 0 {
			time.Sleep(time.Microsecond)
		}
	}
	if got := stalledDrops(); got != extra {
		t.Errorf("stalled subscriber dropped %d, want %d", got, extra)
	}
	if got := liveDrops(); got != 0 {
		t.Errorf("a subscriber that kept up dropped %d, want 0", got)
	}
	cancelStalled()
	b.Publish(Event{Kind: EventStateChanged, Name: "a"})
	if got := stalledDrops(); got != extra {
		t.Errorf("after cancel the count moved or reset: %d, want %d", got, extra)
	}
	cancelLive()
	<-done
}

package supervisor

// Governing tests: SPEC-0003 REQ "Lifecycle Events"; SPEC-0002 REQ "Event
// Subscription" (subscribe-and-push, backpressure never blocks the producer).

import (
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
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

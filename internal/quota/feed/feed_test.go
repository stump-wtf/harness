package feed

// Feed Tests
//
// Sync's promise is the one the quota park at a run's exit depends on: every
// event the observer published before Sync returned has reached the sink. A
// fake observer publishes from its Sync exactly as the real one does (onto the
// subscription's buffer, never blocking), so the test is of the feed's half:
// one goroutine reads the subscription and answers the flush, so nothing can
// be in hand, unfed, when the answer comes.
//
// Governing: SPEC-0021 REQ-13, REQ-21; stump.wtf/harness#477.
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#477.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/agent-trace/classify"

	"github.com/stump-wtf/harness/internal/observe"
)

// fakeObserver publishes burst events onto the subscription from each Sync,
// as the real observer's scan does, and trickles events in the background
// the way its poll does.
type fakeObserver struct {
	mu    sync.Mutex
	ch    chan observe.Event
	burst int
}

func (o *fakeObserver) Subscribe(_ string, buf int) (<-chan observe.Event, func()) {
	o.ch = make(chan observe.Event, buf)
	var once sync.Once
	return o.ch, func() {
		once.Do(func() {
			o.mu.Lock()
			defer o.mu.Unlock()
			close(o.ch)
			o.ch = nil
		})
	}
}

func (o *fakeObserver) publish(ev observe.Event) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.ch == nil {
		return
	}
	select {
	case o.ch <- ev:
	default:
	}
}

func (o *fakeObserver) Sync(context.Context) {
	for range o.burst - 1 {
		o.publish(observe.Event{Harness: "review", Adapter: "crush", Kind: observe.KindTool, Time: time.Now()})
	}
	o.publish(observe.Event{Harness: "review", Adapter: "crush", Kind: observe.KindMark,
		Mark: classify.Mark{Type: "error", Note: "Payment Required"}, Time: time.Now()})
}

type countingSink struct {
	mu        sync.Mutex
	successes int
	errors    []string
}

func (s *countingSink) QuotaObserve(_, _ string, _ time.Time, success bool, note string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if success {
		s.successes++
		return
	}
	s.errors = append(s.errors, note)
}

func (s *countingSink) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.successes, len(s.errors)
}

// Every event a Sync published has reached the sink when Sync returns, with
// background events arriving all the while. Run with -race -count=N.
func TestSyncReturnsAfterTheSinkHasEverything(t *testing.T) {
	obs := &fakeObserver{burst: 50}
	sink := &countingSink{}
	f := Start(obs, sink, Options{Buffer: 4096})
	defer f.Stop()

	stop := make(chan struct{})
	var bg sync.WaitGroup
	bg.Add(1)
	go func() { // the poll, publishing alongside the syncs
		defer bg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				obs.publish(observe.Event{Harness: "other", Kind: observe.KindMark, Mark: classify.Mark{Type: "user-message"}})
				time.Sleep(50 * time.Microsecond)
			}
		}
	}()
	for i := 1; i <= 20; i++ {
		if err := f.Sync(context.Background()); err != nil {
			t.Fatal(err)
		}
		if succ, errs := sink.counts(); succ < i*(obs.burst-1) || errs < i {
			t.Fatalf("after sync %d the sink has %d successes and %d errors, want at least %d and %d", i, succ, errs, i*(obs.burst-1), i)
		}
	}
	close(stop)
	bg.Wait()
}

// Only tool calls and error marks are model outcomes.
func TestFoldPassesOnlyModelOutcomes(t *testing.T) {
	sink := &countingSink{}
	f := &Feed{sink: sink}
	f.fold(observe.Event{Kind: observe.KindMark, Mark: classify.Mark{Type: "user-message", Note: "hi"}})
	f.fold(observe.Event{Kind: observe.KindMark, Mark: classify.Mark{Type: "turn-end"}})
	f.fold(observe.Event{Kind: observe.KindMark, Mark: classify.Mark{Type: "error", Note: "429"}})
	f.fold(observe.Event{Kind: observe.KindTool})
	if succ, errs := sink.counts(); succ != 1 || errs != 1 || sink.errors[0] != "429" {
		t.Fatalf("sink got %d successes and errors %v, want 1 and [429]", succ, sink.errors)
	}
}

// A Sync after Stop, or with a context that has run out, returns at once.
func TestSyncAfterStopReturns(t *testing.T) {
	f := Start(&fakeObserver{burst: 1}, &countingSink{}, Options{})
	f.Stop()
	done := make(chan error, 1)
	go func() { done <- f.Sync(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Sync after Stop: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Sync after Stop did not return")
	}
}

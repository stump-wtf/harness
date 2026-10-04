package observe

// Sync Tests
//
// Sync is the observer's answer to a decision that must not race its poll:
// the quota park at a run's exit (SPEC-0021 REQ-13) asks for everything the
// agent wrote up to now, and it must be in the subscriber's buffer when Sync
// returns, however far away the next poll is.
//
// Governing: SPEC-0021 REQ-11, REQ-13; stump.wtf/harness#477.
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#477.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/core"
	rt "github.com/stump-wtf/harness/internal/runtrace/runtracetest"
)

// An error mark written after the loop's first scan is in the subscriber's
// buffer when Sync returns, with the poll an hour away.
func TestSyncDeliversWhatWasWrittenBeforeIt(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.PollInterval = time.Hour })
	f.src.add(core.Harness{Name: "review", Adapter: "crush", Workdir: f.work}, running(start))
	ch, cancel := f.obs.Subscribe("budget", 16)
	defer cancel()
	f.obs.Start()
	waitScanned(t, f.obs)

	f.clock.Set(start.Add(2 * time.Second))
	rt.WriteCrushDB(t, f.crushDB(), rt.CrushSession{
		ID: "run-1", Created: start, Updated: start.Add(2 * time.Second),
		Messages: []rt.CrushMessage{
			userSays("review the open PRs", start.Add(time.Second)),
			providerError("Payment Required", start.Add(2*time.Second)),
		},
	})
	if got := drain(ch); len(got) != 0 {
		t.Fatalf("delivered before Sync: %v", describe(got))
	}
	f.obs.Sync(context.Background())
	if got, want := describe(drain(ch)), []string{"review:mark:user-message@0", "review:mark:error@0"}; !equal(got, want) {
		t.Fatalf("after Sync: %v, want %v", got, want)
	}
}

// Sync and the loop's scans never overlap (the scan state has one owner at a
// time), and Sync after Stop returns at once. Run under -race.
func TestSyncSerializesWithTheLoopAndStop(t *testing.T) {
	f := newFixture(t, func(o *Options) { o.PollInterval = time.Millisecond })
	f.src.add(core.Harness{Name: "review", Adapter: "crush", Workdir: f.work}, running(start))
	rt.WriteCrushDB(t, f.crushDB(), rt.CrushSession{ID: "run-1", Created: start, Updated: start,
		Messages: []rt.CrushMessage{providerError("429", start)}})
	f.obs.Start()
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 20 {
				f.obs.Sync(context.Background())
			}
		}()
	}
	wg.Wait()
	f.obs.Stop()
	done := make(chan struct{})
	go func() { f.obs.Sync(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Sync after Stop did not return")
	}
}

// waitScanned waits for the loop's first scan, so a test's writes land after
// it and only Sync can deliver them.
func waitScanned(t *testing.T, o *Observer) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for o.Stats().LastScan.IsZero() {
		if time.Now().After(deadline) {
			t.Fatal("the observer never scanned")
		}
		time.Sleep(time.Millisecond)
	}
	// LastScan is stamped as a scan begins; taking the scan lock (and
	// nothing else) waits for that scan to end.
	o.scanMu.Lock()
	o.scanMu.Unlock()
}

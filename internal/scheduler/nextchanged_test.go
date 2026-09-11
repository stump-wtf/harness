package scheduler

// Governing: SPEC-0008 REQ "Lifecycle Events" (job_schedule_changed); issue
// #120.

import (
	"sync"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// TestNextChangedReportsEveryMove: arming, advancing past a firing, re-arming
// on a changed spec and disarming each report the new next window; a no-change
// reload reports nothing.
func TestNextChangedReportsEveryMove(t *testing.T) {
	var (
		mu  sync.Mutex
		got []nextChange
	)
	clock := newFakeClock(utc(2026, 9, 11, 1, 30, 0))
	s := New(Options{
		Clock:    clock,
		Location: time.UTC,
		Store:    newMemStore(),
		NextChanged: func(name string, next time.Time) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, nextChange{name: name, next: next})
		},
	})
	t.Cleanup(s.Close)
	changes := func() []nextChange {
		mu.Lock()
		defer mu.Unlock()
		return append([]nextChange(nil), got...)
	}
	expect := func(step string, want ...nextChange) {
		t.Helper()
		have := changes()
		if len(have) != len(want) {
			t.Fatalf("%s: changes = %+v, want %+v", step, have, want)
		}
		for i := range want {
			if have[i].name != want[i].name || !have[i].next.Equal(want[i].next) {
				t.Fatalf("%s: change %d = %+v, want %+v", step, i, have[i], want[i])
			}
		}
	}

	hourly := cfgOf(job{"sweep", "0 * * * *", false})
	s.Apply(hourly)
	armed := nextChange{name: "sweep", next: utc(2026, 9, 11, 2, 0, 0)}
	expect("arm", armed)

	s.Apply(hourly)
	expect("no-change reload", armed)

	clock.set(utc(2026, 9, 11, 2, 0, 0))
	s.evaluate()
	s.firings.Wait()
	advanced := nextChange{name: "sweep", next: utc(2026, 9, 11, 3, 0, 0)}
	expect("advance past a firing", armed, advanced)

	s.Apply(cfgOf(job{"sweep", "30 * * * *", false}))
	rearmed := nextChange{name: "sweep", next: utc(2026, 9, 11, 2, 30, 0)}
	expect("changed spec re-arms, not disarms", armed, advanced, rearmed)

	s.Apply(&core.Config{})
	expect("disarm", armed, advanced, rearmed, nextChange{name: "sweep"})
}

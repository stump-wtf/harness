package budget

// Governing: SPEC-0021 REQ-2 (day_starts and its zone), REQ-3 "The budget
// day": Scenario "Fall-back day", the 23-hour spring-forward day, Scenario
// "A budget day in another zone", and the arithmetic under Scenario "Sleeping
// through midnight" (the counters' reset is the Manager's, manager_admit.go).
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#470.

import (
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/hours"
)

func mustInstant(t *testing.T, s string) hours.DailyInstant {
	t.Helper()
	d, err := hours.ParseDailyInstant(s)
	if err != nil {
		t.Fatalf("ParseDailyInstant(%q): %v", s, err)
	}
	return d
}

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestDayBoundsAnInstant(t *testing.T) {
	spec := mustInstant(t, "TZ=UTC 06:00")
	for _, c := range []struct {
		now         time.Time
		start, next time.Time
	}{
		// After today's start: today 06:00 to tomorrow 06:00.
		{time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC), time.Date(2026, 10, 4, 6, 0, 0, 0, time.UTC), time.Date(2026, 10, 5, 6, 0, 0, 0, time.UTC)},
		// Before it: yesterday's day, across a month boundary.
		{time.Date(2026, 10, 1, 5, 59, 0, 0, time.UTC), time.Date(2026, 9, 30, 6, 0, 0, 0, time.UTC), time.Date(2026, 10, 1, 6, 0, 0, 0, time.UTC)},
		// Exactly at the start: the new day (start <= now < next).
		{time.Date(2026, 10, 4, 6, 0, 0, 0, time.UTC), time.Date(2026, 10, 4, 6, 0, 0, 0, time.UTC), time.Date(2026, 10, 5, 6, 0, 0, 0, time.UTC)},
	} {
		start, next := Day(c.now, spec)
		if !start.Equal(c.start) || !next.Equal(c.next) {
			t.Errorf("Day(%v) = [%v, %v), want [%v, %v)", c.now, start, next, c.start, c.next)
		}
	}
}

// TestDayInAnotherZone is REQ-2 "A budget day in another zone": with
// day_starts = "TZ=America/Los_Angeles 06:00", a new day begins at 13:00 UTC
// in summer, whatever zone the daemon runs in.
func TestDayInAnotherZone(t *testing.T) {
	spec := mustInstant(t, "TZ=America/Los_Angeles 06:00")
	before := time.Date(2026, 7, 15, 12, 59, 0, 0, time.UTC)
	at := time.Date(2026, 7, 15, 13, 0, 0, 0, time.UTC)
	s1, n1 := Day(before, spec)
	s2, _ := Day(at, spec)
	if !n1.Equal(at) || !s2.Equal(at) || s1.Equal(s2) {
		t.Errorf("12:59 UTC is in [%v, %v), 13:00 UTC starts %v; want a new day at 13:00 UTC", s1, n1, s2)
	}
}

// TestDayFallBack is REQ-3 "Fall-back day": day_starts = "00:00" in a zone
// that falls back at 02:00 makes that day 25 hours long, and every instant in
// it, the repeated hour included, lands in the one day.
func TestDayFallBack(t *testing.T) {
	la := mustZone(t, "America/Los_Angeles")
	spec := mustInstant(t, "TZ=America/Los_Angeles 00:00")
	// 2026-11-01: PDT falls back to PST at 02:00.
	start, next := Day(time.Date(2026, 11, 1, 12, 0, 0, 0, la), spec)
	if got := next.Sub(start); got != 25*time.Hour {
		t.Fatalf("fall-back day lasts %v, want 25h ([%v, %v))", got, start, next)
	}
	for h := time.Duration(0); h < 25*time.Hour; h += 30 * time.Minute {
		s, _ := Day(start.Add(h), spec)
		if !s.Equal(start) {
			t.Fatalf("%v into the fall-back day lands in the day starting %v, want %v", h, s, start)
		}
	}
	// And the spring-forward day is 23 hours.
	start, next = Day(time.Date(2026, 3, 8, 12, 0, 0, 0, la), spec)
	if got := next.Sub(start); got != 23*time.Hour {
		t.Fatalf("spring-forward day lasts %v, want 23h", got)
	}
}

// TestDayStartInsideTheSpringGap: a day_starts that does not exist on the
// spring-forward date (02:30 in Los Angeles on 2026-03-08) still bounds every
// instant, and the day around the gap is never empty.
func TestDayStartInsideTheSpringGap(t *testing.T) {
	la := mustZone(t, "America/Los_Angeles")
	spec := mustInstant(t, "TZ=America/Los_Angeles 02:30")
	for _, now := range []time.Time{
		time.Date(2026, 3, 8, 1, 59, 0, 0, la),
		time.Date(2026, 3, 8, 3, 10, 0, 0, la),
		time.Date(2026, 3, 8, 4, 0, 0, 0, la),
		time.Date(2026, 3, 9, 2, 29, 0, 0, la),
	} {
		start, next := Day(now, spec)
		if now.Before(start) || !now.Before(next) || next.Sub(start) < 22*time.Hour || next.Sub(start) > 26*time.Hour {
			t.Errorf("Day(%v) = [%v, %v): not a day around now", now, start, next)
		}
	}
}

// TestDaySleepingThroughMidnight is the arithmetic half of REQ-3 "Sleeping
// through midnight": an instant past several rollovers lands in its own day,
// not the one after the last day the caller saw.
func TestDaySleepingThroughMidnight(t *testing.T) {
	spec := mustInstant(t, "TZ=UTC 00:00")
	_, next := Day(time.Date(2026, 10, 4, 23, 30, 0, 0, time.UTC), spec)
	woke := time.Date(2026, 10, 7, 8, 0, 0, 0, time.UTC)
	start, _ := Day(woke, spec)
	if want := time.Date(2026, 10, 7, 0, 0, 0, 0, time.UTC); !start.Equal(want) || start.Equal(next) {
		t.Errorf("woke at %v into the day starting %v, want %v", woke, start, want)
	}
}

// TestDayZeroSpecIsLocalMidnight: an absent day_starts is 00:00 in the
// daemon's zone.
func TestDayZeroSpecIsLocalMidnight(t *testing.T) {
	now := time.Date(2026, 10, 4, 15, 0, 0, 0, time.Local)
	start, next := Day(now, hours.DailyInstant{})
	if want := time.Date(2026, 10, 4, 0, 0, 0, 0, time.Local); !start.Equal(want) {
		t.Errorf("start = %v, want local midnight %v", start, want)
	}
	if want := time.Date(2026, 10, 5, 0, 0, 0, 0, time.Local); !next.Equal(want) {
		t.Errorf("next = %v, want %v", next, want)
	}
}

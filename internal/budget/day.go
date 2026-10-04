package budget

// The Budget Day
//
// A budget day runs from one day_starts instant, in its zone, to the next
// (SPEC-0021 REQ-3). Day is that arithmetic for a caller-supplied instant:
// the day it falls in, and when that day ends. It walks calendar dates rather
// than adding 24 hours, so a day that a DST change makes 23 or 25 hours long
// is as long as the wall clock makes it ("Fall-back day"), and an instant
// many days past the last rollover simply lands in its own day ("Sleeping
// through midnight": no spend carries over, because the caller starts its
// counters afresh for the day Day returns).
//
// Governing: ADR-0027; SPEC-0021 REQ-2 "The [budget] table" (day_starts),
// REQ-3 "The budget day"; design.md § "The budget day reuses the gate's
// clock" (the caller's clock is the scheduler tick's).
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#470.

import (
	"time"

	"github.com/stump-wtf/harness/internal/hours"
)

// Day returns the budget day now falls in under spec: start is the latest
// day_starts instant at or before now, and next the first one after it, so
// start <= now < next always holds. The zero spec is 00:00 in time.Local, the
// default for an absent day_starts.
func Day(now time.Time, spec hours.DailyInstant) (start, next time.Time) {
	loc := spec.Location()
	hh, mm := spec.Clock()
	t := now.In(loc)
	y, mo, d := t.Date()
	start = at(y, mo, d, hh, mm, loc)
	// A day_starts inside a spring-forward gap normalizes forward, so the
	// instant for today can land after now even when now's wall clock is
	// past day_starts; and before day_starts it is simply tomorrow's start.
	// Either way the day began on an earlier date: step back until it did.
	for start.After(now) {
		d--
		start = at(y, mo, d, hh, mm, loc)
	}
	next = at(y, mo, d+1, hh, mm, loc)
	// The mirror case: a date whose start normalized backward past now's.
	for !next.After(now) {
		d++
		start, next = next, at(y, mo, d+1, hh, mm, loc)
	}
	return start, next
}

// at is hh:mm on the date y-mo-d in loc. time.Date normalizes an out-of-range
// day, so d-1 on the 1st is the last day of the previous month.
func at(y int, mo time.Month, d, hh, mm int, loc *time.Location) time.Time {
	return time.Date(y, mo, d, hh, mm, 0, 0, loc)
}

package scheduler

// Window Resolution
//
// nextAfter answers "when is this entry's first window strictly after t?" and
// is the only place time zones and DST enter the scheduler. robfig/cron parses
// the expression — including a CRON_TZ=/TZ= prefix, which pins the zone the
// expression is read in regardless of the daemon's own — but its Next walks
// real instants hour by hour, which gets DST wrong for a job pinned to a time
// of day: "30 2 * * *" is silently skipped on spring-forward day (02:30 never
// exists) and "30 1 * * *" runs twice on fall-back day (01:30 happens twice).
//
// So schedules split the way ISC/Vixie cron splits them:
//
//   - An expression that fires in EVERY hour ("*/15 * * * *", "@hourly") or an
//     "@every" interval is a cadence in real time. robfig's Next is exactly
//     right for it, and a DST change neither adds nor drops a run.
//   - An expression restricted to particular hours ("30 2 * * *", "@daily",
//     "0 9 * * 1") names wall-clock times. Each wall-clock window runs once: a
//     window inside a spring-forward gap runs at the instant the pre-jump
//     offset names (02:30 EST, which reads 03:30 EDT — exactly 24 real hours
//     after yesterday's run), and a window repeated by a fall-back runs only at
//     its first occurrence.
//
// Governing: ADR-0013, SPEC-0008 REQ "Schedule Time Zone".
//
// @joestump-agent 09/11/2026 - Added for issue #117.

import (
	"time"

	"github.com/robfig/cron/v3"
)

// allHours is the hour-field bitmask of an expression that fires every hour.
const allHours = 1<<24 - 1

// maxLabelSkips bounds the fall-back loop in wallNext. A label is skipped only
// when its first occurrence is already behind t, which can happen for at most
// the labels inside one repeated hour (60 minutes, or 120 for a two-hour
// shift) — so the bound is never reached by a real zone.
const maxLabelSkips = 256

// nextAfter returns e's first window strictly after t, or zero if the
// expression never fires again. The result carries no monotonic reading.
func (s *Scheduler) nextAfter(e *entry, t time.Time) time.Time {
	var next time.Time
	switch sch := e.sched.(type) {
	case *cron.SpecSchedule:
		zone := sch.Location
		if zone == time.Local {
			// No CRON_TZ=/TZ= prefix: the daemon's zone.
			zone = s.loc
		}
		if sch.Hour&allHours == allHours {
			// robfig evaluates a Local-located spec in t's zone.
			next = sch.Next(t.In(zone))
		} else {
			next = wallNext(sch, zone, t)
		}
	default:
		next = e.sched.Next(t)
	}
	return next.Round(0)
}

// wallNext is nextAfter for an hour-restricted expression: it walks wall-clock
// labels in zone rather than instants, then resolves each label to an instant.
func wallNext(spec *cron.SpecSchedule, zone *time.Location, t time.Time) time.Time {
	// Evaluating in UTC makes robfig see a clock with no DST, so every label
	// the expression names exists exactly once.
	naive := *spec
	naive.Location = time.UTC

	lt := t.In(zone)
	label := time.Date(lt.Year(), lt.Month(), lt.Day(), lt.Hour(), lt.Minute(), lt.Second(), 0, time.UTC)
	for range maxLabelSkips {
		label = naive.Next(label)
		if label.IsZero() {
			return time.Time{}
		}
		if at := labelInstant(label, zone); at.After(t) {
			return at
		}
		// This label's first occurrence is already behind t (t sits in a
		// fall-back's repeated hour): it has had its run.
	}
	return spec.Next(t.In(zone))
}

// labelInstant resolves a wall-clock label (carried in a UTC time.Time) to an
// instant in zone: its first occurrence if the label is repeated, the instant
// the pre-transition offset names if the label falls in a gap.
//
// Deliberately not time.Date(…, zone): Go documents its choice across a
// transition as unspecified, and in practice it resolves a spring-forward gap
// BACKWARDS (02:30 America/New_York becomes 01:30 EST), before the jump.
func labelInstant(label time.Time, zone *time.Location) time.Time {
	y, mo, d := label.Date()
	h, mi, sec := label.Clock()
	probe := time.Date(y, mo, d, h, mi, sec, 0, zone)

	// Across a DST transition the two candidate offsets are the ones in force
	// a day either side of it. Real zones never transition twice in 48 hours.
	_, before := probe.Add(-24 * time.Hour).Zone()
	_, after := probe.Add(24 * time.Hour).Zone()

	var first time.Time
	for _, off := range []int{before, after} {
		at := time.Date(y, mo, d, h, mi, sec, 0, time.FixedZone("", off)).In(zone)
		if !sameLabel(at, label) {
			continue
		}
		if first.IsZero() || at.Before(first) {
			first = at
		}
	}
	if !first.IsZero() {
		return first
	}
	// In a gap: neither offset reads back as the label.
	return time.Date(y, mo, d, h, mi, sec, 0, time.FixedZone("", before)).In(zone)
}

// sameLabel reports whether at, read in its own zone, shows label's wall clock.
func sameLabel(at, label time.Time) bool {
	ay, amo, ad := at.Date()
	ah, ami, asec := at.Clock()
	ly, lmo, ld := label.Date()
	lh, lmi, lsec := label.Clock()
	return ay == ly && amo == lmo && ad == ld && ah == lh && ami == lmi && asec == lsec
}

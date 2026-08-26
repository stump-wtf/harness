// Package schedfmt renders a harness's schedule for humans: the cron
// expression as a short cadence label ("daily 09:30", "Mondays 07:00") and the
// protocol's RFC 3339 next-run stamp as a countdown ("in 2h", "due").
//
// It exists because two surfaces answer the same two questions — `harness
// list`/`describe` on the CLI and the cockpit's dashboard rows — and a cron
// parser duplicated per surface drifts. Both read the same wire fields
// (protocol.HarnessInfo.Schedule and .NextRun), so both should phrase them
// identically; an operator who learns "in 2h" in the table should not meet
// "2h0m0s" in the TUI.
//
// Everything here is presentation, not scheduling: the daemon owns the cron
// and resolves the firing time (ADR-0013). Label is deliberately conservative
// — an expression it cannot state plainly returns "" so the caller falls back
// to showing the raw cron rather than a confident paraphrase that is wrong.
//
// Governing: ADR-0013 (scheduled one-shot jobs; the daemon owns next-fire),
// SPEC-0003 (state legibility across every listing surface).
//
// @joestump-agent 08/20/2026 - Extracted from cmd/harness so the TUI can
// surface the schedule and next-run time too (issue #160).
//
// @joestump-agent 08/22/2026 - Label now names an interval by asking the
// schedule when it fires, via robfig/cron — already this repo's parser and
// scheduler — instead of reading the expression's notation. "0 */6 * * *"
// returned "" and so rendered unhighlighted next to its neighbours. A first
// pass fixed that by hand-parsing "*/n" with a divisibility rule; @joestump
// asked why this was hand-rolled at all, which was the right question. Real
// firing times settle every case the rule had to enumerate — and one it got
// wrong: "0,30 * * * *" IS a constant 30m cadence, which the rule declined.
package schedfmt

import (
	"fmt"
	"strings"
	"time"

	"github.com/robfig/cron/v3"

	"gitea.stump.rocks/stump.wtf/harness/internal/core"
)

// NextIn renders an RFC 3339 next-run stamp as a countdown an operator reads
// at a glance: "in 3h", "in 12m", "due" once the firing time has passed.
//
// Empty and unparseable input both return "" — a harness with no schedule and
// a daemon that has not resolved a firing time yet are equally "nothing to
// say here", and callers decide what a blank looks like on their surface.
func NextIn(nextRun string) string {
	if nextRun == "" {
		return ""
	}
	next, err := time.Parse(time.RFC3339, nextRun)
	if err != nil {
		return ""
	}
	return NextInAt(next, time.Now())
}

// NextInAt is NextIn against an explicit reference time, so callers with a
// parsed timestamp (and tests with a fixed clock) skip the round-trip.
func NextInAt(next, now time.Time) string {
	d := next.Sub(now)
	if d <= 0 {
		return "due"
	}
	return "in " + ShortDuration(d)
}

// ShortDuration compacts a duration the way an operator reads it: dropping
// fractional seconds and every zero unit, leading or trailing ("45s", "12m",
// "2h5m", "3d4h").
//
// Formatted by hand rather than through Duration.String(), which always
// carries the smaller units down to seconds — "12m0s", "10h45m0s". That is
// noise at a glance, and "in 10h45m0s" is 11 cells against the NEXT column's
// 10, so the table truncated it back to "in 10h45m…".
//
// Rounding is applied before the unit split so a carry lands in the right
// place: 59m45s reads "1h", not "60m".
func ShortDuration(d time.Duration) string {
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	if d < 24*time.Hour {
		d = d.Round(time.Minute)
	} else {
		d = d.Round(time.Hour)
	}
	days := int(d / (24 * time.Hour))
	hours := int(d/time.Hour) % 24
	mins := int(d/time.Minute) % 60
	switch {
	case days > 0 && hours > 0:
		return fmt.Sprintf("%dd%dh", days, hours)
	case days > 0:
		return fmt.Sprintf("%dd", days)
	case hours > 0 && mins > 0:
		return fmt.Sprintf("%dh%dm", hours, mins)
	case hours > 0:
		return fmt.Sprintf("%dh", hours)
	default:
		return fmt.Sprintf("%dm", mins)
	}
}

// Label converts a cron expression or @-descriptor into the short
// human-readable cadence operators embed in descriptions ("every 6h",
// "daily 09:30", "Mondays 07:00"). Returns "" for empty or unparseable
// input so callers can skip highlighting.
//
// Deliberately terse, and deliberately not a general cron describer. The
// DESCRIPTION cell highlights this string only where it appears VERBATIM in
// text the operator wrote, so the vocabulary has to match what a person types
// in a config — "every 6h", not "At 0 minutes past the hour, every 6 hours".
// A describer library (lnquy/cron and friends) phrases for prose and for
// width neither constraint tolerates; the interval arithmetic underneath,
// which is the part worth not hand-rolling, comes from robfig/cron below.
//
// Clock-shaped cadences are named from the fields, because rendering "09:30"
// requires reading them. Everything else falls through to interval().
func Label(schedule string) string {
	if schedule == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(schedule, "@every "):
		dur := strings.TrimPrefix(schedule, "@every ")
		return "every " + dur
	case schedule == "@daily":
		return "daily"
	case schedule == "@hourly":
		return "hourly"
	case schedule == "@weekly":
		return "weekly"
	case schedule == "@monthly":
		return "monthly"
	}
	parts := strings.Fields(schedule)
	if len(parts) != 5 {
		return ""
	}
	minute, hour, dom, month, dow := parts[0], parts[1], parts[2], parts[3], parts[4]
	timeStr := formatCronTime(hour, minute)
	switch {
	case dom != "*" && month == "*" && dow == "*":
		return fmt.Sprintf("monthly %s", timeStr)
	case dow != "*" && dom == "*" && month == "*":
		dayName := cronDayName(dow)
		if dayName != "" {
			return fmt.Sprintf("%s %s", dayName, timeStr)
		}
		return fmt.Sprintf("day %s %s", dow, timeStr)
	case hour != "*" && minute != "*" && dom == "*" && month == "*" && dow == "*":
		if !strings.Contains(hour, "/") && !strings.Contains(hour, ",") && !strings.Contains(hour, "-") {
			return fmt.Sprintf("daily %s", timeStr)
		}
	case hour == "*" && minute != "*" && dom == "*" && month == "*" && dow == "*":
		// Only a single literal minute reads as "hourly :MM". A step, list or
		// range does not, and rendering it anyway produced strings like
		// "hourly :*/7" that describe no schedule at all. Those fall through
		// to the interval check below, which either names the cadence or
		// declines.
		if isLiteralField(minute) {
			return fmt.Sprintf("hourly :%s", minute)
		}
	}

	// Nothing above could name it from its fields. Ask the schedule when it
	// actually fires: if every gap is the same, it IS an interval and reads as
	// "every 6h".
	if d, ok := interval(schedule); ok {
		return "every " + ShortDuration(d)
	}
	return ""
}

// interval reports the firing period of a schedule, and whether it has one at
// all.
//
// Derived by asking the parsed schedule for successive firing times rather than
// by reading the expression, because a cron field does not mean what its
// notation suggests: steps do not wrap. "0 */7 * * *" fires at 0,7,14,21 and
// then jumps only 3h to the next midnight, so it is not "every 7h" for a
// quarter of the day — and "0,30 */6 * * *" fires twice per six-hour block, so
// the hour step alone does not describe it. Both fall out of comparing real
// gaps, with no rule to write down or to get wrong; an earlier version of this
// function enumerated divisibility by hand and had to special-case each.
//
// robfig/cron is already this repo's parser and scheduler (internal/scheduler,
// internal/config validation), so this asks the same engine that will actually
// fire the harness — the label cannot disagree with the behaviour.
//
// Runs only for expressions the literal branches above declined, which in
// practice means step notation; daily/weekly/monthly never reach it.
func interval(schedule string) (time.Duration, bool) {
	// The sampling window below proves constancy only for a pattern that
	// repeats DAILY, and that holds exactly when the date fields are
	// unrestricted: with day-of-month, month and day-of-week all "*", the
	// schedule fires on some set of (hour, minute) within every day, so the
	// sequence has a 24h period and two days of equal gaps settle it forever.
	//
	// Restrict any date field and the guarantee is gone, because the pattern
	// now has a monthly or yearly period the window cannot see:
	//
	//	0 9 * */3 *   fires daily at 09:00 but only in Jan/Apr/Jul/Oct, so
	//	              every gap inside the window is 24h and the three
	//	              two-month silences are invisible -> "every 1d"
	//	0 0 1 */2 *   has a period longer than the window entirely, so exactly
	//	              ONE gap is measured and compared against nothing, and its
	//	              real gaps run 59d/61d/62d -> "every 61d"
	//
	// Both are wrong, and wrong here is worse than silent: LabelOrRaw shows
	// the raw expression when this declines, so declining costs a paraphrase
	// while guessing replaces a correct cron with a false one in the TUI.
	//
	// This is the same "steps do not wrap" hazard the rest of this function
	// exists for, one field over — the date fields wrap at boundaries the
	// window never reaches, rather than at midnight, which it does.
	//
	// @joestump-agent 08/23/2026 - Review fix.
	if parts := strings.Fields(schedule); len(parts) != 5 ||
		parts[2] != "*" || parts[3] != "*" || parts[4] != "*" {
		return 0, false
	}
	sched, err := cron.ParseStandard(schedule)
	if err != nil {
		return 0, false
	}
	// A fixed UTC reference keeps this deterministic and clear of DST, where a
	// legitimately-constant interval would show a 23h or 25h gap once a year.
	//
	// Start measuring from the FIRST FIRE, not from the reference: the gap
	// between an arbitrary instant and the next fire is a phase offset, not a
	// period, and comparing it against the real gaps rejected every schedule
	// whose first fire did not land on the reference ("30 */2 * * *" measured
	// 30m then 2h and declined a perfectly constant 2h cadence).
	t := sched.Next(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC))
	if t.IsZero() {
		return 0, false
	}
	var first time.Duration
	// Sample across two days so an hour step's midnight wrap and a minute
	// step's top-of-hour wrap are both inside the window. The count bound is a
	// backstop for a pathological expression, not a tuning knob.
	const (
		window   = 48 * time.Hour
		maxFires = 4096
	)
	deadline := t.Add(window)
	for i := 0; i < maxFires; i++ {
		next := sched.Next(t)
		if next.IsZero() {
			return 0, false // fires at most once — no cadence to state
		}
		gap := next.Sub(t)
		switch {
		case i == 0:
			first = gap
		case gap != first:
			return 0, false
		}
		t = next
		if t.After(deadline) {
			return first, first > 0
		}
	}
	return 0, false
}

// isLiteralField reports whether a cron field is a single literal value, i.e.
// carries no wildcard, list, range or step — the only shape that can be
// interpolated straight into a label like "hourly :30".
func isLiteralField(field string) bool {
	return field != "" && !strings.ContainsAny(field, "*,-/")
}

// LabelOrRaw is Label with the raw expression as its fallback, for surfaces
// with room for exactly one field and no second place to put the cron. A
// paraphrase is friendlier, but "0 */6 * * *" beats a blank cell.
func LabelOrRaw(schedule string) string {
	if l := Label(schedule); l != "" {
		return l
	}
	return schedule
}

// formatCronTime renders an hour:minute pair from cron fields, zero-padding
// each to two digits so the column reads as a clock ("9"/"30" -> "09:30",
// "0"/"0" -> "00:00"). Go's %0Ns applies the zero flag to strings, so the
// cron fields need no numeric conversion first.
func formatCronTime(hour, minute string) string {
	return fmt.Sprintf("%02s:%02s", hour, minute)
}

// cronDayName maps a single-value day-of-week field (0-7 or three-letter
// name) to its capitalized English name. Returns "" for ranges, lists, or
// wildcards.
func cronDayName(dow string) string {
	names := map[string]string{
		"0": "Sundays", "7": "Sundays",
		"1": "Mondays", "2": "Tuesdays", "3": "Wednesdays",
		"4": "Thursdays", "5": "Fridays", "6": "Saturdays",
		"SUN": "Sundays", "MON": "Mondays", "TUE": "Tuesdays",
		"WED": "Wednesdays", "THU": "Thursdays", "FRI": "Fridays",
		"SAT": "Saturdays",
	}
	return names[strings.ToUpper(dow)]
}

// Idle State Presentation For Scheduled Harnesses
//
// A cron one-shot spends almost all of its life not running, and the state
// machine calls that `stopped`. For an always-on harness that word is right —
// it means "not running and not wanted". For a scheduled one it is actively
// misleading: the harness is armed and will fire on its own, but the listing
// reads as though someone turned it off, in the same warm red-family color
// SPEC-0001 gives stopped so it "draws the eye".
//
// So the presentation layer renames that one combination — stopped AND
// scheduled — to `idle`, and callers pair it with amber rather than pink. No
// new core.State is involved: the state machine is unchanged and the wire
// still says "stopped". This is phrasing, exactly like Label and NextIn above,
// and it lives here for the same reason they do — `harness list`/`describe`
// and the cockpit dashboard must not phrase the same harness two ways.
//
// @joestump-agent 08/25/2026 - Added for issue #268. @joestump asked for
//   orange-not-red and "idle" after a scheduled sweep read as failed-adjacent
//   in `harness list`.

// IdleLabel is what a stopped scheduled harness is called instead of
// "stopped".
const IdleLabel = "idle"

// IsIdle reports whether this state/schedule pair is a scheduled harness
// resting between firings — the one combination StateLabel renames. Callers
// use it to pick amber over the stopped color; every other state of a
// scheduled harness (running, failed, degraded) keeps its own color, because
// each still means exactly what it says.
func IsIdle(state, schedule string) bool {
	return schedule != "" && core.State(state) == core.StateStopped
}

// StateLabel renders a harness's state for humans, substituting "idle" for a
// scheduled harness's "stopped". Every other state, and every unscheduled
// harness, is returned unchanged.
func StateLabel(state, schedule string) string {
	if IsIdle(state, schedule) {
		return IdleLabel
	}
	return state
}

// ScheduleGlyph is the clock a scheduled harness shows in place of its state
// glyph. The color still carries the state (or amber, when idle); the glyph
// shape carries "this harness is cron-fired".
const ScheduleGlyph = "⏱"

// Glyph returns the status glyph for a harness: the clock for anything with a
// schedule, otherwise the SPEC-0003 state glyph. An unknown state falls back
// to a neutral bullet so a row is never blank.
//
// Here rather than per-surface for the same reason StateLabel is: `harness
// list` swapped in the clock, the cockpit did not, so the two surfaces drew
// the same harness with different icons — the exact drift this package was
// extracted to prevent. A caller that renders its own live spinner for the
// transient states (the TUI does) picks that before consulting this.
func Glyph(state, schedule string) string {
	if schedule != "" {
		return ScheduleGlyph
	}
	s := core.State(state)
	if !s.Valid() {
		return "·"
	}
	return s.Glyph()
}

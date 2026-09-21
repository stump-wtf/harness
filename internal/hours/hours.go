// Package hours parses and evaluates operating_hours expressions: the weekly
// windows during which a resident harness is allowed to run (ADR-0019).
//
// The package is deliberately pure: no clock, no I/O. Parse turns a string
// into an Expr; In answers "is t inside a window, and when does that answer
// next change" for a caller-supplied t. Every temporal decision — including
// which instant "now" is — stays outside this package, in the scheduler's
// clock seam, so the grammar and the membership test are exhaustively table-
// testable and the wall-clock guard test (guard_test.go) can hold the
// invariant mechanically.
//
// Governing: ADR-0019 (operating hours), SPEC-0012 REQ "Operating Hours Key".
package hours

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// weekOrder is Mon..Sun, the order the grammar's day ranges expand in
// ("Fri-Mon" is Fri, Sat, Sun, Mon) — NOT time.Weekday's Sun-first order.
var weekOrder = [7]time.Weekday{
	time.Monday, time.Tuesday, time.Wednesday, time.Thursday,
	time.Friday, time.Saturday, time.Sunday,
}

// weekIndex returns wd's position in weekOrder (Mon=0 .. Sun=6).
func weekIndex(wd time.Weekday) int { return (int(wd) + 6) % 7 }

var dayNames = map[string]time.Weekday{
	"mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday, "sun": time.Sunday,
}

// dayLabel is weekOrder's canonical three-letter spelling, used by String().
var dayLabel = [7]string{"Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"}

// window is one expanded, single-day occurrence: in hours from start up to
// (not including) end, both in seconds since that day's local midnight. end
// is in (0, 86400]; 86400 spells 24:00. A window is overnight — it runs past
// midnight into the following day — when end <= start (see overnight()).
//
// Governing: SPEC-0012 REQ "Operating Hours Key" (day ranges/lists are
// expanded to one window per day at parse time, so evaluation never re-walks
// the grammar).
type window struct {
	day        time.Weekday
	start, end int
}

// overnight reports whether w runs past midnight into the following day: an
// end at or before its start, other than the explicit end-of-day 24:00 (which
// normalizes to end == 86400, always > any valid start).
func (w window) overnight() bool { return w.end <= w.start }

// Expr is a parsed operating_hours expression: a zone plus the windows it
// resolved to. The zero Expr is not meaningful on its own; obtain one from
// Parse. Expr carries no clock and does no I/O — see the package doc.
type Expr struct {
	// zoneText is the zone name as written after TZ=/CRON_TZ=, or "" when the
	// expression carries no prefix (the caller's zone — the daemon's local
	// zone in production, per SPEC-0012 REQ "Operating Hours Key").
	zoneText string
	loc      *time.Location
	// windows is sorted by (weekIndex(day), start, end) for a deterministic
	// String() and stable equality in tests.
	windows []window
}

// location returns e's resolved zone, defaulting to time.Local for the zero
// Expr or one parsed with no TZ=/CRON_TZ= prefix.
func (e Expr) location() *time.Location {
	if e.loc != nil {
		return e.loc
	}
	return time.Local
}

// Parse validates and parses s against the operating_hours grammar (SPEC-0012
// REQ "Operating Hours Key"):
//
//	operating_hours = [ zone-prefix " " ] window *( ";" window )
//	zone-prefix     = ( "TZ=" / "CRON_TZ=" ) zone-name
//	window          = [ day-spec " " ] time "-" time
//	day-spec        = day-item *( "," day-item )
//	day-item        = day / day "-" day
//	day             = "Mon" / "Tue" / "Wed" / "Thu" / "Fri" / "Sat" / "Sun"   ; case-insensitive
//	time            = HH ":" MM                                          ; 00:00 through 24:00
//
// A blank value, an unknown day or zone, a malformed or out-of-range time, and
// a window whose start equals its end are each returned as an error naming
// the offending piece — the caller (internal/config) wraps it with the
// harness and key name (SPEC-0012 REQ "Operating Hours Key").
func Parse(s string) (Expr, error) {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return Expr{}, fmt.Errorf("must not be blank")
	}

	rest := trimmed
	var zoneText string
	var loc *time.Location
	if strings.HasPrefix(rest, "TZ=") || strings.HasPrefix(rest, "CRON_TZ=") {
		token := rest
		if sp := strings.IndexAny(rest, " \t"); sp >= 0 {
			token = rest[:sp]
			rest = strings.TrimSpace(rest[sp+1:])
		} else {
			rest = ""
		}
		eq := strings.IndexByte(token, '=')
		zoneText = token[eq+1:]
		if zoneText == "" {
			return Expr{}, fmt.Errorf("zone prefix %q: missing zone name", token)
		}
		l, err := time.LoadLocation(zoneText)
		if err != nil {
			// Governing: same zone resolution as SPEC-0008 REQ "Schedule Time
			// Zone" — time.LoadLocation, which the embedded tzdata import in
			// cmd/harness/tzdata.go covers for every caller in the binary
			// (robfig/cron's CRON_TZ= parsing included), so a harness.toml
			// that loads on a laptop loads the same way in a minimal
			// container with no /usr/share/zoneinfo.
			return Expr{}, fmt.Errorf("unknown time zone %q: %w", zoneText, err)
		}
		loc = l
	}
	if rest == "" {
		return Expr{}, fmt.Errorf("must specify at least one window")
	}

	var windows []window
	for _, seg := range strings.Split(rest, ";") {
		ws, err := parseWindow(seg)
		if err != nil {
			return Expr{}, err
		}
		windows = append(windows, ws...)
	}

	sort.Slice(windows, func(i, j int) bool {
		a, b := windows[i], windows[j]
		if weekIndex(a.day) != weekIndex(b.day) {
			return weekIndex(a.day) < weekIndex(b.day)
		}
		if a.start != b.start {
			return a.start < b.start
		}
		return a.end < b.end
	})

	return Expr{zoneText: zoneText, loc: loc, windows: windows}, nil
}

// parseWindow parses one ";"-separated window segment into its expanded
// per-day occurrences.
func parseWindow(seg string) ([]window, error) {
	trimmed := strings.TrimSpace(seg)
	if trimmed == "" {
		return nil, fmt.Errorf("empty window (check for a stray \";\")")
	}
	fields := strings.Fields(trimmed)

	var dayToken, timeToken string
	switch len(fields) {
	case 1:
		timeToken = fields[0]
	case 2:
		dayToken, timeToken = fields[0], fields[1]
	default:
		return nil, fmt.Errorf("window %q: malformed (want \"[days] HH:MM-HH:MM\")", trimmed)
	}

	days, err := parseDaySpec(dayToken)
	if err != nil {
		return nil, fmt.Errorf("window %q: %w", trimmed, err)
	}

	start, end, err := parseTimeRange(timeToken)
	if err != nil {
		return nil, fmt.Errorf("window %q: %w", trimmed, err)
	}
	if start == end {
		return nil, fmt.Errorf("window %q: start and end must not be equal", trimmed)
	}

	out := make([]window, len(days))
	for i, d := range days {
		out[i] = window{day: d, start: start, end: end}
	}
	return out, nil
}

// parseDaySpec parses a comma-separated day-item list into the set of days it
// names, expanding ranges in week order (Mon..Sun, wrapping: "Fri-Mon" is Fri,
// Sat, Sun, Mon). An empty token (no day-spec in the window) means every day.
func parseDaySpec(token string) ([]time.Weekday, error) {
	if token == "" {
		return weekOrder[:], nil
	}
	var days []time.Weekday
	seen := map[time.Weekday]bool{}
	add := func(wd time.Weekday) {
		if !seen[wd] {
			seen[wd] = true
			days = append(days, wd)
		}
	}
	for _, item := range strings.Split(token, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			return nil, fmt.Errorf("day list %q: empty day (check for a stray \",\")", token)
		}
		if a, b, ok := strings.Cut(item, "-"); ok {
			from, err := parseDay(a)
			if err != nil {
				return nil, err
			}
			to, err := parseDay(b)
			if err != nil {
				return nil, err
			}
			for i, n := weekIndex(from), 0; n < 7; i, n = (i+1)%7, n+1 {
				add(weekOrder[i])
				if i == weekIndex(to) {
					break
				}
			}
		} else {
			wd, err := parseDay(item)
			if err != nil {
				return nil, err
			}
			add(wd)
		}
	}
	return days, nil
}

// parseDay parses a single case-insensitive day abbreviation.
func parseDay(s string) (time.Weekday, error) {
	wd, ok := dayNames[strings.ToLower(strings.TrimSpace(s))]
	if !ok {
		return 0, fmt.Errorf("unknown day %q (want Mon, Tue, Wed, Thu, Fri, Sat, or Sun)", s)
	}
	return wd, nil
}

// parseTimeRange parses "HH:MM-HH:MM" into start/end seconds since midnight.
// 24:00 is valid only as the end.
func parseTimeRange(token string) (start, end int, err error) {
	if strings.Count(token, "-") != 1 {
		return 0, 0, fmt.Errorf("malformed time range %q (want \"HH:MM-HH:MM\")", token)
	}
	a, b, _ := strings.Cut(token, "-")
	start, err = parseTime(a, false)
	if err != nil {
		return 0, 0, fmt.Errorf("start %w", err)
	}
	end, err = parseTime(b, true)
	if err != nil {
		return 0, 0, fmt.Errorf("end %w", err)
	}
	return start, end, nil
}

// parseTime parses "HH:MM" into seconds since midnight. allow24 permits the
// single value 24:00 (spelled 86400 seconds); every other field must be a real
// wall-clock time (hour 0-23, minute 0-59).
func parseTime(s string, allow24 bool) (int, error) {
	hh, mm, ok := strings.Cut(s, ":")
	if !ok {
		return 0, fmt.Errorf("time %q: malformed (want \"HH:MM\")", s)
	}
	h, err := strconv.Atoi(hh)
	if err != nil || len(hh) != 2 {
		return 0, fmt.Errorf("time %q: malformed hour", s)
	}
	m, err := strconv.Atoi(mm)
	if err != nil || len(mm) != 2 {
		return 0, fmt.Errorf("time %q: malformed minute", s)
	}
	if h == 24 && m == 0 {
		if !allow24 {
			return 0, fmt.Errorf("time %q: 24:00 is valid only as a window's end", s)
		}
		return 24 * 3600, nil
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Errorf("time %q: out of range (want 00:00 through 23:59, or 24:00 as an end)", s)
	}
	return h*3600 + m*60, nil
}

// String renders e back into operating_hours syntax: a canonical form (every
// day-range/list expanded to one window per day, sorted) that Parse accepts
// and that evaluates identically to e, though not necessarily byte-identical
// to whatever text produced e — FuzzParse checks the round trip on that
// semantic contract, not on the exact bytes.
func (e Expr) String() string {
	var b strings.Builder
	if e.zoneText != "" {
		b.WriteString("TZ=")
		b.WriteString(e.zoneText)
		b.WriteByte(' ')
	}
	parts := make([]string, len(e.windows))
	for i, w := range e.windows {
		parts[i] = fmt.Sprintf("%s %s-%s", dayLabel[weekIndex(w.day)], fmtTime(w.start), fmtTime(w.end))
	}
	b.WriteString(strings.Join(parts, "; "))
	return b.String()
}

// fmtTime renders seconds-since-midnight as "HH:MM" (or "24:00" for 86400).
func fmtTime(secs int) string {
	return fmt.Sprintf("%02d:%02d", secs/3600, (secs%3600)/60)
}

// In reports whether t falls inside one of e's windows, read in e's zone. next
// is the first instant strictly after t at which that answer changes; ok is
// false when no such instant exists within the next eight local days — in
// practice, only when e's windows union to the entire week (SPEC-0012 REQ
// "Operating Hours Key": every valid expression repeats weekly, so an eight
// day search either finds the next flip or proves there isn't one).
func (e Expr) In(t time.Time) (in bool, next time.Time, ok bool) {
	in = e.member(t)
	next, ok = e.nextChange(t, in)
	return in, next, ok
}

// member is In's instant-only half: is t, converted to e's zone, inside a
// window? Membership needs no wall-clock-to-instant conversion (only the
// reverse, t.In(loc), which is unambiguous even across a DST transition) — see
// the package doc and design.md § "A pure internal/hours package".
func (e Expr) member(t time.Time) bool {
	loc := e.location()
	lt := t.In(loc)
	wd := lt.Weekday()
	prevWd := weekOrder[(weekIndex(wd)+6)%7]
	secs := lt.Hour()*3600 + lt.Minute()*60 + lt.Second()
	for _, w := range e.windows {
		switch {
		case w.day == wd && w.overnight():
			if secs >= w.start {
				return true
			}
		case w.day == wd:
			if secs >= w.start && secs < w.end {
				return true
			}
		case w.day == prevWd && w.overnight():
			if secs < w.end {
				return true
			}
		}
	}
	return false
}

// nextStep bounds nextChange's coarse scan: half the grammar's finest
// granularity (a minute), so no window is ever short enough in real time to
// fall entirely between two samples — including one a spring-forward gap has
// shrunk, whose real duration that day can (correctly) come out under this
// and be found as never having occurred, not as a false flip. A boundary-
// candidate walk (window starts/ends over the next eight days, as design.md
// sketches) would be faster in the common case, but a boundary whose labeled
// wall-clock instant falls inside a DST gap resolves to a real instant either
// side of where membership actually changes — the change happens at the
// gap's own start, not at the label — which a fixed grid sampled directly
// from member() (already DST-safe; see member's doc) cannot get wrong.
const nextStep = 30 * time.Second

// nextHorizon bounds nextChange's scan. Every valid expression repeats
// weekly, so a flip either turns up within one week plus a day of slack, or
// the expression covers the entire week and there is no flip to find.
const nextHorizon = 8 * 24 * time.Hour

// nextChange finds the first instant after t where member's answer differs
// from curIn (which must equal e.member(t)), scanning forward in nextStep
// increments and bisecting the bracket it lands in down to the second — the
// daemon's own tick resolution, so finer precision would be unobservable.
func (e Expr) nextChange(t time.Time, curIn bool) (time.Time, bool) {
	deadline := t.Add(nextHorizon)
	prev := t
	for cur := t.Add(nextStep); !cur.After(deadline); cur = cur.Add(nextStep) {
		if e.member(cur) != curIn {
			return bisectFlip(e, prev, cur, curIn), true
		}
		prev = cur
	}
	return time.Time{}, false
}

// bisectFlip narrows [lo, hi] to the second — member(lo) == curIn and
// member(hi) != curIn hold on entry and are preserved as loop invariants — and
// returns hi, the first instant past the flip.
func bisectFlip(e Expr, lo, hi time.Time, curIn bool) time.Time {
	for hi.Sub(lo) > time.Second {
		mid := lo.Add(hi.Sub(lo) / 2)
		if e.member(mid) == curIn {
			lo = mid
		} else {
			hi = mid
		}
	}
	return hi.Round(0)
}

// PrevEnd reports the most recent instant at or before t at which one of e's
// windows closed — the in→out flip closest behind t. It is the instant a gated
// harness went out of hours, which SPEC-0012 REQ "Graceful Shutdown" anchors a
// close's deadline to ("measured from that instant and not from when the
// daemon noticed it"), so a suspend across the boundary still yields a
// deadline on the real timeline.
//
// ok is false when no such flip exists within the past eight local days — in
// practice only when e's windows union to the entire week — or when t is in
// hours, where the current window's end is still ahead and there is no closed
// window behind the question. The scan mirrors nextChange's: backward in
// nextStep increments, then bisected to the second, so a boundary whose label
// falls in a DST gap resolves to where membership actually flips, exactly as
// In does.
func (e Expr) PrevEnd(t time.Time) (time.Time, bool) {
	if e.member(t) {
		return time.Time{}, false
	}
	floor := t.Add(-nextHorizon)
	hi := t // member(hi) == false: the loop invariant bisectFlip needs
	for cur := t.Add(-nextStep); !cur.Before(floor); cur = cur.Add(-nextStep) {
		if e.member(cur) {
			return bisectFlip(e, cur, hi, true), true
		}
		hi = cur
	}
	return time.Time{}, false
}

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
package schedfmt

import (
	"fmt"
	"strings"
	"time"
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
		return fmt.Sprintf("hourly :%s", minute)
	}
	return ""
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

// formatCronTime renders an hour:minute pair from cron fields, dropping
// leading zeros for readability ("09:30" not "9:30", but "0:00" stays).
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

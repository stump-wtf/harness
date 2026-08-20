package schedfmt

// Governing: ADR-0013 (scheduled one-shot jobs), SPEC-0003 (state legibility).
//
// These tables moved here from cmd/harness when the TUI grew a second caller
// (issue #160) — the phrasing is a shared contract now, so it is pinned once
// where both surfaces read it.
//
// @joestump-agent 08/20/2026 - Moved from cmd/harness/schedule_meta_test.go
// and extended with the NextIn/NextInAt boundaries.

import (
	"testing"
	"time"
)

func TestLabel(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"every", "@every 6h", "every 6h"},
		{"daily-descriptor", "@daily", "daily"},
		{"hourly-descriptor", "@hourly", "hourly"},
		{"weekly-descriptor", "@weekly", "weekly"},
		{"monthly-descriptor", "@monthly", "monthly"},
		{"daily-cron", "30 9 * * *", "daily 09:30"},
		{"weekly-cron", "0 7 * * 1", "Mondays 07:00"},
		{"weekly-cron-named", "0 7 * * MON", "Mondays 07:00"},
		{"hourly-cron", "15 * * * *", "hourly :15"},
		{"monthly-cron", "0 0 15 * *", "monthly 00:00"},
		{"six-hourly", "0 */6 * * *", ""}, // wildcard hour, not a simple label
		{"wrong-field-count", "0 7 * *", ""},
	}
	for _, tc := range cases {
		if got := Label(tc.in); got != tc.want {
			t.Errorf("%s: Label(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// TestLabelOrRaw pins the fallback the TUI meta line depends on: an expression
// too irregular to paraphrase must still render, or a `0 */6 * * *` harness
// shows no cadence at all on the only surface that carries one.
func TestLabelOrRaw(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"30 9 * * *", "daily 09:30"},
		{"0 */6 * * *", "0 */6 * * *"},
	}
	for _, tc := range cases {
		if got := LabelOrRaw(tc.in); got != tc.want {
			t.Errorf("LabelOrRaw(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestNextIn covers the two inputs that must render as "nothing to say"
// rather than as a confident time: no schedule at all, and a daemon that has
// not resolved a firing time yet (or sent something unparseable).
func TestNextIn(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"malformed", "not-a-time", ""},
		{"past", time.Now().Add(-time.Minute).Format(time.RFC3339), "due"},
		{"future", time.Now().Add(90 * time.Minute).Format(time.RFC3339), "in 1h30m"},
	}
	for _, tc := range cases {
		if got := NextIn(tc.in); got != tc.want {
			t.Errorf("%s: NextIn(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// TestNextInAt pins the boundary against a fixed clock: exactly-now is "due",
// not "in 0s".
func TestNextInAt(t *testing.T) {
	now := time.Date(2026, 8, 20, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		next time.Time
		want string
	}{
		{"exactly now", now, "due"},
		{"one second ago", now.Add(-time.Second), "due"},
		{"in 45s", now.Add(45 * time.Second), "in 45s"},
		{"in 2h", now.Add(2 * time.Hour), "in 2h"},
	}
	for _, tc := range cases {
		if got := NextInAt(tc.next, now); got != tc.want {
			t.Errorf("%s: got %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestShortDuration(t *testing.T) {
	cases := []struct {
		in   time.Duration
		want string
	}{
		{45 * time.Second, "45s"},
		{5 * time.Minute, "5m"},
		{90 * time.Minute, "1h30m"},
		{2 * time.Hour, "2h"},
		// Rounding carries into the next unit rather than reporting "60m".
		{59*time.Minute + 45*time.Second, "1h"},
		{52 * time.Hour, "2d4h"},
		{48 * time.Hour, "2d"},
	}
	for _, tc := range cases {
		if got := ShortDuration(tc.in); got != tc.want {
			t.Errorf("%v: got %q, want %q", tc.in, got, tc.want)
		}
	}
}

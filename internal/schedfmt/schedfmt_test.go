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
	"strings"
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
		// A pure hour step at a fixed minute is an interval, and must phrase
		// identically to the @every spelling of the same cadence above.
		{"six-hourly", "0 */6 * * *", "every 6h"},
		{"two-hourly-offset", "30 */2 * * *", "every 2h"},
		{"twelve-hourly", "0 */12 * * *", "every 12h"},
		{"minute-step", "*/15 * * * *", "every 15m"},
		{"minute-step-30", "*/30 * * * *", "every 30m"},
		// Steps that do not divide their field evenly are NOT intervals: */7
		// fires at 0,7,14,21 and then wraps only 3h to midnight, so claiming
		// "every 7h" would be wrong for a quarter of the day. Say nothing.
		{"uneven-hour-step", "0 */7 * * *", ""},
		{"uneven-minute-step", "*/7 * * * *", ""},
		// A minute list is not "hourly :MM" — it fires more than once an hour,
		// and used to render as "hourly :0,30". Deriving from real firing
		// times names it properly: :00 and :30 every hour IS a constant 30m
		// cadence. An earlier hand-written rule declined this one.
		{"minute-list", "0,30 * * * *", "every 30m"},
		// A list of minutes fires more than once per six-hour block, so the
		// hour step alone does not describe it and the gaps are uneven.
		{"multi-minute-in-interval", "0,30 */6 * * *", ""},
		// Deriving from real firing times handles these without a rule per
		// shape: a range-with-step, and a step whose phase is offset.
		{"range-step", "0 8-18/2 * * *", ""}, // 8..18 then a 14h jump to 08:00
		{"every-3h", "0 */3 * * *", "every 3h"},
		{"every-20m", "*/20 * * * *", "every 20m"},
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
		// Now paraphrasable, so LabelOrRaw no longer falls back to the raw
		// cron for this one — the fallback still covers genuinely irregular
		// expressions like the uneven step below.
		{"0 */6 * * *", "every 6h"},
		{"0 */7 * * *", "0 */7 * * *"},
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

// TestScheduledHarnessDescriptionsHighlight reproduces the reported bug
// end-to-end. The DESCRIPTION cell highlights a cadence only when Label's
// output appears verbatim in the operator's description string, so a label
// this package declines to produce silently renders that harness's cadence in
// plain text while its neighbours are highlighted — which is exactly how the
// "every 6h" row looked next to "daily 09:30" and "Mondays 07:00".
//
// These are the three real scheduled harnesses from harness.d.
func TestScheduledHarnessDescriptionsHighlight(t *testing.T) {
	rows := []struct{ sched, desc string }{
		{"0 */6 * * *", "StumpCloud health sweep · every 6h · glm-5.2 (Hyper)"},
		{"30 9 * * *", "PR feedback sweep (own PRs, both forges) · daily 09:30 · glm-5.2 (Hyper)"},
		{"0 7 * * 1", "Issue/PR grooming across forges · Mondays 07:00 · glm-5.2 (Hyper)"},
	}
	for _, r := range rows {
		l := Label(r.sched)
		if l == "" {
			t.Errorf("Label(%q) = \"\", so its cadence renders unhighlighted", r.sched)
			continue
		}
		if !strings.Contains(r.desc, l) {
			t.Errorf("Label(%q) = %q, not present in %q", r.sched, l, r.desc)
		}
	}
}

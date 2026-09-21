package hours

// Governing: ADR-0019, SPEC-0012 REQ "Operating Hours Key" — every WHEN/THEN
// scenario in the requirement is exercised here as a table test, plus DST
// (spring-forward gap, fall-back repeat) coverage for In and next, and the
// ok=false case for an expression covering the entire week.

import (
	"strings"
	"testing"
	"time"
)

func mustParse(t *testing.T, s string) Expr {
	t.Helper()
	e, err := Parse(s)
	if err != nil {
		t.Fatalf("Parse(%q): unexpected error: %v", s, err)
	}
	return e
}

// utc is a convenience constructor for a UTC instant.
func utc(y int, mo time.Month, d, h, mi, s int) time.Time {
	return time.Date(y, mo, d, h, mi, s, 0, time.UTC)
}

// ---- Parse: grammar acceptance ---------------------------------------------

func TestParseAccepts(t *testing.T) {
	for _, s := range []string{
		"09:00-13:00",
		"Mon-Fri 09:00-13:00",
		"TZ=America/Los_Angeles Mon-Fri 09:00-13:00",
		"CRON_TZ=UTC Mon-Fri 09:00-13:00",
		"Mon-Fri 09:00-12:00; Mon-Fri 13:00-17:00",
		"Sat,Sun 10:00-12:00",
		"Sun-Thu 22:00-02:00",
		"Mon-Sun 00:00-24:00",
		"Fri 22:00-02:00",
		"Fri-Mon 10:00-11:00",
		"  Mon-Fri   09:00-13:00  ",
		"Mon-Fri 09:00-12:00 ; Mon-Fri 13:00-17:00",
		"mon-fri 09:00-13:00",
	} {
		if _, err := Parse(s); err != nil {
			t.Errorf("Parse(%q): unexpected error: %v", s, err)
		}
	}
}

func TestParseRejects(t *testing.T) {
	cases := []struct {
		s    string
		want string // substring the error must contain
	}{
		{"", "blank"},
		{"   ", "blank"},
		{"Mon 09:00-09:00", "start and end must not be equal"},
		{"TZ=Mars/Olympus 09:00-13:00", "Mars/Olympus"},
		{"TZ= 09:00-13:00", "missing zone name"},
		{"Xyz 09:00-13:00", "unknown day"},
		{"Mon-Xyz 09:00-13:00", "unknown day"},
		{"Mon,,Tue 09:00-13:00", "empty day"},
		{"Mon Tue 09:00-13:00", "malformed"},
		{"25:00-13:00", "out of range"},
		{"09:60-13:00", "out of range"},
		{"9:00-13:00", "malformed hour"},
		{"09:0-13:00", "malformed minute"},
		{"0900-1300", "malformed"},
		{"09:00-13:00-15:00", "malformed time range"},
		{"24:00-13:00", "24:00 is valid only as a window's end"},
		{"09:00-", "malformed"},
		{"09:00-13:00;", "empty window"},
		{";09:00-13:00", "empty window"},
	}
	for _, c := range cases {
		_, err := Parse(c.s)
		if err == nil {
			t.Errorf("Parse(%q): expected error, got nil", c.s)
			continue
		}
		if !strings.Contains(err.Error(), c.want) {
			t.Errorf("Parse(%q): error = %q, want substring %q", c.s, err.Error(), c.want)
		}
	}
}

// ---- REQ "Operating Hours Key" scenarios -----------------------------------

// Scenario: Weekday window in a named zone.
func TestScenarioNamedZoneWindow(t *testing.T) {
	e := mustParse(t, "TZ=America/Los_Angeles Mon-Fri 09:00-13:00")
	loc, err := time.LoadLocation("America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	// Monday 09:00 Pacific, whatever zone the daemon runs in.
	mon900 := time.Date(2026, 9, 21, 9, 0, 0, 0, loc)
	if in, _, _ := e.In(mon900); !in {
		t.Error("expected in hours at Monday 09:00 Pacific")
	}
	mon1259 := time.Date(2026, 9, 21, 12, 59, 59, 0, loc)
	if in, _, _ := e.In(mon1259); !in {
		t.Error("expected in hours at Monday 12:59:59 Pacific")
	}
	mon1300 := time.Date(2026, 9, 21, 13, 0, 0, 0, loc)
	if in, _, _ := e.In(mon1300); in {
		t.Error("expected out of hours at Monday 13:00 Pacific")
	}
	sat900 := time.Date(2026, 9, 26, 9, 0, 0, 0, loc)
	if in, _, _ := e.In(sat900); in {
		t.Error("expected out of hours at Saturday 09:00 Pacific")
	}
	// Same instant, read from a completely different daemon zone (UTC):
	// membership must agree, because the expression is evaluated in ITS zone.
	if in, _, _ := e.In(mon900.In(time.UTC)); !in {
		t.Error("expected in hours evaluating the same instant from UTC")
	}
}

// Scenario: Lunch break.
func TestScenarioLunchBreak(t *testing.T) {
	// Pinned to UTC (rather than left to the daemon's/test host's local
	// zone) purely so the UTC test instants below are deterministic; the
	// scenario itself is zone-agnostic.
	e := mustParse(t, "TZ=UTC Mon-Fri 09:00-12:00; Mon-Fri 13:00-17:00")
	// Monday 2026-09-21.
	if in, _, _ := e.In(utc(2026, 9, 21, 12, 30, 0)); in {
		t.Error("expected 12:30 out of hours")
	}
	if in, _, _ := e.In(utc(2026, 9, 21, 13, 0, 0)); !in {
		t.Error("expected 13:00 in hours")
	}
}

// Scenario: Overnight window.
func TestScenarioOvernightWindow(t *testing.T) {
	// Pinned to UTC for the same reason as TestScenarioLunchBreak.
	e := mustParse(t, "TZ=UTC Fri 22:00-02:00")
	// Friday 2026-09-25, Saturday 2026-09-26.
	if in, _, _ := e.In(utc(2026, 9, 25, 23, 0, 0)); !in {
		t.Error("expected Friday 23:00 in hours")
	}
	if in, _, _ := e.In(utc(2026, 9, 26, 1, 59, 0)); !in {
		t.Error("expected Saturday 01:59 in hours")
	}
	if in, _, _ := e.In(utc(2026, 9, 26, 2, 0, 0)); in {
		t.Error("expected Saturday 02:00 out of hours")
	}
	if in, _, _ := e.In(utc(2026, 9, 25, 21, 59, 0)); in {
		t.Error("expected Friday 21:59 out of hours")
	}
}

// Scenario: Wrapping day range.
func TestScenarioWrappingDayRange(t *testing.T) {
	// Pinned to UTC for the same reason as TestScenarioLunchBreak.
	e := mustParse(t, "TZ=UTC Fri-Mon 10:00-11:00")
	// Week of 2026-09-21 (Mon) .. 2026-09-27 (Sun): Fri=09-25, Sat=09-26,
	// Sun=09-27, Mon=09-28, Tue=09-29.
	for _, day := range []int{25, 26, 27, 28} {
		if in, _, _ := e.In(utc(2026, 9, day, 10, 30, 0)); !in {
			t.Errorf("expected 2026-09-%d 10:30 in hours", day)
		}
	}
	if in, _, _ := e.In(utc(2026, 9, 29, 10, 30, 0)); in {
		t.Error("expected Tuesday 10:30 out of hours")
	}
}

// Scenario: Equal start and end.
func TestScenarioEqualStartEnd(t *testing.T) {
	_, err := Parse("Mon 09:00-09:00")
	if err == nil || !strings.Contains(err.Error(), "start and end must not be equal") {
		t.Fatalf("expected equal-start-end error, got %v", err)
	}
}

// Scenario: Unknown zone.
func TestScenarioUnknownZone(t *testing.T) {
	_, err := Parse("TZ=Mars/Olympus 09:00-13:00")
	if err == nil || !strings.Contains(err.Error(), "Mars/Olympus") {
		t.Fatalf("expected error naming the zone, got %v", err)
	}
}

// ---- Always window (ok=false) ----------------------------------------------

func TestAlwaysWindowNeverFlips(t *testing.T) {
	e := mustParse(t, "Mon-Sun 00:00-24:00")
	in, next, ok := e.In(utc(2026, 9, 21, 12, 0, 0))
	if !in {
		t.Fatal("expected always in hours")
	}
	if ok {
		t.Fatalf("expected ok=false for a whole-week expression, got next=%v", next)
	}
	if !next.IsZero() {
		t.Errorf("expected zero next, got %v", next)
	}
}

// ---- DST: spring forward (In) ----------------------------------------------

func TestDSTSpringForwardIn(t *testing.T) {
	e := mustParse(t, "TZ=America/New_York 01:30-02:30")
	// 2026-03-08: America/New_York springs forward at 02:00 local -> 03:00.
	if in, _, _ := e.In(utc(2026, 3, 8, 6, 59, 59)); !in { // 01:59:59 EST
		t.Error("expected in hours at 01:59:59 EST")
	}
	if in, _, _ := e.In(utc(2026, 3, 8, 7, 0, 0)); in { // 03:00:00 EDT
		t.Error("expected out of hours at 03:00:00 EDT")
	}
}

// ---- DST: fall back (In) ---------------------------------------------------

func TestDSTFallBackIn(t *testing.T) {
	e := mustParse(t, "TZ=America/New_York 01:00-02:00")
	// 2026-11-01: America/New_York falls back at 02:00 EDT -> 01:00 EST, so
	// 01:00-01:59 local happens twice.
	if in, _, _ := e.In(utc(2026, 11, 1, 5, 30, 0)); !in { // 01:30 EDT (first)
		t.Error("expected in hours during the first 01:30 (EDT)")
	}
	if in, _, _ := e.In(utc(2026, 11, 1, 6, 30, 0)); !in { // 01:30 EST (second)
		t.Error("expected in hours during the second 01:30 (EST)")
	}
}

// ---- DST: next across a spring-forward gap ---------------------------------

func TestDSTSpringForwardNext(t *testing.T) {
	e := mustParse(t, "TZ=America/New_York 01:30-02:30")

	// Before the window opens: next is the 01:30 EST open, same day.
	in, next, ok := e.In(utc(2026, 3, 8, 6, 0, 0)) // 01:00 EST
	if in {
		t.Fatal("expected out of hours at 01:00 EST")
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if want := utc(2026, 3, 8, 6, 30, 0); !next.Equal(want) { // 01:30 EST
		t.Errorf("next = %v, want %v", next, want)
	}

	// Inside the window, just before the gap: the label 02:30 never occurs
	// that day (the clock jumps straight from 01:59:59 EST to 03:00:00 EDT),
	// so the window is real-time-shortened and the close happens at the gap's
	// own start, not at a label.
	in, next, ok = e.In(utc(2026, 3, 8, 6, 59, 0)) // 01:59 EST
	if !in {
		t.Fatal("expected in hours at 01:59 EST")
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if want := utc(2026, 3, 8, 7, 0, 0); !next.Equal(want) { // 03:00 EDT, the jump
		t.Errorf("next = %v, want %v (the gap's start)", next, want)
	}
}

// ---- DST: next across a fall-back repeat -----------------------------------

func TestDSTFallBackNext(t *testing.T) {
	e := mustParse(t, "TZ=America/New_York 01:00-02:00")

	// From just before the window opens (its first, EDT occurrence).
	in, next, ok := e.In(utc(2026, 11, 1, 4, 59, 0)) // 00:59 EDT
	if in {
		t.Fatal("expected out of hours at 00:59 EDT")
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if want := utc(2026, 11, 1, 5, 0, 0); !next.Equal(want) { // 01:00 EDT
		t.Errorf("next = %v, want %v", next, want)
	}

	// The window's end label (02:00) is repeated too (it occurs only once,
	// at 02:00 EST, since the fall-back repeats 01:00-01:59, not 02:00
	// itself) — from inside the second (EST) occurrence, next is that close.
	in, next, ok = e.In(utc(2026, 11, 1, 6, 30, 0)) // 01:30 EST (second)
	if !in {
		t.Fatal("expected in hours at 01:30 EST")
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if want := utc(2026, 11, 1, 7, 0, 0); !next.Equal(want) { // 02:00 EST
		t.Errorf("next = %v, want %v", next, want)
	}
}

// ---- next: plain (non-DST) cases -------------------------------------------

func TestNextPlain(t *testing.T) {
	// Pinned to UTC for the same reason as TestScenarioLunchBreak.
	e := mustParse(t, "TZ=UTC Mon-Fri 09:00-13:00")
	// Monday 2026-09-21, well before the window.
	in, next, ok := e.In(utc(2026, 9, 21, 8, 0, 0))
	if in {
		t.Fatal("expected out of hours")
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if want := utc(2026, 9, 21, 9, 0, 0); !next.Equal(want) {
		t.Errorf("next = %v, want %v", next, want)
	}

	// Friday afternoon: next open is Monday 09:00.
	in, next, ok = e.In(utc(2026, 9, 25, 14, 0, 0))
	if in {
		t.Fatal("expected out of hours")
	}
	if !ok {
		t.Fatal("expected ok=true")
	}
	if want := utc(2026, 9, 28, 9, 0, 0); !next.Equal(want) {
		t.Errorf("next = %v, want %v", next, want)
	}
}

// ---- String / round trip ----------------------------------------------------

func TestStringRoundTrips(t *testing.T) {
	for _, s := range []string{
		"09:00-13:00",
		"Mon-Fri 09:00-13:00",
		"TZ=America/Los_Angeles Mon-Fri 09:00-13:00",
		"Mon-Fri 09:00-12:00; Mon-Fri 13:00-17:00",
		"Fri 22:00-02:00",
		"Fri-Mon 10:00-11:00",
		"Mon-Sun 00:00-24:00",
	} {
		e1 := mustParse(t, s)
		s2 := e1.String()
		e2, err := Parse(s2)
		if err != nil {
			t.Fatalf("Parse(%q) [String() of %q]: %v", s2, s, err)
		}
		// Semantic equivalence: sample a spread of instants across the week
		// and require agreement, rather than requiring identical structs (the
		// package makes no promise about internal field layout).
		base := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC) // a Monday
		for i := 0; i < 7*24; i++ {
			sample := base.Add(time.Duration(i) * time.Hour)
			in1, _, _ := e1.In(sample)
			in2, _, _ := e2.In(sample)
			if in1 != in2 {
				t.Fatalf("round trip %q -> %q diverges at %v: %v != %v", s, s2, sample, in1, in2)
			}
		}
	}
}

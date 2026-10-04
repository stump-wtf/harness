package budget

// Governing: SPEC-0021 REQ-3 "The budget day" — DST and zone arithmetic is
// where a hand-checked table misses a case, so FuzzDay checks Day's
// invariants against arbitrary instants, zones and day_starts.
//
// @joestump 10/04/2026 - Added for stump.wtf/harness#470.

import (
	"fmt"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/hours"
)

// fuzzZones covers DST in both hemispheres, a half-hour offset, a 30-minute
// DST shift (Lord Howe), a zone that skipped a whole day (Pacific/Apia,
// 2011-12-30), and no DST at all.
var fuzzZones = []string{
	"UTC", "America/Los_Angeles", "Europe/Berlin", "Australia/Sydney",
	"Asia/Kolkata", "Australia/Lord_Howe", "Pacific/Apia", "America/Sao_Paulo",
}

// FuzzDay asserts, for any instant and day_starts: start <= now < next, the
// day is between 22 and 26 hours long except where a zone dropped a calendar
// date, Day(start) is the same day, and Day(next) is the day after it.
func FuzzDay(f *testing.F) {
	f.Add(int64(1790000000), uint8(0), uint8(0), uint8(1))
	f.Add(int64(1793523600), uint8(2), uint8(30), uint8(1))  // near the 2026 US fall-back
	f.Add(int64(1772961600), uint8(2), uint8(30), uint8(1))  // near the 2026 US spring-forward
	f.Add(int64(1325152800), uint8(0), uint8(0), uint8(6))   // Apia's missing 2011-12-30
	f.Add(int64(1775350800), uint8(23), uint8(59), uint8(5)) // Lord Howe
	f.Fuzz(func(t *testing.T, unix int64, hh, mm, zone uint8) {
		if unix < 0 || unix > 4102444800 { // 1970 through 2100
			return
		}
		name := fuzzZones[int(zone)%len(fuzzZones)]
		spec, err := hours.ParseDailyInstant(fmt.Sprintf("TZ=%s %02d:%02d", name, hh%24, mm%60))
		if err != nil {
			t.Fatal(err)
		}
		now := time.Unix(unix, 0)
		start, next := Day(now, spec)
		if now.Before(start) || !now.Before(next) {
			t.Fatalf("Day(%v, %s) = [%v, %v) does not contain now", now, spec, start, next)
		}
		if l := next.Sub(start); (l < 22*time.Hour || l > 26*time.Hour) && name != "Pacific/Apia" {
			t.Fatalf("Day(%v, %s) is %v long", now, spec, l)
		}
		if s, n := Day(start, spec); !s.Equal(start) || !n.Equal(next) {
			t.Fatalf("Day(start=%v) = [%v, %v), want the same day [%v, %v)", start, s, n, start, next)
		}
		if s, _ := Day(next, spec); !s.Equal(next) {
			t.Fatalf("Day(next=%v) starts %v, want %v", next, s, next)
		}
	})
}

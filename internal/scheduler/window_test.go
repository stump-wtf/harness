package scheduler

// Governing: ADR-0013, SPEC-0008 REQ "Schedule Time Zone"; issue #117.

import (
	"testing"
	"time"
)

// TestLabelInstant pins how a wall-clock label resolves across every shape of
// transition: an ordinary hour, a spring-forward gap, a fall-back repeat, and a
// zone whose offset and DST shift are both half an hour.
func TestLabelInstant(t *testing.T) {
	ny := mustZone(t, "America/New_York")
	kolkata := mustZone(t, "Asia/Kolkata")
	lordHowe := mustZone(t, "Australia/Lord_Howe") // +10:30 standard, +11:00 DST

	cases := []struct {
		name  string
		label time.Time
		zone  *time.Location
		want  time.Time
	}{
		{"ordinary hour", utc(2026, 9, 11, 9, 0, 0), ny, utc(2026, 9, 11, 13, 0, 0)},
		// 02:30 never exists on 2026-03-08; it runs at 02:30 EST, which reads
		// 03:30 EDT. time.Date alone would have picked 01:30 EST — before the jump.
		{"spring-forward gap", utc(2026, 3, 8, 2, 30, 0), ny, utc(2026, 3, 8, 7, 30, 0)},
		{"fall-back repeat takes the first occurrence", utc(2026, 11, 1, 1, 30, 0), ny, utc(2026, 11, 1, 5, 30, 0)},
		{"half-hour offset", utc(2026, 9, 11, 9, 0, 0), kolkata, utc(2026, 9, 11, 3, 30, 0)},
		{"half-hour DST gap", utc(2026, 10, 4, 2, 15, 0), lordHowe, utc(2026, 10, 3, 15, 45, 0)},
		{"UTC", utc(2026, 9, 11, 9, 0, 0), time.UTC, utc(2026, 9, 11, 9, 0, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := labelInstant(tc.label, tc.zone); !got.Equal(tc.want) {
				t.Errorf("labelInstant(%s in %s) = %v, want %v", tc.label.Format("2006-01-02 15:04"), tc.zone, got.UTC(), tc.want)
			}
		})
	}
}

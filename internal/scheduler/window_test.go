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

// TestArmedAfterSpringForwardRunsTheGapWindow: a schedule armed after the
// clock jumps but before its gap window's resolved instant — 03:10 EDT, with
// "30 2 * * *" resolving to 03:30 EDT — still runs that window today. Reading
// the arm time at the post-jump offset started the label walk at 03:10, past
// the 02:30 label, and silently moved the run to tomorrow. A schedule armed
// after 03:30 EDT has genuinely passed today's window and waits.
func TestArmedAfterSpringForwardRunsTheGapWindow(t *testing.T) {
	ny := mustZone(t, "America/New_York")
	for _, tc := range []struct {
		name  string
		armed time.Time
		want  time.Time
	}{
		{"armed before the jump", utc(2026, 3, 8, 6, 45, 0), utc(2026, 3, 8, 7, 30, 0)}, // 01:45 EST
		{"armed at the jump", utc(2026, 3, 8, 7, 0, 0), utc(2026, 3, 8, 7, 30, 0)},      // 03:00 EDT
		{"armed after the jump", utc(2026, 3, 8, 7, 10, 0), utc(2026, 3, 8, 7, 30, 0)},  // 03:10 EDT
		{"armed at the resolved window", utc(2026, 3, 8, 7, 30, 0), utc(2026, 3, 9, 6, 30, 0)},
		{"armed after the resolved window", utc(2026, 3, 8, 7, 45, 0), utc(2026, 3, 9, 6, 30, 0)},
		{"armed the next day", utc(2026, 3, 9, 6, 10, 0), utc(2026, 3, 9, 6, 30, 0)}, // 02:10 EDT
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, tc.armed, ny)
			r.s.Apply(cfgOf(job{"sweep", "30 2 * * *", false}))
			if next, _ := r.s.NextFire("sweep"); !next.Equal(tc.want) {
				t.Errorf("NextFire = %v, want %v", next.UTC(), tc.want)
			}
			r.runUntil(utc(2026, 3, 10, 12, 0, 0), 30*time.Second)
			var windows []time.Time
			for _, f := range r.fired() {
				windows = append(windows, f.Window)
			}
			if len(windows) == 0 || !windows[0].Equal(tc.want) {
				t.Errorf("first run = %v, want %v", windows, tc.want)
			}
			for i := 1; i < len(windows); i++ {
				if !windows[i].After(windows[i-1]) {
					t.Errorf("windows not strictly increasing: %v", windows)
				}
			}
		})
	}
}

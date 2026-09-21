package hours

// Governing: ADR-0019, SPEC-0012 REQ "Operating Hours Key" — design.md §
// Risks "Grammar surface": a new mini-language is a new parser to fuzz.
//
// @joestump-agent 09/21/2026 - Added for #381.

import (
	"testing"
	"time"
)

// FuzzParse round-trips Parse -> String -> Parse: whatever Parse accepts,
// String must render back into something Parse accepts again, and the two
// Exprs must agree on membership everywhere a week-long sample sweep checks —
// catching a grammar edge Parse silently mishandles well before it reaches a
// real harness.toml.
func FuzzParse(f *testing.F) {
	for _, seed := range []string{
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
		"Mon 09:00-09:00",
		"TZ=Mars/Olympus 09:00-13:00",
		"",
		"   ",
		"Xyz 09:00-13:00",
		"09:00-13:00-15:00",
		"24:00-13:00",
	} {
		f.Add(seed)
	}

	f.Fuzz(func(t *testing.T, s string) {
		e1, err := Parse(s)
		if err != nil {
			return // s is not a valid expression; nothing to round-trip
		}

		s2 := e1.String()
		e2, err := Parse(s2)
		if err != nil {
			t.Fatalf("Parse(%q) succeeded, but Parse(String()) = Parse(%q) failed: %v", s, s2, err)
		}

		base := time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC) // a Monday
		for i := 0; i < 7*24; i++ {
			sample := base.Add(time.Duration(i) * time.Hour)
			in1, _, _ := e1.In(sample)
			in2, _, _ := e2.In(sample)
			if in1 != in2 {
				t.Fatalf("round trip %q -> %q diverges at %v: %v != %v", s, s2, sample, in1, in2)
			}
		}

		// String() must itself be stable under a second round trip.
		s3 := e2.String()
		if s3 != s2 {
			t.Fatalf("String() not stable: %q -> %q -> %q", s, s2, s3)
		}
	})
}

package schedfmt

// Governing: SPEC-0014 REQ "Trigger Visibility" (a triggered harness is shown
// as triggered, not disabled); SPEC-0008 REQ "Schedule Visibility".
//
// @joestump 09/24/2026 - Added for stump.wtf/harness#476.

import "testing"

// TestTriggeredHarnessPresentation: a harness only trigger sources fire is
// armed, with the trigger glyph — never "stopped", and never the clock, which
// would promise a next window it does not have.
func TestTriggeredHarnessPresentation(t *testing.T) {
	cases := []struct {
		schedule  string
		triggered bool
		firing    string
		glyph     string
		label     string
	}{
		{"", false, "", "○", "stopped"},
		{"", true, TriggeredFiring, TriggerGlyph, ArmedLabel},
		{"@every 6h", false, "@every 6h", ScheduleGlyph, ArmedLabel},
		// Both: the schedule wins, because it is the one with a next window.
		{"@every 6h", true, "@every 6h", ScheduleGlyph, ArmedLabel},
	}
	for _, tc := range cases {
		firing := Firing(tc.schedule, tc.triggered)
		if firing != tc.firing {
			t.Errorf("Firing(%q, %v) = %q, want %q", tc.schedule, tc.triggered, firing, tc.firing)
		}
		if g := Glyph("stopped", firing); g != tc.glyph {
			t.Errorf("Glyph(stopped, %q) = %q, want %q", firing, g, tc.glyph)
		}
		if l := StateLabel("stopped", firing, false, false); l != tc.label {
			t.Errorf("StateLabel(stopped, %q) = %q, want %q", firing, l, tc.label)
		}
	}
	// Running is running, triggered or not.
	if l := StateLabel("running", TriggeredFiring, false, false); l != "running" {
		t.Errorf("a running triggered harness reads %q", l)
	}
	if got := TriggersLabel([]string{"webhook.ci", "channel.sb"}); got != "webhook.ci, channel.sb" {
		t.Errorf("TriggersLabel = %q", got)
	}
}

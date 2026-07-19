package tui

import (
	"testing"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// TestStoppedMembersNonDestructive is the SPEC-0001 scenario "Non-destructive
// switch": switching to a profile starts only that profile's stopped members;
// already-running members (in or out of the profile) are never touched.
func TestStoppedMembersNonDestructive(t *testing.T) {
	harnesses := []protocol.HarnessInfo{
		{Name: "a-running", State: "running"},   // in profile B, already up → leave
		{Name: "b-stopped", State: "stopped"},   // in profile B, stopped → start
		{Name: "c-failed", State: "failed"},     // in profile B, failed → start
		{Name: "d-degraded", State: "degraded"}, // in profile B, supervisor-managed → leave
		{Name: "e-other", State: "running"},     // NOT in profile B → must never appear
	}
	profileB := protocol.ProfileInfo{
		Name:      "B",
		Harnesses: []string{"a-running", "b-stopped", "c-failed", "d-degraded"},
	}

	got := stoppedMembers(profileB, harnesses)
	want := map[string]bool{"b-stopped": true, "c-failed": true}

	if len(got) != len(want) {
		t.Fatalf("stoppedMembers = %v, want keys %v", got, want)
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("stoppedMembers included %q which should be left alone", n)
		}
		if n == "e-other" {
			t.Error("a harness outside the profile must never be started")
		}
	}
}

// TestStoppedMembersIgnoresUnknown verifies a profile referencing an unknown
// harness doesn't panic or return it.
func TestStoppedMembersIgnoresUnknown(t *testing.T) {
	got := stoppedMembers(
		protocol.ProfileInfo{Name: "P", Harnesses: []string{"ghost"}},
		[]protocol.HarnessInfo{{Name: "real", State: "stopped"}},
	)
	if len(got) != 0 {
		t.Fatalf("unknown member should yield nothing, got %v", got)
	}
}

// TestActiveProfile verifies the active-profile lookup.
func TestActiveProfile(t *testing.T) {
	ps := sampleProfiles()
	p := activeProfile(ps)
	if p == nil || p.Name != "signal-ops" {
		t.Fatalf("activeProfile = %v, want signal-ops", p)
	}
	if activeProfile(nil) != nil {
		t.Error("no profiles should yield nil active")
	}
}

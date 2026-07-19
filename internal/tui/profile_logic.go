package tui

// Governing: SPEC-0001 REQ "Profile Switcher" — selecting a profile filters the
// dashboard and offers a NON-DESTRUCTIVE start of that profile's stopped
// members; harnesses outside the profile keep running (ADR-0006). This computes
// exactly which harnesses the "start stopped" action would touch.

import "gitea.stump.rocks/stump.wtf/harness/internal/protocol"

// runningStates are the states we consider "already up" — we never restart or
// stop these when switching profiles.
var runningStates = map[string]bool{
	"running":    true,
	"starting":   true,
	"restarting": true,
	"degraded":   true, // supervisor is actively managing it; leave it be
}

// stoppedMembers returns the members of profile that are currently stopped/
// failed and would be started by the non-destructive "start stopped" action. It
// never returns a harness outside the profile, and never one already running —
// so accepting the switch cannot disturb harnesses from the previous profile
// (SPEC-0001 scenario "Non-destructive switch").
func stoppedMembers(profile protocol.ProfileInfo, harnesses []protocol.HarnessInfo) []string {
	byName := make(map[string]protocol.HarnessInfo, len(harnesses))
	for _, h := range harnesses {
		byName[h.Name] = h
	}
	var out []string
	for _, name := range profile.Harnesses {
		h, ok := byName[name]
		if !ok {
			continue
		}
		if !runningStates[h.State] {
			out = append(out, name)
		}
	}
	return out
}

// findProfile returns a pointer to the named profile, or nil.
func findProfile(profiles []protocol.ProfileInfo, name string) *protocol.ProfileInfo {
	for i := range profiles {
		if profiles[i].Name == name {
			return &profiles[i]
		}
	}
	return nil
}

// activeProfile returns the profile flagged Active, or nil if none is active
// (the "show all" default state).
func activeProfile(profiles []protocol.ProfileInfo) *protocol.ProfileInfo {
	for i := range profiles {
		if profiles[i].Active {
			return &profiles[i]
		}
	}
	return nil
}

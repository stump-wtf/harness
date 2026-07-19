package tui

// Governing: SPEC-0001 REQ "Dashboard" (profile-filtered list carrying glyph/
// name/state/↻/uptime) and REQ "Harness Hop" (`[`/`]` prev/next with wrap) and
// REQ "Zero And Error States" (degraded rows expand with last-exit + backoff
// countdown). These are the pure decision functions the Dashboard and Attached
// models call; keeping them separate makes every scenario table-testable.

import (
	"fmt"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// filterByProfile returns the harnesses visible under the active profile. When
// showAll is true, or there is no active profile, every harness is returned in
// list order (SPEC-0001 REQ "Dashboard": "filtered to the active profile with a
// toggle to show all"). Membership preserves the daemon's list ordering, not the
// profile's, so the dashboard order is stable across profile switches.
func filterByProfile(harnesses []protocol.HarnessInfo, profile *protocol.ProfileInfo, showAll bool) []protocol.HarnessInfo {
	if showAll || profile == nil {
		out := make([]protocol.HarnessInfo, len(harnesses))
		copy(out, harnesses)
		return out
	}
	member := make(map[string]bool, len(profile.Harnesses))
	for _, n := range profile.Harnesses {
		member[n] = true
	}
	var out []protocol.HarnessInfo
	for _, h := range harnesses {
		if member[h.Name] {
			out = append(out, h)
		}
	}
	return out
}

// hopIndex returns the index to hop to from cur by delta over n items, wrapping
// around both ends (SPEC-0001 REQ "Harness Hop"). n==0 returns 0.
func hopIndex(cur, n, delta int) int {
	if n <= 0 {
		return 0
	}
	return ((cur+delta)%n + n) % n
}

// restartMarker renders the `↻<count>` restart column, blank when zero so a
// healthy row stays quiet (design: healthy harnesses don't shout).
func restartMarker(count int) string {
	if count <= 0 {
		return ""
	}
	return fmt.Sprintf("↻%d", count)
}

// nextActionText renders the right-hand "uptime / next-action" column. A
// harness waiting to retry shows its backoff countdown; a failed one shows its
// exit code; otherwise it's blank (uptime is filled by the caller which knows
// start time). This is the shared bit the degraded-row expansion also uses.
func nextActionText(h protocol.HarnessInfo) string {
	if h.NextRetryInMs > 0 {
		return "retry in " + humanizeMs(h.NextRetryInMs)
	}
	return ""
}

// flappingDetail is the expanded second line for a degraded/flapping row
// (SPEC-0001 REQ "Zero And Error States": the ◐ row expands to show last exit
// code + backoff countdown, one keystroke to logs).
func flappingDetail(h protocol.HarnessInfo) string {
	detail := fmt.Sprintf("last exit %d", h.LastExitCode)
	if h.RestartCount > 0 {
		detail += fmt.Sprintf(" · %d restarts", h.RestartCount)
	}
	if h.NextRetryInMs > 0 {
		detail += " · retry in " + humanizeMs(h.NextRetryInMs)
	}
	detail += " · l: logs"
	return detail
}

// isDegraded reports whether a row should render its expanded flapping detail.
func isDegraded(h protocol.HarnessInfo) bool {
	return h.Flapping || h.State == "degraded" || h.State == "failed"
}

// humanizeMs renders a millisecond backoff as a compact human duration.
func humanizeMs(ms int64) string {
	d := time.Duration(ms) * time.Millisecond
	switch {
	case d < time.Second:
		return fmt.Sprintf("%dms", ms)
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Round(time.Second)/time.Second))
	default:
		return fmt.Sprintf("%dm%ds", int(d/time.Minute), int((d%time.Minute)/time.Second))
	}
}

// selectByName returns the index of the harness named n in list, or -1. Used to
// keep the selection pinned to the same harness across a refresh/filter change.
func selectByName(list []protocol.HarnessInfo, n string) int {
	for i, h := range list {
		if h.Name == n {
			return i
		}
	}
	return -1
}

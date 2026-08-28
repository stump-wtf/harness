package tui

// Governing: SPEC-0001 REQ "Dashboard" (profile-filtered list carrying glyph/
// name/state/↻/uptime) and REQ "Harness Hop" (`[`/`]` prev/next with wrap) and
// REQ "Zero And Error States" (degraded rows expand with last-exit + backoff
// countdown). These are the pure decision functions the Dashboard and Attached
// models call; keeping them separate makes every scenario table-testable.

import (
	"fmt"
	"strings"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
	"gitea.stump.rocks/stump.wtf/harness/internal/schedfmt"
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
// harness waiting to retry shows its backoff countdown; a scheduled one shows
// how long until its cron next fires; otherwise it's blank (uptime is filled
// by the caller which knows start time). This is the shared bit the
// degraded-row expansion also uses.
//
// Backoff wins over the schedule when both are live: a harness bouncing right
// now is the more urgent fact, and the cadence is still on the meta line
// beneath the row.
//
// The countdown is what makes "stopped (scheduled)" mean something. Without
// it a cron job that fires in ten minutes and one whose daemon never armed it
// render identically — the dashboard asserted a harness was scheduled but
// could not say when, which is the question an operator actually has (#160).
func nextActionText(h protocol.HarnessInfo) string {
	if h.NextRetryInMs > 0 {
		return "retry in " + humanizeMs(h.NextRetryInMs)
	}
	return nextRunText(h.NextRun)
}

// nextRunText phrases the daemon's resolved next-fire stamp for the row's
// right-hand column: "next in 2h", or "due now" once the firing time has
// passed and the run has not been observed yet.
//
// "next " prefixes the shared countdown because the column's other tenant is
// "retry in 45s" — bare "in 2h" next to a stopped harness reads as a retry,
// which is the opposite of a healthy cron job. Empty input (no schedule, or a
// daemon that has not resolved a time) renders nothing at all.
func nextRunText(nextRun string) string {
	switch s := schedfmt.NextIn(nextRun); s {
	case "":
		return ""
	case "due":
		return "due now"
	default:
		return "next " + s
	}
}

// harnessMeta returns the per-row metadata sub-line as its two separable
// halves: the elidable `what` (the configured cmd, or the prompt for an agent
// one-shot) and the fixed-width `rest` facts that follow it.
//
// This is the block that used to hang off the bottom of the peek pane as a
// key/value dump (#199). It lives under the harness now because that is what
// it describes — the preview pane's job is the guest's screen, not a debug
// readout — and it is split rather than pre-joined so metaLine can shrink the
// one field with unbounded length instead of truncating the whole line and
// losing the pid off the right edge.
//
// A degraded row folds its expansion in here too (SPEC-0001 REQ "Zero And
// Error States") rather than claiming a third line, and the caller paints the
// whole line in the degraded color — a louder signal than the separate line it
// replaces.
//
// Two of the fields the old peek summary and the old flapping detail carried
// are deliberately absent, because renderRow already puts them one line above:
// the restart count (its `↻N` marker) and the backoff countdown
// (nextActionText). Repeating either is noise, not completeness, and the
// columns are better spent on the cmd that has nowhere else to go.
func harnessMeta(h protocol.HarnessInfo) (what string, rest []string) {
	// A prompt harness carries no configured cmd — surface the prompt, what
	// the user actually wrote (ADR-0011 spawn-time synthesis).
	what = orDefault(h.Adapter, "crush")
	switch {
	case h.Prompt != "":
		what = flattenSpace(h.Prompt)
	case h.PromptFile != "":
		// The path, not the file's contents — the daemon never sends them
		// (ADR-0018), and a whole specification would not fit this column.
		what = flattenSpace(h.PromptFile)
	}
	if h.Model != "" {
		rest = append(rest, "model "+h.Model)
	}
	if h.AutoAccept {
		rest = append(rest, "yolo")
	}
	// The cadence, not the countdown: nextActionText already put "next in 2h"
	// on the row above, and repeating it here would spend columns to say the
	// same thing twice. This is the half that answers "how often" — and it
	// falls back to the raw cron, because a schedule too irregular to
	// paraphrase is exactly the one worth reading verbatim (#160).
	if h.Schedule != "" {
		rest = append(rest, schedfmt.LabelOrRaw(h.Schedule))
	}
	rest = append(rest,
		orDefault(h.Backend, "native"),
		fmt.Sprintf("exit %d", h.LastExitCode),
	)
	if h.PID > 0 {
		rest = append(rest, fmt.Sprintf("pid %d", h.PID))
	}
	if isDegraded(h) {
		rest = append(rest, "l: logs")
	}
	return what, rest
}

// flattenSpace collapses every run of whitespace — newlines included — into a
// single space.
//
// A prompt is routinely written as a YAML block scalar, so it arrives with
// embedded newlines. The metadata sub-line is ONE line by construction: every
// height budget in the list pane counts it as one (viewList's contentBudget,
// scrollListToSel's listRowLines), and lipgloss pads a pane up but never
// truncates it down, so a `what` carrying a newline renders extra physical
// rows that nothing counted and pushes the dashboard past m.h — the alt-screen
// scroll of #179. Width clamps cannot catch it either: lipgloss.Width of a
// multi-line string is its widest line, so a tall-but-narrow prompt measures as
// fitting.
func flattenSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
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

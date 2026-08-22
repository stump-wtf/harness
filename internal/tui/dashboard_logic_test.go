package tui

import (
	"strings"
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// TestFilterByProfile verifies the dashboard list is filtered to the active
// profile, and the show-all toggle reveals everything (SPEC-0001 REQ
// "Dashboard").
func TestFilterByProfile(t *testing.T) {
	hs := sampleHarnesses()
	prof := &protocol.ProfileInfo{Name: "signal-ops", Harnesses: []string{"crush-signal", "claude-src"}}

	filtered := filterByProfile(hs, prof, false)
	if len(filtered) != 2 {
		t.Fatalf("filtered len = %d, want 2", len(filtered))
	}
	if filtered[0].Name != "crush-signal" || filtered[1].Name != "claude-src" {
		t.Fatalf("filtered preserved daemon order wrong: %v", names(filtered))
	}

	all := filterByProfile(hs, prof, true)
	if len(all) != len(hs) {
		t.Fatalf("show-all len = %d, want %d", len(all), len(hs))
	}

	none := filterByProfile(hs, nil, false)
	if len(none) != len(hs) {
		t.Fatalf("nil profile should show all, got %d", len(none))
	}
}

// TestHopIndexWraps verifies `[`/`]` wrap-around over the visible list
// (SPEC-0001 REQ "Harness Hop").
func TestHopIndexWraps(t *testing.T) {
	cases := []struct{ cur, n, delta, want int }{
		{0, 5, 1, 1},
		{4, 5, 1, 0},  // wrap forward past the end
		{0, 5, -1, 4}, // wrap backward past the start
		{2, 5, -1, 1},
		{0, 0, 1, 0}, // empty
		{0, 1, 1, 0}, // single item stays
	}
	for _, c := range cases {
		if got := hopIndex(c.cur, c.n, c.delta); got != c.want {
			t.Errorf("hopIndex(%d,%d,%d) = %d, want %d", c.cur, c.n, c.delta, got, c.want)
		}
	}
}

// TestHarnessMetaDegraded verifies the degraded-row expansion survived #199
// folding it into the metadata sub-line (SPEC-0001 REQ "Zero And Error
// States"). Every fact the old two-line expansion carried is still on screen —
// the exit code and logs hint in the sub-line, the restart count and backoff
// countdown one line above, where renderRow already had them.
func TestHarnessMetaDegraded(t *testing.T) {
	h := protocol.HarnessInfo{State: "degraded", LastExitCode: 137, RestartCount: 3, NextRetryInMs: 8000, Flapping: true}
	what, rest := harnessMeta(h)
	got := metaLine(what, rest, 0)
	for _, want := range []string{"exit 137", "logs"} {
		if !strings.Contains(got, want) {
			t.Errorf("metaLine = %q, missing %q", got, want)
		}
	}
	// Not duplicated from the main row.
	for _, banned := range []string{"retry in", "3 restart"} {
		if strings.Contains(got, banned) {
			t.Errorf("metaLine = %q repeats %q, already on the row above", got, banned)
		}
	}
	if want := "retry in 8s"; nextActionText(h) != want {
		t.Errorf("nextActionText = %q, want %q — the countdown must stay on the main row", nextActionText(h), want)
	}
	if restartMarker(h.RestartCount) != "↻3" {
		t.Errorf("restartMarker = %q, want ↻3 on the main row", restartMarker(h.RestartCount))
	}
	if !isDegraded(h) {
		t.Error("flapping harness should be degraded")
	}
	if isDegraded(protocol.HarnessInfo{State: "running"}) {
		t.Error("running harness should not be degraded")
	}
}

// TestHarnessMetaCarriesThePeekSummary pins the fields that moved out of the
// peek pane (#199): a healthy row must still be able to answer cmd, model,
// yolo, backend, exit and pid without opening `describe`.
func TestHarnessMetaCarriesThePeekSummary(t *testing.T) {
	h := protocol.HarnessInfo{
		Name: "agent", State: "running", Adapter: "claude-code",
		Backend: "tmux", LastExitCode: 143, PID: 4242,
	}
	what, rest := harnessMeta(h)
	got := metaLine(what, rest, 0)
	for _, want := range []string{"claude-code", "tmux", "exit 143", "pid 4242"} {
		if !strings.Contains(got, want) {
			t.Errorf("metaLine = %q, missing %q", got, want)
		}
	}

	// A prompt harness shows the prompt where a cmd harness shows its cmd
	// (ADR-0011), plus the model and yolo flags folded into the argv at spawn.
	p := protocol.HarnessInfo{
		Name: "one-shot", State: "running", Prompt: "check the deployments",
		Model: "claude-opus-5", AutoAccept: true,
	}
	what, rest = harnessMeta(p)
	got = metaLine(what, rest, 0)
	for _, want := range []string{"check the deployments", "model claude-opus-5", "yolo", "native"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt metaLine = %q, missing %q", got, want)
		}
	}

	// The restart count is deliberately absent — renderRow's ↻N carries it.
	what, rest = harnessMeta(protocol.HarnessInfo{RestartCount: 9})
	if got := metaLine(what, rest, 0); strings.Contains(got, "9 restart") {
		t.Errorf("metaLine = %q duplicates the restart count already on the main row", got)
	}
}

// TestScheduledHarnessIsLegibleInTheList is the dashboard half of #160: a
// scheduled harness has to answer "is it armed, and when does it fire" from
// the list alone.
//
// Before this, the row said "stopped (scheduled)" and stopped there — a cron
// job firing in two hours and one whose daemon never armed it rendered
// identically, and the only way to tell them apart was `describe`.
func TestScheduledHarnessIsLegibleInTheList(t *testing.T) {
	h := protocol.HarnessInfo{
		Name: "stumpcloud-sweep", State: "stopped", Adapter: "claude-code",
		Schedule: "30 9 * * *",
		NextRun:  time.Now().Add(2 * time.Hour).Format(time.RFC3339),
	}
	if got, want := nextActionText(h), "next in 2h"; got != want {
		t.Errorf("nextActionText = %q, want %q", got, want)
	}
	what, rest := harnessMeta(h)
	if got := metaLine(what, rest, 0); !strings.Contains(got, "daily 09:30") {
		t.Errorf("metaLine = %q, missing the cadence", got)
	}
	// The cadence and the countdown split across the two lines rather than
	// both landing in the sub-line.
	what, rest = harnessMeta(h)
	if got := metaLine(what, rest, 0); strings.Contains(got, "next in") {
		t.Errorf("metaLine = %q repeats the countdown already on the row above", got)
	}
}

// TestScheduledHarnessMetaFallsBackToRawCron: an expression schedfmt cannot
// paraphrase must still render its cron verbatim. The sub-line is the only
// place the TUI carries the cadence, so a blank there means the dashboard
// claims a harness is scheduled and then refuses to say how often.
func TestScheduledHarnessMetaFallsBackToRawCron(t *testing.T) {
	// "0 */6 * * *" used to live here, but it now paraphrases as "every 6h"
	// (an even step IS an interval). Exercise the fallback with a step that
	// genuinely cannot be stated as one: */7 fires at 0,7,14,21 and then wraps
	// only 3h to midnight, so schedfmt declines to paraphrase it.
	h := protocol.HarnessInfo{Name: "sweep", State: "stopped", Schedule: "0 */7 * * *"}
	what, rest := harnessMeta(h)
	if got := metaLine(what, rest, 0); !strings.Contains(got, "0 */7 * * *") {
		t.Errorf("metaLine = %q, missing the raw cron fallback", got)
	}
}

// TestBackoffOutranksTheSchedule: a scheduled one-shot that is bouncing right
// now shows the retry, not the next cron fire. Both are "what happens next",
// and the imminent one is the one the operator needs.
func TestBackoffOutranksTheSchedule(t *testing.T) {
	h := protocol.HarnessInfo{
		State: "degraded", NextRetryInMs: 8000,
		Schedule: "@daily", NextRun: time.Now().Add(3 * time.Hour).Format(time.RFC3339),
	}
	if got, want := nextActionText(h), "retry in 8s"; got != want {
		t.Errorf("nextActionText = %q, want %q", got, want)
	}
}

// TestRestartMarker verifies the restart column stays quiet at zero.
func TestRestartMarker(t *testing.T) {
	if restartMarker(0) != "" {
		t.Error("zero restarts should render blank")
	}
	if restartMarker(2) != "↻2" {
		t.Errorf("restartMarker(2) = %q, want ↻2", restartMarker(2))
	}
}

// TestHumanizeMs spot-checks the backoff formatting.
func TestHumanizeMs(t *testing.T) {
	cases := map[int64]string{
		500:   "500ms",
		8000:  "8s",
		65000: "1m5s",
	}
	for ms, want := range cases {
		if got := humanizeMs(ms); got != want {
			t.Errorf("humanizeMs(%d) = %q, want %q", ms, got, want)
		}
	}
}

func names(hs []protocol.HarnessInfo) []string {
	var out []string
	for _, h := range hs {
		out = append(out, h.Name)
	}
	return out
}

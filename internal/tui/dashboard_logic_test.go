package tui

import (
	"strings"
	"testing"

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

// TestFlappingDetail verifies the degraded-row expansion carries the last exit
// code, restart count, and backoff countdown with a one-key logs hint
// (SPEC-0001 REQ "Zero And Error States").
func TestFlappingDetail(t *testing.T) {
	h := protocol.HarnessInfo{State: "degraded", LastExitCode: 137, RestartCount: 3, NextRetryInMs: 8000, Flapping: true}
	got := flappingDetail(h)
	for _, want := range []string{"last exit 137", "3 restarts", "retry in 8s", "logs"} {
		if !strings.Contains(got, want) {
			t.Errorf("flappingDetail = %q, missing %q", got, want)
		}
	}
	if !isDegraded(h) {
		t.Error("flapping harness should be degraded")
	}
	if isDegraded(protocol.HarnessInfo{State: "running"}) {
		t.Error("running harness should not be degraded")
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

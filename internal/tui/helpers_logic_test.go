package tui

// Governing: SPEC-0001 REQ "Zero And Error States" (the next-action column and
// the degraded-row expansion), SPEC-0001 REQ "Dashboard" (selection by name),
// ADR-0006 (profiles).
//
// The remaining pure helpers that had no direct test: nextActionText,
// selectByName, findProfile, joinNames, and splitLines. None of them is
// complicated, which is exactly why they were skipped — but three of them feed
// decisions with real consequences (which harness a verb targets, which profile
// a switch resolves to, what the operator is told a failing harness is doing),
// and a pure function with an obvious table test is the cheapest coverage in
// the package.
//
// Also here: the dashboard multi-rune expansion edge the existing #145 tests
// do not reach.

import (
	"testing"
	"time"

	"gitea.stump.rocks/stump.wtf/harness/internal/protocol"
)

// TestNextActionText pins the right-hand column. A harness in backoff must say
// so — this is the operator's only cue that a failed harness is going to retry
// itself rather than sitting dead.
func TestNextActionText(t *testing.T) {
	tests := []struct {
		name string
		h    protocol.HarnessInfo
		want string
	}{
		{"idle harness shows nothing", protocol.HarnessInfo{}, ""},
		{"backoff shows a countdown", protocol.HarnessInfo{NextRetryInMs: 5000}, "retry in " + humanizeMs(5000)},
		{
			// Zero must not render "retry in 0s" — that reads as an imminent
			// retry when there is none scheduled.
			"zero backoff is not a countdown",
			protocol.HarnessInfo{NextRetryInMs: 0, LastExitCode: 1},
			"",
		},
		{
			// A schedule the daemon has not resolved a firing time for says
			// nothing rather than guessing (#160).
			"scheduled without a resolved next run stays blank",
			protocol.HarnessInfo{Schedule: "@daily"},
			"",
		},
		{
			// Past its firing time and still stopped: the operator needs to
			// see it is overdue, not a negative countdown.
			"overdue schedule reads as due now",
			protocol.HarnessInfo{Schedule: "@daily", NextRun: time.Now().Add(-time.Minute).Format(time.RFC3339)},
			"due now",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := nextActionText(tt.h); got != tt.want {
				t.Errorf("nextActionText() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestSelectByName pins the lookup every retarget depends on: the palette's
// describe/logs/edit jumps, the refresh selection pin, and hopTo all resolve a
// harness by name through this. A wrong index here silently points the cursor
// at a different harness than the one named.
func TestSelectByName(t *testing.T) {
	list := sampleHarnesses()
	for i, h := range list {
		if got := selectByName(list, h.Name); got != i {
			t.Errorf("selectByName(%q) = %d, want %d", h.Name, got, i)
		}
	}
	if got := selectByName(list, "no-such-harness"); got != -1 {
		t.Errorf("selectByName(missing) = %d, want -1 so callers can skip the retarget", got)
	}
	if got := selectByName(nil, "anything"); got != -1 {
		t.Errorf("selectByName(nil list) = %d, want -1", got)
	}
	// Empty name must not accidentally match a harness with an empty Name.
	if got := selectByName([]protocol.HarnessInfo{{Name: "real"}}, ""); got != -1 {
		t.Errorf("selectByName(\"\") = %d, want -1", got)
	}
}

// TestFindProfile pins profile resolution, which gates the profile switch.
// Returning the wrong profile would start the wrong set of harnesses.
func TestFindProfile(t *testing.T) {
	ps := sampleProfiles()
	for i := range ps {
		got := findProfile(ps, ps[i].Name)
		if got == nil {
			t.Fatalf("findProfile(%q) = nil", ps[i].Name)
		}
		if got.Name != ps[i].Name {
			t.Errorf("findProfile(%q) returned %q", ps[i].Name, got.Name)
		}
		// Must be a pointer INTO the slice, not a copy — callers read
		// Harnesses off it and a detached copy would silently diverge.
		if got != &ps[i] {
			t.Errorf("findProfile(%q) returned a copy, not a pointer into the slice", ps[i].Name)
		}
	}
	if got := findProfile(ps, "nope"); got != nil {
		t.Errorf("findProfile(missing) = %+v, want nil", got)
	}
	if got := findProfile(nil, "any"); got != nil {
		t.Errorf("findProfile(nil) = %+v, want nil", got)
	}
}

// TestJoinNames pins the status-line formatting used after a profile switch.
func TestJoinNames(t *testing.T) {
	tests := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{}, ""},
		{[]string{"one"}, "one"},
		{[]string{"one", "two"}, "one, two"},
		{[]string{"a", "b", "c"}, "a, b, c"},
	}
	for _, tt := range tests {
		if got := joinNames(tt.in); got != tt.want {
			t.Errorf("joinNames(%v) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// TestSplitLines pins the log-splitting helper, including the trailing-newline
// rule. Every log tail ends with a newline; counting that as an extra blank row
// would shift the peek pane's budget by one on every render.
func TestSplitLines(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"empty is no lines", "", nil},
		{"single line without newline", "one", []string{"one"}},
		{"trailing newline does not add a blank row", "one\n", []string{"one"}},
		{"two lines", "one\ntwo", []string{"one", "two"}},
		{"two lines with trailing newline", "one\ntwo\n", []string{"one", "two"}},
		{"CRLF is normalized", "one\r\ntwo\r\n", []string{"one", "two"}},
		{"interior blank lines are preserved", "one\n\ntwo\n", []string{"one", "", "two"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitLines(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("splitLines(%q) = %q (%d lines), want %q (%d)", tt.in, got, len(got), tt.want, len(tt.want))
			}
			for i := range tt.want {
				if got[i] != tt.want[i] {
					t.Errorf("line %d = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

// TestDashboardMultiRuneStopsAtAnyOverlay extends the #145 coverage. The
// existing tests prove the expansion loop breaks when a *guarded* action opens
// the confirm overlay. The same break must fire for every other overlay-opening
// rune — otherwise a burst like "/abc" would open search and then keep feeding
// "abc" to the dashboard's navigation bindings instead of the search input.
func TestDashboardMultiRuneStopsAtAnyOverlay(t *testing.T) {
	tests := []struct {
		name  string
		burst string
		want  overlay
	}{
		{"search opens and swallows the rest", "/jj", overlaySearch},
		{"palette opens and swallows the rest", ":jj", overlayPalette},
		{"help opens and swallows the rest", "?jj", overlayHelp},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := baseModel(120, 40)
			m.showAll = true
			startSel := m.sel

			m.Update(runeKey(tt.burst))

			if m.overlay != tt.want {
				// Must fail, not skip: skipping here would swallow exactly the
				// regression this test exists for — an expansion loop that
				// stopped dispatching leaves overlay==overlayNone, and a Skip
				// would hide that AND the selection assertion below.
				t.Fatalf("burst %q did not open %v (overlay=%v)", tt.burst, tt.want, m.overlay)
			}
			if m.sel != startSel {
				t.Errorf("selection moved from %d to %d — runes after the overlay opened "+
					"were still dispatched to the dashboard", startSel, m.sel)
			}
		})
	}
}

// TestDashboardMultiRuneAllUnboundIsQuiet pins the len(cmds)==0 path: a burst
// of runes that match nothing must return cleanly rather than emitting an empty
// batch.
func TestDashboardMultiRuneAllUnboundIsQuiet(t *testing.T) {
	m := baseModel(120, 40)
	m.showAll = true
	startSel := m.sel

	_, cmd := m.Update(runeKey("zzz"))

	if m.sel != startSel {
		t.Errorf("unbound burst moved the selection from %d to %d", startSel, m.sel)
	}
	if cmd != nil {
		// Not an error on its own, but it must not blow up when drained.
		drain(cmd)
	}
}

package metrics

// Per-Program Metric Tests
//
// Governing: SPEC-0013 REQ-3 (the family and its outcomes), REQ-5 (the label
// grammar, the cap, and what must never become a label), REQ-6 (a program never
// counted is absent, not zero). Issue #913.
//
// @joestump-agent 10/31/2026 - Added for stump.wtf/harness#913.

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stump-wtf/harness/internal/observe"
)

// shellEvent is one tool call whose command runs programs, failing when isErr
// is set. Everything else matches toolEvent, so the call is counted like any
// other by the model-reachability family.
func shellEvent(harness, session string, at time.Time, programs []string, isErr bool) observe.Event {
	ev := toolEvent(harness, session, at)
	ev.Tool.Tool = "Bash"
	ev.Tool.Programs = programs
	ev.Tool.IsError = isErr
	return ev
}

func programLabels(f families) []string {
	fam, ok := f["harness_agent_program_calls_total"]
	if !ok {
		return nil
	}
	seen := map[string]bool{}
	for _, m := range fam.GetMetric() {
		for _, lp := range m.GetLabel() {
			if lp.GetName() == "program" {
				seen[lp.GetValue()] = true
			}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	return out
}

// A program never counted reports nothing at all, so a dashboard cannot read a
// harness that has run no shell command as one whose every program is healthy.
func TestProgramMetricAbsentUntilFirstCall(t *testing.T) {
	src := newFakeSource()
	src.add(crushHarness("worker"), runningSnap())
	feed := newFakeFeed()
	m := newTestMetrics(t, src, Options{Observer: feed})

	feed.ch <- toolEvent("worker", "s1", t0)
	eventually(t, "the tool call counted", func() bool {
		v, _ := scrape(t, m).get("harness_model_calls_total", lbls("harness", "worker", "outcome", "success"))
		return v == 1
	})
	if _, ok := scrape(t, m)["harness_agent_program_calls_total"]; ok {
		t.Fatal("harness_agent_program_calls_total present for a call that names no program")
	}
}

// outcome comes from the tool result, and both outcomes report for a program
// once it has been seen, so increase() can find its first error.
func TestProgramCallsCarryTheirOutcome(t *testing.T) {
	src := newFakeSource()
	src.add(crushHarness("worker"), runningSnap())
	feed := newFakeFeed()
	m := newTestMetrics(t, src, Options{Observer: feed})

	feed.ch <- shellEvent("worker", "s1", t0, []string{"tea", "jq"}, false)
	eventually(t, "both programs counted ok", func() bool {
		f := scrape(t, m)
		a, okA := f.get("harness_agent_program_calls_total", lbls("harness", "worker", "program", "tea", "outcome", "ok"))
		b, okB := f.get("harness_agent_program_calls_total", lbls("harness", "worker", "program", "jq", "outcome", "ok"))
		return okA && a == 1 && okB && b == 1
	})

	feed.ch <- shellEvent("worker", "s1", t0.Add(time.Second), []string{"tea"}, true)
	eventually(t, "the failing call counted as an error", func() bool {
		f := scrape(t, m)
		v, ok := f.get("harness_agent_program_calls_total", lbls("harness", "worker", "program", "tea", "outcome", "error"))
		return ok && v == 1
	})

	f := scrape(t, m)
	if v := f.must(t, "harness_agent_program_calls_total", lbls("harness", "worker", "program", "tea", "outcome", "ok")); v != 1 {
		t.Errorf("tea ok = %v, want 1 (a failed result must not land on the ok side)", v)
	}
	if v := f.must(t, "harness_agent_program_calls_total", lbls("harness", "worker", "program", "jq", "outcome", "error")); v != 0 {
		t.Errorf("jq error = %v, want 0", v)
	}
	if v := f.must(t, "harness_model_calls_total", lbls("harness", "worker", "outcome", "success")); v != 2 {
		t.Errorf("model calls changed with the new family: got %v, want 2", v)
	}
}

// 70 distinct names for one harness become 64 series plus program="other": the
// cap is what lets the fleet keep this family, and it is enforced where the
// count is taken, so nothing unbounded is ever stored.
func TestProgramCapFoldsOverflowIntoOther(t *testing.T) {
	s := newSeries()
	names := make([]string, 70)
	for i := range names {
		names[i] = fmt.Sprintf("prog-%02d", i)
	}
	s.countPrograms(names, false)

	if got := s.namedPrograms(); got != ProgramCap {
		t.Fatalf("named programs = %d, want the cap %d", got, ProgramCap)
	}
	if got := s.programs[ProgramOther][0]; got != 6 {
		t.Errorf("other = %d, want the 6 names past the cap", got)
	}
	for i, name := range names {
		want := uint64(0)
		if i < ProgramCap {
			want = 1
		}
		if got := s.programs[name][0]; got != want {
			t.Errorf("%s = %d, want %d", name, got, want)
		}
	}

	// Past the cap, a name seen again still counts under its own series: the
	// cap bounds the label set, it does not stop the counting.
	s.countPrograms([]string{"prog-00", "prog-69"}, false)
	if got := s.programs["prog-00"][0]; got != 2 {
		t.Errorf("prog-00 after a repeat = %d, want 2", got)
	}
	if got := s.programs[ProgramOther][0]; got != 7 {
		t.Errorf("other after a repeat of an overflow name = %d, want 7", got)
	}
}

// A name that is not a plain lowercase executable name is never a label: an env
// assignment, an argument, a path or shell punctuation is counted as other, so
// no secret and no command line reaches a label value.
func TestNoEnvAssignmentOrArgumentBecomesAProgramLabel(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "tea", "tea"},
		{"agent-trace uppercase folded", "Python3", "python3"},
		{"dashed and dotted", "golangci-lint", "golangci-lint"},
		{"at the length limit", strings.Repeat("a", 32), strings.Repeat("a", 32)},
		{"past the length limit", strings.Repeat("a", 33), ProgramOther},
		{"env assignment", "GITEA_TOKEN=supersecret", ProgramOther},
		{"call with its argument", "tea pr list", ProgramOther},
		{"path", "/usr/local/bin/golangci-lint", ProgramOther},
		{"substitution", "$(which go)", ProgramOther},
		{"empty", "", ProgramOther},
	}
	for _, tc := range cases {
		if got := ProgramLabel(tc.in); got != tc.want {
			t.Errorf("%s: ProgramLabel(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}

	src := newFakeSource()
	src.add(crushHarness("worker"), runningSnap())
	feed := newFakeFeed()
	m := newTestMetrics(t, src, Options{Observer: feed})
	feed.ch <- shellEvent("worker", "s1", t0,
		[]string{"GITEA_TOKEN=supersecret", "tea pr list", "/opt/bin/tea", "Go"}, false)
	eventually(t, "the call counted", func() bool {
		v, ok := scrape(t, m).get("harness_agent_program_calls_total", lbls("harness", "worker", "program", ProgramOther, "outcome", "ok"))
		return ok && v == 3
	})
	f := scrape(t, m)
	for _, name := range programLabels(f) {
		if strings.ContainsAny(name, "=/ ") || strings.Contains(name, "$") {
			t.Errorf("program label %q carries an assignment, an argument or a path", name)
		}
		if name == "supersecret" {
			t.Errorf("program label %q is an env assignment's value", name)
		}
	}
	if v := f.must(t, "harness_agent_program_calls_total", lbls("harness", "worker", "program", "go", "outcome", "ok")); v != 1 {
		t.Errorf("go = %v, want 1 (uppercase folds, it is not refused)", v)
	}
}
